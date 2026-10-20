//go:build linux

package diagnose

import (
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/google/nftables"
	"golang.org/x/sys/unix"

	"github.com/jingkaihe/matchlock/pkg/kvm"
)

func Run() Result {
	checks := []Check{
		checkFirecrackerInstalled(),
		checkKVMDeviceExists(),
		checkKVMAcceleration(),
		checkKVMAccessible(),
		checkUserGroup("kvm", "Run `sudo matchlock setup user <name>` and log in again."),
		checkMatchlockCapabilities(),
		checkIPForwardingEnabled(),
		checkTunDeviceAccessible(),
		checkUserGroup("netdev", "Run `sudo matchlock setup user <name>` and log in again."),
		checkNFTablesAvailable(),
	}
	return finalize(checks)
}

func checkFirecrackerInstalled() Check {
	version := firecrackerVersion()
	if version == "" {
		return Fail("firecracker", "Firecracker is not installed or bundled package assets are missing.", "Run `sudo matchlock setup linux` or install the Matchlock package that bundles firecracker/jailer.")
	}
	return Pass("firecracker", fmt.Sprintf("Firecracker %s is available.", version))
}

func checkKVMDeviceExists() Check {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return Fail("kvm-device", "/dev/kvm is not available.", "Enable CPU virtualization in BIOS/UEFI and load the kvm kernel modules.")
	}
	return Pass("kvm-device", "/dev/kvm exists.")
}

func checkKVMAcceleration() Check {
	// Prefer the real probe: it opens /dev/kvm, verifies the KVM API version,
	// and creates a throwaway VM descriptor. That is far more reliable than the
	// x86-only vmx/svm /proc/cpuinfo heuristic, which is meaningless on arm64.
	return kvmAccelerationCheck(kvm.Check())
}

// kvmAccelerationCheck converts a KVM probe Result into a diagnostic. Factored
// out so every Status maps deterministically and is table-testable.
func kvmAccelerationCheck(res kvm.Result) Check {
	switch res.Status {
	case kvm.StatusAvailable:
		return Pass("cpu-virtualization", res.String())
	case kvm.StatusNotPresent:
		return Fail("cpu-virtualization", res.String(), "Enable CPU virtualization in BIOS/UEFI and load the kvm kernel modules.")
	case kvm.StatusPermissionDenied:
		return Fail("cpu-virtualization", res.String(), "Ensure your user is in the kvm group (run `sudo matchlock setup user <name>`), then log out and back in.")
	case kvm.StatusIncompatible, kvm.StatusUnusable:
		return Fail("cpu-virtualization", res.String(), "KVM present but not usable; check nested virtualization support on this host.")
	default:
		// Unexpected: the probe itself failed in a way we did not classify.
		// Never fall back to CPU flags here — a real probe failure must surface
		// as a probe diagnostic, not be overridden by an x86-only heuristic.
		return Warn("cpu-virtualization", res.String(), "KVM probe failed unexpectedly; inspect dmesg/`kvm-ok` and host kernel support.")
	}
}

func checkKVMAccessible() Check {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return Fail("kvm-access", fmt.Sprintf("/dev/kvm access: %v", err), "Run as a user in the kvm group, then log out and back in.")
	}
	_ = f.Close()
	return Pass("kvm-access", "/dev/kvm is readable and writable.")
}

func checkUserGroup(groupName, fix string) Check {
	currentUser, err := currentUser()
	if err != nil {
		return Fail("group-"+groupName, err.Error(), fix)
	}
	groupIDs, err := currentUser.GroupIds()
	if err != nil {
		return Fail("group-"+groupName, fmt.Sprintf("Could not inspect groups for %s: %v", currentUser.Username, err), fix)
	}
	for _, groupID := range groupIDs {
		group, err := user.LookupGroupId(groupID)
		if err != nil {
			return Fail("group-"+groupName, fmt.Sprintf("Could not resolve group id %s for %s: %v", groupID, currentUser.Username, err), fix)
		}
		if group.Name == groupName {
			return Pass("group-"+groupName, fmt.Sprintf("User %s is in %s.", currentUser.Username, groupName))
		}
	}
	return Fail("group-"+groupName, fmt.Sprintf("User %s is not in %s.", currentUser.Username, groupName), fix)
}

