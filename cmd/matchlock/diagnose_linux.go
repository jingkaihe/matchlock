//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
)

type diagnoseStatus string

const (
	diagnoseStatusPass diagnoseStatus = "pass"
	diagnoseStatusFail diagnoseStatus = "fail"
	diagnoseStatusWarn diagnoseStatus = "warn"
)

type diagnoseCheck struct {
	Name    string         `json:"name"`
	Status  diagnoseStatus `json:"status"`
	Message string         `json:"message"`
	Fix     string         `json:"fix,omitempty"`
}

type diagnoseResult struct {
	OK     bool            `json:"ok"`
	Checks []diagnoseCheck `json:"checks"`
}

type commandRunner func(name string, args ...string) *exec.Cmd

var execCommand commandRunner = exec.Command

var diagnoseCmd = &cobra.Command{
	Use:   "diagnose",
	Short: "Diagnose host requirements for running matchlock",
	RunE:  runDiagnose,
}

func init() {
	diagnoseCmd.Flags().Bool("json", false, "Output machine-readable JSON")
	rootCmd.AddCommand(diagnoseCmd)
}

func runDiagnose(cmd *cobra.Command, args []string) error {
	jsonOutput, _ := cmd.Flags().GetBool("json")
	result := runLinuxDiagnose()

	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			return err
		}
	} else {
		for _, check := range result.Checks {
			icon := "✓"
			switch check.Status {
			case diagnoseStatusFail:
				icon = "✗"
			case diagnoseStatusWarn:
				icon = "⚠"
			}
			fmt.Printf("%s %s: %s\n", icon, check.Name, check.Message)
			if check.Fix != "" {
				fmt.Printf("  fix: %s\n", check.Fix)
			}
		}
	}

	if !result.OK {
		return errx.With(ErrDiagnoseFailed, ": one or more checks failed")
	}
	return nil
}

func runLinuxDiagnose() diagnoseResult {
	checks := []diagnoseCheck{
		checkFirecrackerInstalled(),
		checkKVMDeviceExists(),
		checkKVMAcceleration(),
		checkKVMAccessible(),
		checkUserGroup("kvm", "Add your user to the kvm group and log in again."),
		checkMatchlockCapabilities(),
		checkIPForwardingEnabled(),
		checkTunDeviceAccessible(),
		checkUserGroup("netdev", "Add your user to the netdev group and log in again, or run `sudo matchlock setup linux`."),
		checkNFTablesAvailable(),
	}

	ok := true
	for _, check := range checks {
		if check.Status == diagnoseStatusFail {
			ok = false
			break
		}
	}

	return diagnoseResult{OK: ok, Checks: checks}
}

func checkFirecrackerInstalled() diagnoseCheck {
	version := getFirecrackerVersion()
	if version == "" {
		return diagnoseCheck{
			Name:    "firecracker",
			Status:  diagnoseStatusFail,
			Message: "Firecracker is not installed or not on PATH.",
			Fix:     "Run `sudo matchlock setup linux` or install firecracker and jailer into your PATH.",
		}
	}

	return diagnoseCheck{
		Name:    "firecracker",
		Status:  diagnoseStatusPass,
		Message: fmt.Sprintf("Firecracker %s is available.", version),
	}
}

func checkKVMDeviceExists() diagnoseCheck {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return diagnoseCheck{
			Name:    "kvm-device",
			Status:  diagnoseStatusFail,
			Message: "/dev/kvm is not available.",
			Fix:     "Enable CPU virtualization in BIOS/UEFI and load the kvm kernel modules.",
		}
	}

	return diagnoseCheck{
		Name:    "kvm-device",
		Status:  diagnoseStatusPass,
		Message: "/dev/kvm exists.",
	}
}

func checkKVMAcceleration() diagnoseCheck {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return diagnoseCheck{
			Name:    "cpu-virtualization",
			Status:  diagnoseStatusWarn,
			Message: errx.Wrap(ErrReadCPUInfo, err).Error(),
			Fix:     "Verify your CPU exposes vmx (Intel) or svm (AMD) flags.",
		}
	}

	if hasCPUVirtualizationFlag(data) {
		return diagnoseCheck{
			Name:    "cpu-virtualization",
			Status:  diagnoseStatusPass,
			Message: "CPU virtualization flags are present.",
		}
	}

	return diagnoseCheck{
		Name:    "cpu-virtualization",
		Status:  diagnoseStatusFail,
		Message: "CPU virtualization flags (vmx/svm) were not found.",
		Fix:     "Enable virtualization in BIOS/UEFI or use a host with hardware virtualization support.",
	}
}

func checkKVMAccessible() diagnoseCheck {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return diagnoseCheck{
			Name:    "kvm-access",
			Status:  diagnoseStatusFail,
			Message: errx.Wrap(ErrKVMAccess, err).Error(),
			Fix:     "Run as a user in the kvm group, then log out and back in.",
		}
	}
	_ = f.Close()

	return diagnoseCheck{
		Name:    "kvm-access",
		Status:  diagnoseStatusPass,
		Message: "/dev/kvm is readable and writable.",
	}
}

