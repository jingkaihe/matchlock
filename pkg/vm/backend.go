package vm

import (
	"context"

	"github.com/jingkaihe/matchlock/pkg/api"
)

type VMConfig struct {
	ID           string
	KernelPath   string
	RootfsPath   string
	CPUs         int
	MemoryMB     int
	NetworkFD    int
	VsockCID     uint32
	VsockPath    string
	SocketPath   string
	LogPath      string
	KernelArgs   string
	Env          map[string]string
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
}
