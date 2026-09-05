//go:build linux

package qemu

import (
	"fmt"
	"os"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
	linuxvm "github.com/jingkaihe/matchlock/pkg/vm/linux"
)

// tapNamePrefix is the interface-name prefix for QEMU TAP devices. Firecracker
// uses "fc-"; QEMU uses "qm-" so the two never collide on the same host.
const tapNamePrefix = "qm-"

// networkSetup owns a host TAP device and its open file descriptor. The TAP is
// created and configured in Backend.Create (before Start) so the sandbox can
// read TapName() and provision the host-side firewall/NAT; Start then passes the
// open FD to QEMU via ExtraFiles so QEMU never needs CAP_NET_ADMIN.
type networkSetup struct {
	tapName string
	tapFile *os.File
}

// setupNetwork creates a TAP, configures it with the gateway address and MTU,
// and returns an owned setup. It mirrors the Linux backend defaults
// (gateway 192.168.100.1/24, guest 192.168.100.2). The TAP is NON-PERSISTENT, so
// the kernel destroys it once the LAST descriptor closes. QEMU inherits a dup via
// ExtraFiles and the rpc parent keeps its own fd, so the interface survives while
// either holds a descriptor and is reaped when both are gone (clean Close
// teardown, or a hard crash where QEMU and the rpc both exit) — no persistent
// TAP left behind. On setup failure it releases the FD.
func (m *Machine) setupNetwork() (*networkSetup, error) {
	tapName := tapNameForVMID(m.config.ID)

	tapFD, err := linuxvm.CreateNonPersistentTAP(tapName)
	if err != nil {
		return nil, errx.Wrap(ErrCreateTAP, err)
	}
	// Ownership: tapFD must be released if any later step fails. If everything
	// succeeds we keep it open as the parent copy (QEMU gets its own dup via
	// ExtraFiles). For a non-persistent TAP, closing the fd is sufficient to
	// release the interface; we must NOT reopen by name (see teardownNetwork).
	tapFile := os.NewFile(uintptr(tapFD), tapName)
	fail := func(opErr error) error {
		_ = tapFile.Close()
		return opErr
	}

	subnetCIDR := m.config.SubnetCIDR
	if subnetCIDR == "" {
		subnetCIDR = "192.168.100.1/24"
	}
	if err := linuxvm.ConfigureInterface(tapName, subnetCIDR); err != nil {
		return nil, fail(errx.Wrap(ErrTAPConfigure, err))
	}
	if err := linuxvm.SetMTU(tapName, effectiveMTU(m.config.MTU)); err != nil {
		return nil, fail(errx.Wrap(ErrTAPSetMTU, err))
	}

	return &networkSetup{tapName: tapName, tapFile: tapFile}, nil
}

// teardownNetwork releases the host-side TAP. It must be called only after the
// QEMU child has exited (so its inherited dup is gone). For a NON-PERSISTENT TAP
// the kernel destroys the interface when the last descriptor closes, so this only
// needs to close the parent fd. It MUST NOT call linuxvm.DeleteInterface by name:
// DeleteInterface re-issues TUNSETIFF, which would RE-CREATE the interface if it
// already disappeared (the exact leak this fix removes). Re-running is a no-op.
func (n *networkSetup) teardownNetwork() error {
	if n == nil {
		return nil
	}
	var errs []string
	if n.tapFile != nil {
		if err := n.tapFile.Close(); err != nil && !errorsIsClosed(err) {
			errs = append(errs, err.Error())
		}
		n.tapFile = nil
	}
	n.tapName = ""
	if len(errs) > 0 {
		return errx.With(ErrTeardownTAP, ": %s", strings.Join(errs, "; "))
	}
	return nil
}

// childFDFor maps an ExtraFiles index to the fd number the child sees: fd 3 is
// the first entry, so entry index i maps to 3+i.
func childFDFor(extraFilesIndex int) int { return 3 + extraFilesIndex }

// networkArgs returns QEMU -netdev/-device args that attach the guest NIC to
// the already-open TAP fd using userspace virtio networking. The fd is passed
// to QEMU via ExtraFiles (hence the number, not a path), so QEMU needs no
// CAP_NET_ADMIN and the single-queue TAP is never reopened.
func (n *networkSetup) networkArgs(extraFilesIndex int) []string {
	return []string{
		"-netdev", fmt.Sprintf("tap,id=net0,fd=%d,vhost=off", childFDFor(extraFilesIndex)),
		"-device", "virtio-net-pci,netdev=net0,mac=" + generateMAC("qemu-"+n.tapName),
	}
}

// generateMAC produces a Locally Administered unicast MAC from a seed string,
// mirroring the Linux backend's GenerateMAC.
func generateMAC(seed string) string {
	h := uint32(0)
	for _, c := range seed {
		h = h*31 + uint32(c)
	}
	return fmt.Sprintf("AA:FC:%02X:%02X:%02X:%02X",
		byte(h>>24), byte(h>>16), byte(h>>8), byte(h))
}

// networkBootArgs returns the kernel-cmdline fragment that statically configures
// the guest NIC so guest-init's bringUpNetwork sees an IPv4 address. Guest IP
// and gateway come from the sandbox-allocated subnet; DNS is appended up to the
// ip= limit of two servers.
func (m *Machine) networkBootArgs() string {
	guestIP := m.config.GuestIP
	if guestIP == "" {
		guestIP = "192.168.100.2"
	}
	gatewayIP := m.config.GatewayIP
	if gatewayIP == "" {
		gatewayIP = "192.168.100.1"
	}
	mtu := effectiveMTU(m.config.MTU)
	// ip=<guest_ip>::<gateway>:<netmask>::eth0:off<dns_suffix>
	return fmt.Sprintf(" ip=%s::%s:255.255.255.0::eth0:off%s matchlock.mtu=%d",
		guestIP, gatewayIP, kernelIPDNSSuffix(m.config.DNSServers), mtu)
}

// kernelIPDNSSuffix returns the ip= DNS suffix (max two servers, per the ip=
// kernel parameter format) so the guest gets name resolution without a DHCP run.
func kernelIPDNSSuffix(dnsServers []string) string {
	var sb strings.Builder
	for i, s := range dnsServers {
		if i >= 2 {
			break
		}
		sb.WriteByte(':')
		sb.WriteString(s)
	}
	return sb.String()
}

// tapNameForVMID derives a stable host-unique TAP name from a VM ID, matching
// Firecracker's scheme so lifecycle reconciliation sees a predictable label.
func tapNameForVMID(vmID string) string {
	suffix := strings.TrimPrefix(vmID, "vm-")
	if len(suffix) >= 8 {
		suffix = suffix[:8]
	}
	return tapNamePrefix + suffix
}

func effectiveMTU(mtu int) int {
	if mtu > 0 {
		return mtu
	}
	return 1500
}

func errorsIsClosed(err error) bool {
	return err != nil &&
		(strings.Contains(err.Error(), "file already closed") || strings.Contains(err.Error(), "closed file"))
}
