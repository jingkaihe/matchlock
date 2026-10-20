// Package darwin splits its code into layers so the non-VZ parts compile and
// run on every platform the module supports, while the Virtualization.framework
// adapter stays behind a darwin+cgo gate:
//
//   - this file (with errors.go and network.go) has no cgo and no
//     Virtualization.framework bindings. The kernel-command-line builder lives
//     here so its regressions (exact destinations, workspace, swap, overlay,
//     cpus, and the dual-stack interception boot args) are covered by ordinary
//     `go test ./pkg/vm/darwin` runs on any host.
//   - backend.go and machine.go are `darwin && cgo`: they create and drive the
//     VZ virtual machine.
//   - backend_stub.go and machine_stub.go are `!(darwin && cgo)`: they keep the
//     module building everywhere (so `CGO_ENABLED=0 GOOS=darwin go build ./...`
//     succeeds from any host) and fail loudly at Create; a cgo-less darwin
//     binary can never boot a VM.
package darwin

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

const (
	VsockPortExec  = 5000
	VsockPortVFS   = 5001
	VsockPortReady = 5002
)

type DarwinBackend struct{}

func NewDarwinBackend() *DarwinBackend {
	return &DarwinBackend{}
}

func (b *DarwinBackend) Name() string {
	return "virtualization.framework"
}

// buildKernelArgs renders the guest kernel command line for one of the three
// darwin network modes. It is a pure function of the VMConfig so every branch
// is testable without a VM:
//
//   - no-network: no NIC is attached at all (matchlock.no_network=1, ip=off).
//   - interception: file-handle NIC into the gVisor userspace stack; the guest
//     gets static IPv4 addressing and — when the config carries an IPv6 link —
//     the matchlock.ipv6= field, so guest-init installs the IPv6 address and
//     the ::/0 default route exactly as it does on Linux. The kernel's own ip=
//     argument is IPv4-only, which is why the address and route travel in
//     their own field.
//   - plain NAT (dhcp): VZ NAT with ip=dhcp. IPv6 stays deliberately absent so
//     the NAT branch behaves exactly as before (it has no interception policy
//     surface on either platform).
func (b *DarwinBackend) buildKernelArgs(config *vm.VMConfig) string {
	if config.KernelArgs != "" {
		return config.KernelArgs
	}

	workspace := config.Workspace
	workspaceArg := ""
	if workspace != "" {
		workspaceArg = " matchlock.workspace=" + workspace
	}

	exactArg := ""
	if len(config.ExactMounts) > 0 {
		exactArg = " matchlock.exact.mounts=" + strings.Join(config.ExactMounts, ",")
	}

	hostname := config.Hostname
	if hostname == "" {
		hostname = config.ID
	}

	mtu := effectiveMTU(config.MTU)

	// Root device is /dev/vda (first virtio block device)
	privilegedArg := ""
	if config.Privileged {
		privilegedArg = " matchlock.privileged=1"
	}

	diskArgs := ""
	devLetter := 'b' // vda is rootfs
	if config.OverlayEnabled {
		lowerDevs := make([]string, 0, len(config.OverlayLowerPaths))
		lowerFS := make([]string, 0, len(config.OverlayLowerPaths))
		for i := range config.OverlayLowerPaths {
			lowerDevs = append(lowerDevs, fmt.Sprintf("vd%c", devLetter))
			fsType := "erofs"
			if i < len(config.OverlayLowerFSTypes) && config.OverlayLowerFSTypes[i] != "" {
				fsType = config.OverlayLowerFSTypes[i]
			}
			lowerFS = append(lowerFS, fsType)
			devLetter++
		}
		upperDev := fmt.Sprintf("vd%c", devLetter)
		devLetter++
		diskArgs += fmt.Sprintf(
			" matchlock.overlay=1 matchlock.overlay.lower=%s matchlock.overlay.lowerfs=%s matchlock.overlay.upper=%s",
			strings.Join(lowerDevs, ","),
			strings.Join(lowerFS, ","),
			upperDev,
		)
	}
	for _, disk := range config.ExtraDisks {
		dev := fmt.Sprintf("vd%c", devLetter)
		devLetter++
		if disk.Swap {
			// A swap backing device is attached but never mounted. Announce the
			// device so guest-init can enable it at boot, and keep consuming the
			// device letter so later disks retain their correct names.
			diskArgs += " matchlock.swap=" + dev
			continue
		}
		diskMount := diskKernelArg(disk)
		diskArgs += fmt.Sprintf(" matchlock.disk.%s=%s", dev, diskMount)
	}

	addHostArgs := ""
	for i, mapping := range config.AddHosts {
		addHostArgs += fmt.Sprintf(" matchlock.add_host.%d=%s,%s", i, mapping.Host, mapping.IP)
	}

	if config.NoNetwork {
		return fmt.Sprintf(
			"console=hvc0 root=/dev/vda rw init=/init reboot=k panic=1 ip=off hostname=%s%s%s matchlock.dns=%s matchlock.no_network=1%s%s matchlock.cpus=%g",
			hostname, workspaceArg, exactArg, vm.KernelDNSParam(config.DNSServers), privilegedArg, diskArgs+addHostArgs, config.CPUs,
		)
	}

	if config.UseInterception {
		guestIP := config.GuestIP
		if guestIP == "" {
			guestIP = "192.168.100.2"
		}
		gatewayIP := config.GatewayIP
		if gatewayIP == "" {
			gatewayIP = "192.168.100.1"
		}
		// The IPv6 field is purely additive next to the IPv4 ip= argument and
		// only appears when the VMConfig carries an IPv6 link (see
		// vm.IPv6KernelArg), so an IPv4-only interception boot is
		// byte-identical to before.
		return fmt.Sprintf(
			"console=hvc0 root=/dev/vda rw init=/init reboot=k panic=1 ip=%s::%s:255.255.255.0::eth0:off%s hostname=%s%s%s matchlock.dns=%s matchlock.mtu=%d%s%s%s%s matchlock.cpus=%g",
			guestIP, gatewayIP, vm.KernelIPDNSSuffix(config.DNSServers), hostname, workspaceArg, exactArg, vm.KernelDNSParam(config.DNSServers), mtu, vm.IPv6KernelArg(config), privilegedArg, diskArgs, addHostArgs, config.CPUs,
		)
	}

	return fmt.Sprintf(
		"console=hvc0 root=/dev/vda rw init=/init reboot=k panic=1 ip=dhcp hostname=%s%s%s matchlock.dns=%s matchlock.mtu=%d%s%s%s matchlock.cpus=%g",
		hostname, workspaceArg, exactArg, vm.KernelDNSParam(config.DNSServers), mtu, privilegedArg, diskArgs, addHostArgs, config.CPUs,
	)
}

