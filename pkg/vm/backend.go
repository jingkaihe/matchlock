package vm

import (
	"context"
	"io"
	"net"
	"strings"

	"github.com/jingkaihe/matchlock/pkg/api"
)

// DiskConfig describes an additional block device to attach to the VM.
type DiskConfig struct {
	HostPath   string // Path to the ext4 image on the host
	GuestMount string // Mount point inside the guest (e.g., "/var/lib/buildkit")
	ReadOnly   bool
}

type VMConfig struct {
	ID              string
	KernelPath      string
	InitramfsPath   string // Optional initramfs/initrd path
	RootfsPath      string
	CPUs            int
	MemoryMB        int
	NetworkFD       int
	VsockCID        uint32
	VsockPath       string
	SocketPath      string
	LogPath         string
	KernelArgs      string
	Env             map[string]string
	GatewayIP       string       // Host TAP IP (e.g., 192.168.100.1)
	GuestIP         string       // Guest IP (e.g., 192.168.100.2)
	SubnetCIDR      string       // CIDR notation (e.g., 192.168.100.1/24)
	Workspace       string       // Guest VFS mount point (default: /workspace)
	UseInterception bool         // Use network interception (MITM proxy)
	Privileged      bool         // Skip in-guest security restrictions (seccomp, cap drop, no_new_privs)
	DNSServers      []string     // DNS servers for the guest (default: 8.8.8.8, 8.8.4.4)
	Hostname        string       // Hostname for the guest (default: vm's ID)
	MTU             int          // Guest interface/network stack MTU (default: 1500)
	PrebuiltRootfs  string       // Pre-prepared rootfs path (skips internal copy if set)
	ExtraDisks      []DiskConfig // Additional block devices to attach
}

type Backend interface {
	Create(ctx context.Context, config *VMConfig) (Machine, error)
	Name() string
}

type Machine interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Wait(ctx context.Context) error
	Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error)
	NetworkFD() (int, error)
	VsockFD() (int, error)
	PID() int
	Close(ctx context.Context) error
	RootfsPath() string // Returns the path to the VM's rootfs (may be a temp copy)
}

type InteractiveMachine interface {
	Machine
	ExecInteractive(ctx context.Context, command string, opts *api.ExecOptions, rows, cols uint16, stdin io.Reader, stdout io.Writer, resizeCh <-chan [2]uint16) (int, error)
}

// VsockDialer is implemented by backends that can establish host-initiated
// vsock connections to guest service ports.
type VsockDialer interface {
	DialVsock(port uint32) (net.Conn, error)
}

// KernelIPDNSSuffix returns the DNS portion of the kernel ip= parameter.
// The ip= format only supports up to 2 DNS servers (`:dns0:dns1`).
func KernelIPDNSSuffix(dnsServers []string) string {
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

// KernelDNSParam returns a comma-separated DNS list for the matchlock.dns= cmdline param.
func KernelDNSParam(dnsServers []string) string {
	return strings.Join(dnsServers, ",")
}
