//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	fcassets "github.com/jingkaihe/matchlock/internal/assets/firecracker"
	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/firecracker"
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Setup matchlock environment",
}

var setupLinuxCmd = &cobra.Command{
	Use:   "linux",
	Short: "Setup matchlock for Linux host and current user",
	Long: `Setup matchlock for Linux by:
  1. Installing or locating Firecracker
  2. Configuring the host for Matchlock
  3. Adding the current user to kvm/netdev groups

Firecracker is installed from the version vendored into this binary, so
this step works without network access.

This command requires root privileges.`,
	RunE: runSetupLinux,
}

var setupUserCmd = &cobra.Command{
	Use:   "user <name>",
	Short: "Enroll a user for Matchlock access on Linux",
	Long: `Enroll a specific user for Matchlock access by:
  1. Adding the user to the kvm group
  2. Adding the user to the netdev group

This command requires root privileges. Users must log out and back in for group
changes to take effect.`,
	Args: cobra.ExactArgs(1),
	RunE: runSetupUser,
}

func init() {
	setupLinuxCmd.Flags().String("user", "", "Username to configure (default: current user or SUDO_USER)")
	setupLinuxCmd.Flags().String("binary", "", "Path to matchlock binary (default: auto-detect)")
	setupLinuxCmd.Flags().String("install-dir", "/usr/libexec/matchlock", "Directory to install Firecracker and jailer")
	setupLinuxCmd.Flags().Bool("best-effort", false, "Continue setup after non-fatal errors")
	setupLinuxCmd.Flags().Bool("skip-firecracker", false, "Skip Firecracker installation")
	setupLinuxCmd.Flags().Bool("skip-permissions", false, "Skip machine permission setup")
	setupLinuxCmd.Flags().Bool("skip-network", false, "Skip network configuration")

	setupCmd.AddCommand(setupUserCmd)
	setupCmd.AddCommand(setupLinuxCmd)
	rootCmd.AddCommand(setupCmd)
}

func runSetupLinux(cmd *cobra.Command, args []string) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("this command requires root privileges. Run with: sudo matchlock setup linux")
	}

	userName, err := resolveSetupUser(cmd)
	if err != nil {
		return err
	}

	fmt.Printf("Setting up matchlock host and user: %s\n\n", userName)

	if err := runSetupLinuxHost(cmd); err != nil {
		return err
	}

	fmt.Println()
	if err := setupUserGroups(userName); err != nil {
		return errx.Wrap(ErrSetupLinux, err)
	}

	fmt.Println()
	fmt.Println("Setup complete!")
	fmt.Printf("User %s must log out and back in for group changes to take effect.\n", userName)
	return nil
}

func runSetupLinuxHost(cmd *cobra.Command) error {
	skipFirecracker, _ := cmd.Flags().GetBool("skip-firecracker")
	bestEffort, _ := cmd.Flags().GetBool("best-effort")
	skipPermissions, _ := cmd.Flags().GetBool("skip-permissions")
	skipNetwork, _ := cmd.Flags().GetBool("skip-network")
	installDir, _ := cmd.Flags().GetString("install-dir")
	binaryPath, _ := cmd.Flags().GetString("binary")

	if binaryPath == "" {
		if exe, err := os.Executable(); err == nil {
			binaryPath = exe
		} else {
			binaryPath = "./bin/matchlock"
		}
	}

	fmt.Println("Setting up matchlock host requirements")
	fmt.Println()

	if !skipFirecracker {
		if err := runSetupStep("firecracker installation", bestEffort, func() error {
			return installFirecracker(installDir)
		}); err != nil {
			return errx.Wrap(ErrSetupLinux, err)
		}
		fmt.Println()
	}

	if !skipPermissions {
		if err := runSetupStep("permission setup", bestEffort, func() error {
			return setupHostPermissions(binaryPath, bestEffort)
		}); err != nil {
			return errx.Wrap(ErrSetupLinux, err)
		}
		fmt.Println()
	}

	if !skipNetwork {
		if err := runSetupStep("network setup", bestEffort, func() error {
			return setupNetwork(bestEffort)
		}); err != nil {
			return errx.Wrap(ErrSetupLinux, err)
		}
		fmt.Println()
	}

	fmt.Println("Host setup complete!")
	return nil
}