// CopyRootfsToTemp copies the rootfs image to a temp file so each VM gets a clean copy.
func CopyRootfsToTemp(srcPath string) (string, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	// Create temp file in same directory to ensure same filesystem (for efficient copy)
	dir := filepath.Dir(srcPath)
	dst, err := os.CreateTemp(dir, "matchlock-rootfs-*.ext4")
	if err != nil {
		// Fall back to system temp if same dir fails
		dst, err = os.CreateTemp("", "matchlock-rootfs-*.ext4")
		if err != nil {
			return "", err
		}
	}
	dstPath := dst.Name()

	_, err = io.Copy(dst, src)
	if err != nil {
		dst.Close()
		os.Remove(dstPath)
		return "", err
	}

	if err := dst.Close(); err != nil {
		os.Remove(dstPath)
		return "", err
	}

	return dstPath, nil
}

func diskKernelArg(disk vm.DiskConfig) string {
	parts := []string{disk.GuestMount}
	if disk.ReadOnly {
		parts = append(parts, "ro")
	}
	if disk.OwnerUID != nil {
		parts = append(parts, fmt.Sprintf("uid=%d", *disk.OwnerUID))
	}
	if disk.OwnerGID != nil {
		parts = append(parts, fmt.Sprintf("gid=%d", *disk.OwnerGID))
	}
	return strings.Join(parts, ",")
}

func effectiveMTU(mtu int) int {
	if mtu > 0 {
		return mtu
	}
	return api.DefaultNetworkMTU
}