func checkMatchlockCapabilities() Check {
	binaryPath, err := os.Executable()
	if err != nil {
		return Fail("binary-capabilities", fmt.Sprintf("Could not resolve current binary: %v", err), "Verify the matchlock binary has cap_net_admin and cap_net_raw, or run `sudo matchlock setup linux`.")
	}
	capData, err := getFileCapability(binaryPath)
	if err != nil {
		return Fail("binary-capabilities", fmt.Sprintf("Could not inspect file capabilities: %v", err), "Run `sudo setcap cap_net_admin,cap_net_raw+ep <matchlock-binary>` or `sudo matchlock setup linux`.")
	}
	hasNetAdmin, hasNetRaw, err := hasRequiredNetworkCapabilities(capData)
	if err != nil {
		return Fail("binary-capabilities", fmt.Sprintf("Could not parse file capabilities: %v", err), "Run `sudo setcap cap_net_admin,cap_net_raw+ep <matchlock-binary>` or `sudo matchlock setup linux`.")
	}
	if hasNetAdmin && hasNetRaw {
		return Pass("binary-capabilities", "matchlock binary has network capabilities.")
	}
	return Fail("binary-capabilities", "matchlock binary is missing cap_net_admin and/or cap_net_raw.", "Run `sudo setcap cap_net_admin,cap_net_raw+ep <matchlock-binary>` or `sudo matchlock setup linux`.")
}

func checkIPForwardingEnabled() Check {
	data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return Fail("ip-forwarding", fmt.Sprintf("read ip_forward: %v", err), "Set net.ipv4.ip_forward=1 and persist it in /etc/sysctl.d/99-matchlock.conf.")
	}
	if strings.TrimSpace(string(data)) != "1" {
		return Fail("ip-forwarding", "net.ipv4.ip_forward is disabled.", "Run `sudo sysctl -w net.ipv4.ip_forward=1` and persist it in /etc/sysctl.d/99-matchlock.conf.")
	}
	return Pass("ip-forwarding", "net.ipv4.ip_forward is enabled.")
}

func checkTunDeviceAccessible() Check {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return Fail("tun-device", fmt.Sprintf("/dev/net/tun access: %v", err), "Create /dev/net/tun, set group netdev and mode 0660, or run `sudo matchlock setup linux`.")
	}
	_ = f.Close()
	return Pass("tun-device", "/dev/net/tun is readable and writable.")
}

func checkNFTablesAvailable() Check {
	conn, err := nftables.New()
	if err != nil {
		return Fail("nf_tables", fmt.Sprintf("nf_tables availability check: %v", err), "Ensure the nf_tables kernel subsystem is available and accessible on this host.")
	}
	_, err = conn.ListTables()
	if err != nil {
		return Fail("nf_tables", fmt.Sprintf("nf_tables availability check: %v", err), "Ensure the nf_tables kernel subsystem is available and accessible on this host.")
	}
	return Pass("nf_tables", "nf_tables is available.")
}

func currentUser() (*user.User, error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		u, err := user.Lookup(sudoUser)
		if err != nil {
			return nil, err
		}
		return u, nil
	}
	if current := os.Getenv("USER"); current != "" {
		u, err := user.Lookup(current)
		if err == nil {
			return u, nil
		}
	}
	return user.Current()
}

func hasCPUVirtualizationFlag(data []byte) bool {
	content := " " + string(data) + " "
	return strings.Contains(content, " vmx ") || strings.Contains(content, " svm ")
}

func getFileCapability(path string) ([]byte, error) {
	size, err := unix.Getxattr(path, "security.capability", nil)
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, unix.ENODATA
	}
	buf := make([]byte, size)
	read, err := unix.Getxattr(path, "security.capability", buf)
	if err != nil {
		return nil, err
	}
	return buf[:read], nil
}

func hasRequiredNetworkCapabilities(data []byte) (bool, bool, error) {
	if len(data) < 12 {
		return false, false, fmt.Errorf("security.capability too short")
	}
	permitted := data[4:8]
	const (
		capNetRaw   = 13
		capNetAdmin = 12
	)
	return capabilityBitSet(permitted, capNetAdmin), capabilityBitSet(permitted, capNetRaw), nil
}

func capabilityBitSet(set []byte, bit uint) bool {
	if len(set) < 4 {
		return false
	}
	mask := uint32(set[0]) | uint32(set[1])<<8 | uint32(set[2])<<16 | uint32(set[3])<<24
	return (mask & (1 << bit)) != 0
}