func runSetupUser(cmd *cobra.Command, args []string) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("this command requires root privileges. Run with: sudo matchlock setup user <name>")
	}

	userName := args[0]
	if _, err := user.Lookup(userName); err != nil {
		return errx.Wrap(ErrDetermineUser, err)
	}

	if err := setupUserGroups(userName); err != nil {
		return errx.Wrap(ErrSetupLinux, err)
	}

	fmt.Println()
	fmt.Println("Setup complete!")
	fmt.Printf("User %s must log out and back in for group changes to take effect.\n", userName)
	return nil
}

func resolveSetupUser(cmd *cobra.Command) (string, error) {
	userName, _ := cmd.Flags().GetString("user")
	if userName != "" {
		return userName, nil
	}

	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		return sudoUser, nil
	}

	u, err := user.Current()
	if err != nil {
		return "", errx.Wrap(ErrDetermineUser, err)
	}
	return u.Username, nil
}

// installFirecracker extracts the Firecracker and jailer binaries for the
// current architecture from the version vendored into this binary. It never
// contacts the network.
//
// The destination directory comes from --install-dir (default /usr/libexec/matchlock).
// NOTE: the runtime resolver (pkg/firecracker.ResolveFirecrackerPath) consults
// MATCHLOCK_FIRECRACKER / MATCHLOCK_JAILER env overrides first, then the FIXED
// /usr/libexec/matchlock, then PATH. A custom --install-dir therefore requires
// PATH or an explicit env override for the runtime to find it. We verify the
// exact file we wrote, not the resolver's result, so a PATH or env install is
// never misreported as ours.
//
// On success the installed firecracker is executed to confirm the vendored
// version is actually runnable; failure to execute is treated as an error
// rather than a silent success.
func installFirecracker(installDir string) error {
	fmt.Println("=== Installing Firecracker (from vendored assets) ===")

	arch, err := fcassets.ResolveArch(runtime.GOARCH)
	if err != nil {
		return err
	}

	version := fcassets.PinnedVersion()
	commit := fcassets.ReleaseCommit()
	fmt.Printf("Vendored Firecracker %s (upstream release commit %s)\n", version, commit)
	fmt.Printf("Extracting firecracker + jailer to %s ...\n", installDir)

	if err := fcassets.InstallBoth(arch, installDir); err != nil {
		return errx.Wrap(ErrSetupLinux, err)
	}

	fcPath := filepath.Join(installDir, "firecracker")
	ver, err := exec.Command(fcPath, "--version").Output()
	if err != nil {
		return errx.With(ErrSetupLinux, ": installed firecracker does not execute: %w", err)
	}
	fields := strings.Fields(string(ver))
	if len(fields) >= 2 {
		fmt.Printf("✓ Installed firecracker %s to %s\n", fields[1], fcPath)
	} else {
		fmt.Printf("✓ Installed firecracker to %s (%s)\n", fcPath, strings.TrimSpace(string(ver)))
	}

	checkKVM()
	return nil
}

func getFirecrackerVersion() string {
	out, err := exec.Command(firecracker.ResolveFirecrackerPath(), "--version").Output()
	if err != nil {
		return ""
	}
	parts := strings.Fields(string(out))
	if len(parts) >= 2 {
		return parts[1]
	}
	return strings.TrimSpace(string(out))
}

func checkKVM() {
	fmt.Println()
	if _, err := os.Stat("/dev/kvm"); err == nil {
		fmt.Println("✓ KVM is available")
	} else {
		fmt.Println("⚠ KVM not available")
		fmt.Println("  Enable virtualization in BIOS/UEFI")
		fmt.Println("  Or run: sudo modprobe kvm kvm_intel (or kvm_amd)")
	}
}

func setupHostPermissions(binaryPath string, bestEffort bool) error {
	fmt.Println("=== Setting up host permissions ===")

	if err := runSetupStep("set binary capabilities", bestEffort, func() error {
		return setCapabilities(binaryPath)
	}); err != nil {
		return err
	}

	if err := runSetupStep("setup /dev/net/tun", bestEffort, setupTunDevice); err != nil {
		return err
	}

	return nil
}

func setupUserGroups(userName string) error {
	fmt.Printf("=== Enrolling user %s ===\n", userName)

	if err := addUserToKVMGroup(userName); err != nil {
		return errx.With(ErrSetupStep, " add user to kvm group: %w", err)
	}

	if err := addUserToNetdevGroup(userName); err != nil {
		return errx.With(ErrSetupStep, " add user to netdev group: %w", err)
	}

	return nil
}

