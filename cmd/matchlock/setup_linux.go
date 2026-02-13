//go:build linux

package main

import (
	"archive/tar"
	"compress/gzip"
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
)

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Setup matchlock environment",
}

var setupLinuxCmd = &cobra.Command{
	Use:   "linux",
	Short: "Setup matchlock for Linux (installs Firecracker and configures permissions)",
	Long: `Setup matchlock for Linux by:
  1. Installing Firecracker from GitHub releases
  2. Adding current user to kvm group
  3. Setting capabilities on matchlock binary
  4. Enabling IP forwarding
  5. Configuring /dev/net/tun
  6. Ensuring nftables kernel module is loaded

This command requires root privileges.`,
	RunE: runSetupLinux,
}

func init() {
	setupLinuxCmd.Flags().String("user", "", "Username to configure (default: current user or SUDO_USER)")
	setupLinuxCmd.Flags().String("binary", "", "Path to matchlock binary (default: auto-detect)")
	setupLinuxCmd.Flags().String("install-dir", "/usr/local/bin", "Directory to install Firecracker")
	setupLinuxCmd.Flags().Bool("skip-firecracker", false, "Skip Firecracker installation")
	setupLinuxCmd.Flags().Bool("skip-permissions", false, "Skip permission setup")
	setupLinuxCmd.Flags().Bool("skip-network", false, "Skip network configuration")

	setupCmd.AddCommand(setupLinuxCmd)
	rootCmd.AddCommand(setupCmd)
}

func runSetupLinux(cmd *cobra.Command, args []string) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("this command requires root privileges. Run with: sudo matchlock setup linux")
	}

	skipFirecracker, _ := cmd.Flags().GetBool("skip-firecracker")
	skipPermissions, _ := cmd.Flags().GetBool("skip-permissions")
	skipNetwork, _ := cmd.Flags().GetBool("skip-network")
	installDir, _ := cmd.Flags().GetString("install-dir")
	userName, _ := cmd.Flags().GetString("user")
	binaryPath, _ := cmd.Flags().GetString("binary")

	if userName == "" {
		userName = os.Getenv("SUDO_USER")
		if userName == "" {
			u, err := user.Current()
			if err != nil {
				return fmt.Errorf("%w: %w", ErrDetermineUser, err)
			}
			userName = u.Username
		}
	}

	if binaryPath == "" {
		if exe, err := os.Executable(); err == nil {
			binaryPath = exe
		} else {
			binaryPath = "./bin/matchlock"
		}
	}

	fmt.Printf("Setting up matchlock for user: %s\n\n", userName)

	if !skipFirecracker {
		if err := installFirecracker(installDir); err != nil {
			fmt.Printf("⚠ Firecracker installation failed: %v\n", err)
		}
		fmt.Println()
	}

	if !skipPermissions {
		if err := setupPermissions(userName, binaryPath); err != nil {
			fmt.Printf("⚠ Permission setup failed: %v\n", err)
		}
		fmt.Println()
	}

	if !skipNetwork {
		if err := setupNetwork(); err != nil {
			fmt.Printf("⚠ Network setup failed: %v\n", err)
		}
		fmt.Println()
	}

	fmt.Println("Setup complete!")
	fmt.Println("Please log out and back in for group changes to take effect.")
	return nil
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
		fmt.Printf("✓ Firecracker %s already installed\n", installedVersion)
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
		return fmt.Errorf("%w: %w", ErrDownloadFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrGzipReader, err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	firecrackerBin := fmt.Sprintf("firecracker-%s-%s", version, arch)
	jailerBin := fmt.Sprintf("jailer-%s-%s", version, arch)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %w", ErrTarReader, err)
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
			return fmt.Errorf("%w %s: %w", ErrCreateFile, destPath, err)
		}

		_, err = io.Copy(f, tr)
		f.Close()
		if err != nil {
			return fmt.Errorf("%w %s: %w", ErrWriteFile, destPath, err)
		}
		fmt.Printf("✓ Installed %s\n", destPath)
	}

	if newVersion := getFirecrackerVersion(); newVersion != "" {
		fmt.Printf("✓ Firecracker %s installed successfully\n", newVersion)
	}

	checkKVM()
	return nil
}

func getFirecrackerVersion() string {
	out, err := exec.Command("firecracker", "--version").Output()
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(string(body), ",") {
		if strings.Contains(line, `"tag_name"`) {
			parts := strings.Split(line, `"`)
			if len(parts) >= 4 {
				return parts[3], nil
			}
		}
	}
	return "", fmt.Errorf("could not parse version")
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

func setupPermissions(userName, binaryPath string) error {
	fmt.Println("=== Setting up permissions ===")

	if err := addUserToKVMGroup(userName); err != nil {
		fmt.Printf("⚠ Could not add user to kvm group: %v\n", err)
	}

	if err := setCapabilities(binaryPath); err != nil {
		fmt.Printf("⚠ Could not set capabilities: %v\n", err)
	}

	if err := setupTunDevice(userName); err != nil {
		fmt.Printf("⚠ Could not setup /dev/net/tun: %v\n", err)
	}

	return nil
}

func addUserToKVMGroup(userName string) error {
	out, _ := exec.Command("groups", userName).Output()
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

func setupTunDevice(userName string) error {
	if _, err := os.Stat("/dev/net/tun"); os.IsNotExist(err) {
		os.MkdirAll("/dev/net", 0755)
		if err := exec.Command("mknod", "/dev/net/tun", "c", "10", "200").Run(); err != nil {
			return err
		}
	}

	if err := exec.Command("getent", "group", "netdev").Run(); err != nil {
		if err := exec.Command("groupadd", "netdev").Run(); err != nil {
			return fmt.Errorf("%w: %w", ErrCreateNetdev, err)
		}
		fmt.Println("✓ Created netdev group")
	}

	out, _ := exec.Command("groups", userName).Output()
	if !strings.Contains(string(out), "netdev") {
		if err := exec.Command("usermod", "-aG", "netdev", userName).Run(); err != nil {
			return fmt.Errorf("%w %s: %w", ErrAddToNetdev, userName, err)
		}
		fmt.Printf("✓ Added %s to netdev group\n", userName)
	}

	if err := exec.Command("chown", "root:netdev", "/dev/net/tun").Run(); err != nil {
		return fmt.Errorf("%w: %w", ErrChownTun, err)
	}
	if err := os.Chmod("/dev/net/tun", 0660); err != nil {
		return err
	}
	fmt.Println("✓ /dev/net/tun is accessible (group netdev, mode 0660)")
	return nil
}

func setupNetwork() error {
	fmt.Println("=== Setting up network ===")

	if err := enableIPForwarding(); err != nil {
		fmt.Printf("⚠ Could not enable IP forwarding: %v\n", err)
	}

	if err := checkNftables(); err != nil {
		fmt.Printf("⚠ nftables check: %v\n", err)
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
		return fmt.Errorf("%w %s: %w", ErrWriteSysctl, dropInFile, err)
	}

	if err := exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run(); err != nil {
		return err
	}
	fmt.Printf("✓ Enabled IP forwarding (via %s)\n", dropInFile)
	return nil
}

func checkNftables() error {
	if err := exec.Command("modprobe", "nf_tables").Run(); err != nil {
		return fmt.Errorf("%w: %w", ErrNfTablesModule, err)
	}
	fmt.Println("✓ nftables kernel module loaded")
	return nil
}
