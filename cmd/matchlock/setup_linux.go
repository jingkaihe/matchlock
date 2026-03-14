//go:build linux

package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

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
	setupLinuxCmd.Flags().String("install-dir", "/usr/local/bin", "Directory to install Firecracker")
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

func installFirecracker(installDir string) error {
	fmt.Println("=== Installing Firecracker ===")

	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	} else if arch == "arm64" {
		arch = "aarch64"
	}

	installedVersion := getFirecrackerVersion()
	if installedVersion != "" {
		fmt.Printf("✓ Firecracker %s already available\n", installedVersion)
		return nil
	}

	version, err := getLatestFirecrackerVersion()
	if err != nil {
		version = "v1.10.1"
		fmt.Printf("Could not fetch latest version, using %s\n", version)
	} else {
		fmt.Printf("Latest version: %s\n", version)
	}

	url := fmt.Sprintf("https://github.com/firecracker-microvm/firecracker/releases/download/%s/firecracker-%s-%s.tgz",
		version, version, arch)

	fmt.Printf("Downloading from %s...\n", url)

	resp, err := http.Get(url)
	if err != nil {
		return errx.Wrap(ErrDownloadFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return errx.Wrap(ErrGzipReader, err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	firecrackerBin := fmt.Sprintf("firecracker-%s-%s", version, arch)
	jailerBin := fmt.Sprintf("jailer-%s-%s", version, arch)

	installed := map[string]bool{
		"firecracker": false,
		"jailer":      false,
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errx.Wrap(ErrTarReader, err)
		}

		baseName := filepath.Base(hdr.Name)
		var destName string
		if baseName == firecrackerBin {
			destName = "firecracker"
		} else if baseName == jailerBin {
			destName = "jailer"
		} else {
			continue
		}

		destPath := filepath.Join(installDir, destName)
		f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return errx.With(ErrCreateFile, " %s: %w", destPath, err)
		}

		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return errx.With(ErrWriteFile, " %s: %w", destPath, err)
		}
		installed[destName] = true
		fmt.Printf("✓ Installed %s\n", destPath)
	}

	for binary, ok := range installed {
		if !ok {
			return errx.With(ErrDownloadFailed, ": %s not found in Firecracker archive", binary)
		}
	}

	if newVersion := getFirecrackerVersion(); newVersion != "" {
		fmt.Printf("✓ Firecracker %s installed successfully\n", newVersion)
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

func getLatestFirecrackerVersion() (string, error) {
	resp, err := http.Get("https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if payload.TagName == "" {
		return "", fmt.Errorf("could not parse version")
	}
	return payload.TagName, nil
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
