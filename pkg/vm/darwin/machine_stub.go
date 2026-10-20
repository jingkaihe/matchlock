//go:build !(darwin && cgo)

package darwin

import (
	"context"
	"net"
	"os"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/vm"
)

// DarwinMachine is the placeholder machine type for builds that cannot link
// the Virtualization.framework bindings. `pkg/sandbox` compiles on every
// darwin build and asserts `vm.Machine` to this concrete type, so the
// placeholder must satisfy the full VM contract; every operation reports
// ErrVZUnavailable. A real machine can never be produced in this
// configuration: Create always errors, so no code path can hold a stub
// machine that pretends to work.
type DarwinMachine struct{}

// Compile-time proof the placeholder satisfies the same interface as the real
// machine, on every platform this file compiles for.
var _ vm.Machine = (*DarwinMachine)(nil)

func (m *DarwinMachine) Start(ctx context.Context) error { return ErrVZUnavailable }
func (m *DarwinMachine) Stop(ctx context.Context) error  { return ErrVZUnavailable }
func (m *DarwinMachine) Wait(ctx context.Context) error  { return ErrVZUnavailable }

func (m *DarwinMachine) Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error) {
	return nil, ErrVZUnavailable
}

func (m *DarwinMachine) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return ErrVZUnavailable
}

func (m *DarwinMachine) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return nil, ErrVZUnavailable
}

func (m *DarwinMachine) ListFiles(ctx context.Context, path string) ([]api.FileInfo, error) {
	return nil, ErrVZUnavailable
}

// NetworkFD reports that this build has no interception network file; the
// sandbox treats a nil NetworkFile as a hard error on the interception path
// long before any FD could be handed out.
func (m *DarwinMachine) NetworkFD() (int, error) { return -1, ErrVZUnavailable }

// NetworkFile reports that this build has no interception network file.
func (m *DarwinMachine) NetworkFile() *os.File { return nil }

func (m *DarwinMachine) VsockFD() (int, error)           { return -1, ErrVZUnavailable }
func (m *DarwinMachine) PID() int                        { return 0 }
func (m *DarwinMachine) Close(ctx context.Context) error { return nil }
func (m *DarwinMachine) RootfsPath() string              { return "" }

// SetupVFSListener reports that this build cannot serve the VFS listener.
func (m *DarwinMachine) SetupVFSListener() (net.Listener, error) { return nil, ErrVZUnavailable }
