package vm

import (
	"context"
	"io"

	"github.com/jingkaihe/matchlock/pkg/api"
)

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
	GatewayIP       string // Host TAP IP (e.g., 192.168.100.1)
	GuestIP         string // Guest IP (e.g., 192.168.100.2)
	SubnetCIDR      string // CIDR notation (e.g., 192.168.100.1/24)
	Workspace       string // Guest VFS mount point (default: /workspace)
	UseInterception bool   // Use network interception (MITM proxy)
	Privileged      bool   // Skip in-guest security restrictions (seccomp, cap drop, no_new_privs)
	PrebuiltRootfs  string // Pre-prepared rootfs path (skips internal copy if set)
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
	Close() error
	RootfsPath() string // Returns the path to the VM's rootfs (may be a temp copy)
}

type InteractiveMachine interface {
	Machine
	ExecInteractive(ctx context.Context, command string, opts *api.ExecOptions, rows, cols uint16, stdin io.Reader, stdout io.Writer, resizeCh <-chan [2]uint16) (int, error)
}