func addUserToKVMGroup(userName string) error {
	out, err := exec.Command("groups", userName).Output()
	if err != nil {
		return errx.With(ErrSetupStep, " add user to kvm group: %w", err)
	}
	groups := strings.Fields(string(out))
	for _, g := range groups {
		if g == "kvm" {
			fmt.Println("✓ User already in kvm group")
			return nil
		}
	}

	if err := exec.Command("usermod", "-aG", "kvm", userName).Run(); err != nil {
		return err
	}
	fmt.Printf("✓ Added %s to kvm group\n", userName)
	return nil
}

func addUserToNetdevGroup(userName string) error {
	if err := exec.Command("getent", "group", "netdev").Run(); err != nil {
		if err := exec.Command("groupadd", "netdev").Run(); err != nil {
			return errx.Wrap(ErrCreateNetdev, err)
		}
		fmt.Println("✓ Created netdev group")
	}

	out, err := exec.Command("groups", userName).Output()
	if err != nil {
		return errx.With(ErrSetupStep, " inspect groups for %s: %w", userName, err)
	}
	if strings.Contains(string(out), "netdev") {
		fmt.Println("✓ User already in netdev group")
		return nil
	}

	if err := exec.Command("usermod", "-aG", "netdev", userName).Run(); err != nil {
		return errx.With(ErrAddToNetdev, " %s: %w", userName, err)
	}
	fmt.Printf("✓ Added %s to netdev group\n", userName)
	return nil
}

func setCapabilities(binaryPath string) error {
	if _, err := os.Stat(binaryPath); err != nil {
		fmt.Printf("⚠ Binary not found at %s - skipping capability setup\n", binaryPath)
		return nil
	}

	if err := exec.Command("setcap", "cap_net_admin,cap_net_raw+ep", binaryPath).Run(); err != nil {
		return err
	}
	fmt.Printf("✓ Set capabilities on %s\n", binaryPath)
	return nil
}

func setupTunDevice() error {
	if _, err := os.Stat("/dev/net/tun"); os.IsNotExist(err) {
		if err := os.MkdirAll("/dev/net", 0755); err != nil {
			return errx.With(ErrSetupStep, " create /dev/net: %w", err)
		}
		if err := exec.Command("mknod", "/dev/net/tun", "c", "10", "200").Run(); err != nil {
			return err
		}
	}

	if err := exec.Command("getent", "group", "netdev").Run(); err != nil {
		if err := exec.Command("groupadd", "netdev").Run(); err != nil {
			return errx.Wrap(ErrCreateNetdev, err)
		}
		fmt.Println("✓ Created netdev group")
	}

	if err := exec.Command("chown", "root:netdev", "/dev/net/tun").Run(); err != nil {
		return errx.Wrap(ErrChownTun, err)
	}
	if err := os.Chmod("/dev/net/tun", 0660); err != nil {
		return err
	}
	fmt.Println("✓ /dev/net/tun is accessible (group netdev, mode 0660)")
	return nil
}

func setupNetwork(bestEffort bool) error {
	fmt.Println("=== Setting up network ===")

	if err := runSetupStep("enable IP forwarding", bestEffort, enableIPForwarding); err != nil {
		return err
	}

	if err := runSetupStep("load nf_tables kernel module", bestEffort, checkNftables); err != nil {
		return err
	}

	return nil
}

func enableIPForwarding() error {
	dropInFile := "/etc/sysctl.d/99-matchlock.conf"
	content := "# Enable IP forwarding for matchlock VM networking\nnet.ipv4.ip_forward = 1\n"

	if existing, err := os.ReadFile(dropInFile); err == nil {
		if string(existing) == content {
			fmt.Println("✓ IP forwarding already configured")
			if err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run(); err != nil {
				return err
			}
			return nil
		}
	}

	if err := os.WriteFile(dropInFile, []byte(content), 0644); err != nil {
		return errx.With(ErrWriteSysctl, " %s: %w", dropInFile, err)
	}

	if err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run(); err != nil {
		return err
	}
	fmt.Printf("✓ Enabled IP forwarding (via %s)\n", dropInFile)
	return nil
}

func checkNftables() error {
	if err := exec.Command("modprobe", "nf_tables").Run(); err != nil {
		return errx.Wrap(ErrNfTablesModule, err)
	}
	fmt.Println("✓ nftables kernel module loaded")
	return nil
}

func runSetupStep(name string, bestEffort bool, fn func() error) error {
	err := fn()
	if err == nil {
		return nil
	}
	wrapped := errx.With(ErrSetupStep, " %s: %w", name, err)
	if bestEffort {
		fmt.Printf("⚠ %s failed: %v\n", name, err)
		return nil
	}
	return wrapped
}