func checkUserGroup(groupName, fix string) diagnoseCheck {
	currentUser, err := currentUserName()
	if err != nil {
		return diagnoseCheck{
			Name:    "group-" + groupName,
			Status:  diagnoseStatusFail,
			Message: err.Error(),
			Fix:     fix,
		}
	}

	out, err := execCommand("groups", currentUser).Output()
	if err != nil {
		return diagnoseCheck{
			Name:    "group-" + groupName,
			Status:  diagnoseStatusFail,
			Message: fmt.Sprintf("Could not inspect groups for %s: %v", currentUser, err),
			Fix:     fix,
		}
	}

	for _, g := range strings.Fields(string(out)) {
		if g == groupName {
			return diagnoseCheck{
				Name:    "group-" + groupName,
				Status:  diagnoseStatusPass,
				Message: fmt.Sprintf("User %s is in %s.", currentUser, groupName),
			}
		}
	}

	return diagnoseCheck{
		Name:    "group-" + groupName,
		Status:  diagnoseStatusFail,
		Message: fmt.Sprintf("User %s is not in %s.", currentUser, groupName),
		Fix:     fix,
	}
}

func checkMatchlockCapabilities() diagnoseCheck {
	binaryPath, err := os.Executable()
	if err != nil {
		return diagnoseCheck{
			Name:    "binary-capabilities",
			Status:  diagnoseStatusFail,
			Message: fmt.Sprintf("Could not resolve current binary: %v", err),
			Fix:     "Verify the matchlock binary has cap_net_admin and cap_net_raw, or run `sudo matchlock setup linux`.",
		}
	}

	out, err := execCommand("getcap", binaryPath).Output()
	if err != nil {
		return diagnoseCheck{
			Name:    "binary-capabilities",
			Status:  diagnoseStatusFail,
			Message: fmt.Sprintf("Could not inspect file capabilities: %v", err),
			Fix:     "Install libcap tools and verify the matchlock binary capabilities.",
		}
	}

	capLine := string(out)
	if strings.Contains(capLine, "cap_net_admin") && strings.Contains(capLine, "cap_net_raw") {
		return diagnoseCheck{
			Name:    "binary-capabilities",
			Status:  diagnoseStatusPass,
			Message: "matchlock binary has network capabilities.",
		}
	}

	return diagnoseCheck{
		Name:    "binary-capabilities",
		Status:  diagnoseStatusFail,
		Message: "matchlock binary is missing cap_net_admin and/or cap_net_raw.",
		Fix:     "Run `sudo setcap cap_net_admin,cap_net_raw+ep <matchlock-binary>` or `sudo matchlock setup linux`.",
	}
}

func checkIPForwardingEnabled() diagnoseCheck {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return diagnoseCheck{
			Name:    "ip-forwarding",
			Status:  diagnoseStatusFail,
			Message: errx.Wrap(ErrReadIPForward, err).Error(),
			Fix:     "Set net.ipv4.ip_forward=1 and persist it in /etc/sysctl.d/99-matchlock.conf.",
		}
	}

	if strings.TrimSpace(string(data)) != "1" {
		return diagnoseCheck{
			Name:    "ip-forwarding",
			Status:  diagnoseStatusFail,
			Message: "net.ipv4.ip_forward is disabled.",
			Fix:     "Run `sudo sysctl -w net.ipv4.ip_forward=1` and persist it in /etc/sysctl.d/99-matchlock.conf.",
		}
	}

	return diagnoseCheck{
		Name:    "ip-forwarding",
		Status:  diagnoseStatusPass,
		Message: "net.ipv4.ip_forward is enabled.",
	}
}

func checkTunDeviceAccessible() diagnoseCheck {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return diagnoseCheck{
			Name:    "tun-device",
			Status:  diagnoseStatusFail,
			Message: errx.Wrap(ErrTunAccess, err).Error(),
			Fix:     "Create /dev/net/tun, set group netdev and mode 0660, or run `sudo matchlock setup linux`.",
		}
	}
	_ = f.Close()

	return diagnoseCheck{
		Name:    "tun-device",
		Status:  diagnoseStatusPass,
		Message: "/dev/net/tun is readable and writable.",
	}
}

func checkNFTablesAvailable() diagnoseCheck {
	cmd := execCommand("modprobe", "-n", "-v", "nf_tables")
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = errx.Wrap(ErrNFTablesCheck, err).Error()
		}
		return diagnoseCheck{
			Name:    "nf_tables",
			Status:  diagnoseStatusFail,
			Message: message,
			Fix:     "Ensure the nf_tables kernel module is available on this host.",
		}
	}

	return diagnoseCheck{
		Name:    "nf_tables",
		Status:  diagnoseStatusPass,
		Message: "nf_tables is available.",
	}
}

func currentUserName() (string, error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		return sudoUser, nil
	}
	if current := os.Getenv("USER"); current != "" {
		return current, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", errx.Wrap(ErrDetermineUser, err)
	}
	return u.Username, nil
}

func hasCPUVirtualizationFlag(data []byte) bool {
	content := " " + string(data) + " "
	return strings.Contains(content, " vmx ") || strings.Contains(content, " svm ")
}
