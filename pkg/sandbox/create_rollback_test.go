package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the real New() create flow through the platform seams
// (backend factory + disk-provisioning helpers + virtualization probes, wired
// per platform in create_rollback_linux_test.go / create_rollback_darwin_test.go).
// They exercise constructor and rollback semantics; they never boot a VM, and
// they mutate package-level seams, so they must not run in parallel with each
// other and carry no t.Parallel().

// stubMachine is a do-nothing vm.Machine used to drive the create flow past the
// backend machine boundary without a hypervisor. It records the lifecycle calls
// so tests can distinguish "registered" from "actually started".
type stubMachine struct {
	started bool
	stopped bool
	closed  bool
}

var _ vm.Machine = (*stubMachine)(nil)

func (m *stubMachine) Start(context.Context) error { m.started = true; return nil }
func (m *stubMachine) Stop(context.Context) error  { m.stopped = true; return nil }
func (m *stubMachine) Wait(context.Context) error  { return nil }
func (m *stubMachine) Exec(context.Context, string, *api.ExecOptions) (*api.ExecResult, error) {
	return &api.ExecResult{ExitCode: 0}, nil
}
func (m *stubMachine) WriteFile(context.Context, string, []byte, uint32) error { return nil }
func (m *stubMachine) ReadFile(context.Context, string) ([]byte, error)        { return nil, nil }
func (m *stubMachine) ListFiles(context.Context, string) ([]api.FileInfo, error) {
	return nil, nil
}
func (m *stubMachine) NetworkFD() (int, error) { return 0, nil }
func (m *stubMachine) VsockFD() (int, error)   { return 0, nil }
func (m *stubMachine) PID() int                { return 0 }

// Close mirrors the real backends: closing a started machine stops it first.
func (m *stubMachine) Close(context.Context) error {
	if m.started {
		m.stopped = true
	}
	m.closed = true
	return nil
}
func (m *stubMachine) RootfsPath() string { return "" }
func (m *stubMachine) TapName() string    { return "" }

// stubBackend stands in for the platform VM backend. createCalls counts
// invocations of Create specifically (the factory may legitimately be invoked
// before later provisioning fails).
type stubBackend struct {
	createCalls int
	createErr   error
	machine     *stubMachine
}

func (b *stubBackend) Create(context.Context, *vm.VMConfig) (vm.Machine, error) {
	b.createCalls++
	if b.createErr != nil {
		return nil, b.createErr
	}
	return b.machine, nil
}
func (b *stubBackend) Name() string { return "stub" }

// noN5CCreateEnv isolates the test into a private HOME and returns the minimal
// no-network config/opts pair. No-network keeps the create flow free of
// subnet/proxy/firewall/TAP host resources.
func noN5CCreateEnv(t *testing.T, id string) (*api.Config, *Options) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MATCHLOCK_BACKEND", "") // never let an exported override pick a backend
	config := &api.Config{
		ID:      id,
		Image:   "alpine:3.22",
		Network: &api.NetworkConfig{NoNetwork: true},
		Resources: &api.Resources{
			CPUs:       api.DefaultCPUs,
			MemoryMB:   api.DefaultMemoryMB,
			DiskSizeMB: 64,
		},
	}
	// KernelPath short-circuits kernel resolution, so no download or cache
	// access is possible.
	opts := &Options{
		RootfsPaths: []string{"/nonexistent/lower.erofs"},
		KernelPath:  "/nonexistent/kernel",
	}
	return config, opts
}

// stubProvisioningSuccess replaces the disk-provisioning helpers with trivial
// ones so create-flow tests are hermetic: no mke2fs/debugfs/guest-init needed.
func stubProvisioningSuccess(t *testing.T) {
	t.Helper()
	prevBootstrap := createBootstrapRootfsFn
	prevExt4 := createExt4ImageFn
	prevUpper := prepareOverlayUpperRootfsFn
	createBootstrapRootfsFn = func(path string) error { return os.WriteFile(path, []byte("stub"), 0600) }
	createExt4ImageFn = func(path string, _ int64) error { return os.WriteFile(path, []byte("stub"), 0600) }
	prepareOverlayUpperRootfsFn = func(string) error { return nil }
	t.Cleanup(func() {
		createBootstrapRootfsFn = prevBootstrap
		createExt4ImageFn = prevExt4
		prepareOverlayUpperRootfsFn = prevUpper
	})
}

// requireNoVMRecord asserts there is no registry row and no state directory for
// id, distinguishing genuine absence from a broken state database.
func requireNoVMRecord(t *testing.T, id string) {
	t.Helper()
	mgr := state.NewManager()

	states, err := mgr.List()
	require.NoError(t, err, "state database must be readable after a failed create")
	for _, st := range states {
		require.NotEqual(t, id, st.ID, "failed create must not leave a registry row")
	}
	_, err = mgr.Get(id)
	require.Error(t, err, "failed create must not leave a fetchable registry row")
	_, err = os.Stat(mgr.Dir(id))
	require.True(t, os.IsNotExist(err), "failed create must not leave a VM state directory")
}

// requireVMRecord asserts a registry row exists with the given status and that
// its state directory survived.
func requireVMRecord(t *testing.T, id, status string) {
	t.Helper()
	mgr := state.NewManager()
	st, err := mgr.Get(id)
	require.NoError(t, err, "expected a registry row for %s", id)
	assert.Equal(t, status, st.Status)
	_, err = os.Stat(mgr.Dir(id))
	require.NoError(t, err, "expected the state directory for %s to survive", id)
}

// registerOtherVM registers an unrelated VM so tests can prove rollback is
// scoped to the failed attempt.
func registerOtherVM(t *testing.T, id string) {
	t.Helper()
	mgr := state.NewManager()
	require.NoError(t, mgr.Register(id, map[string]string{"image": "alpine:3.22"}))
}

func requireOtherVMPreserved(t *testing.T, id string) {
	t.Helper()
	mgr := state.NewManager()
	_, err := mgr.Get(id)
	require.NoError(t, err, "rollback must not touch unrelated VM %s", id)
}

// TestCreateRollbackBootstrapFailureLeavesNoVMRecord drives an injected
// bootstrap-provisioning failure through the real create flow and asserts the
// failed attempt leaves no VM record behind, never reaches the backend, and
// returns the original error unchanged.
func TestCreateRollbackBootstrapFailureLeavesNoVMRecord(t *testing.T) {
	const id = "vm-n5c-bootstrap"
	config, opts := noN5CCreateEnv(t, id)
	registerOtherVM(t, "vm-n5c-bystander")

	backend := installStubBackend(t)
	injected := errors.New("injected bootstrap failure")
	reached := false
	prev := createBootstrapRootfsFn
	createBootstrapRootfsFn = func(string) error {
		reached = true
		return injected
	}
	t.Cleanup(func() { createBootstrapRootfsFn = prev })

	sb, err := New(context.Background(), config, opts)
	require.Error(t, err)
	require.Nil(t, sb)

	assert.True(t, reached, "injected bootstrap seam must be reached")
	assert.Equal(t, 0, backend.createCalls, "backend machine must never be created")
	assert.ErrorIs(t, err, ErrPrepareBootstrapRoot, "original bootstrap error must be returned")
	assert.ErrorIs(t, err, injected, "the injected cause must remain visible")
	assert.NotErrorIs(t, err, ErrRemoveFailedCreate, "rollback itself must not have failed")

	requireNoVMRecord(t, id)
	requireOtherVMPreserved(t, "vm-n5c-bystander")
}

// TestCreateRollbackRealBootstrapFailureIsRolledBack exercises the REAL
// createBootstrapRootfs failure path (no seam injection) by emptying PATH so
// the ext4 formatter lookup inside createExt4Image cannot succeed. This is the
// exact failure shape of the reported defect.
func TestCreateRollbackRealBootstrapFailureIsRolledBack(t *testing.T) {
	const id = "vm-n5c-bootstrap-real"
	config, opts := noN5CCreateEnv(t, id)
	backend := installStubBackend(t)
	// No stubProvisioningSuccess: this test deliberately runs the production
	// provisioning path. An empty PATH makes exec.LookPath("mke2fs") fail
	// deterministically regardless of what the host has installed.
	t.Setenv("PATH", "")

	sb, err := New(context.Background(), config, opts)
	require.Error(t, err)
	require.Nil(t, sb)

	assert.Equal(t, 0, backend.createCalls, "backend machine must never be created")
	assert.ErrorIs(t, err, ErrPrepareBootstrapRoot, "original bootstrap error must be returned")

	requireNoVMRecord(t, id)
}

// TestCreateRollbackPreMachineExtraDiskFailureLeavesNoRecord covers a second
// pre-machine failure class (extra-disk validation) and asserts the same
// rollback.
func TestCreateRollbackPreMachineExtraDiskFailureLeavesNoRecord(t *testing.T) {
	const id = "vm-n5c-extradisk"
	config, opts := noN5CCreateEnv(t, id)
	config.ExtraDisks = []api.DiskMount{{
		HostPath:   "/nonexistent/pgdata.ext4",
		GuestMount: "not-absolute", // rejected by ValidateGuestMount
	}}
	stubProvisioningSuccess(t)
	backend := installStubBackend(t)

	sb, err := New(context.Background(), config, opts)
	require.Error(t, err)
	require.Nil(t, sb)

	assert.Equal(t, 0, backend.createCalls, "backend machine must never be created")
	assert.ErrorIs(t, err, ErrInvalidDiskCfg)

	requireNoVMRecord(t, id)
}

// TestCreateRollbackHelperSkippedOnSuccessAndOnCommit pins the two
// non-destructive paths of the rollback helper.
func TestCreateRollbackHelperSkippedOnSuccessAndOnCommit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mgr := state.NewManager()
	const id = "vm-n5c-helper"
	require.NoError(t, mgr.Register(id, map[string]string{"image": "alpine:3.22"}))
	t.Cleanup(func() { _ = mgr.Unregister(id); _ = mgr.Remove(id) })

	t.Run("nil error does nothing", func(t *testing.T) {
		rb := newCreateRollback(mgr, id)
		require.NoError(t, rb.run(nil))
		_, err := mgr.Get(id)
		require.NoError(t, err, "row must survive a successful create")
	})

	t.Run("committed failure keeps the row", func(t *testing.T) {
		rb := newCreateRollback(mgr, id)
		rb.commit()
		createErr := errors.New("post-machine failure")
		require.ErrorIs(t, rb.run(createErr), createErr)
		_, err := mgr.Get(id)
		require.NoError(t, err, "row must survive a failure after the machine boundary")
	})

	t.Run("uncommitted failure removes row, directory and subnet allocation", func(t *testing.T) {
		alloc := state.NewSubnetAllocator()
		_, err := alloc.Allocate(id)
		require.NoError(t, err)

		rb := newCreateRollback(mgr, id)
		createErr := errors.New("pre-machine failure")
		got := rb.run(createErr)
		require.ErrorIs(t, got, createErr, "original error must survive rollback")
		require.NotErrorIs(t, got, ErrRemoveFailedCreate, "rollback should not have failed")

		requireNoVMRecord(t, id)
		after := state.NewSubnetAllocator()
		_, err = after.Get(id)
		require.Error(t, err, "rollback must clear the subnet allocation")
	})
}

// TestCreateRollbackHelperCleanupFailureJoinsOriginalError proves a failing
// rollback reports both errors without masking the original create error.
func TestCreateRollbackHelperCleanupFailureJoinsOriginalError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0600))
	// A manager whose base dir cannot be opened (its parent is a regular file)
	// makes the rollback's first step fail, so the test exercises the
	// report-both-errors contract without needing to engineer a failure inside
	// the state database itself.
	mgr := state.NewManagerWithDir(filepath.Join(blocker, "vms"))

	rb := newCreateRollback(mgr, "vm-n5c-broken")
	createErr := errors.New("original create failure")
	got := rb.run(createErr)

	require.Error(t, got)
	assert.ErrorIs(t, got, createErr, "original create error must not be masked")
	assert.ErrorIs(t, got, ErrRemoveFailedCreate, "rollback failure must be reported too")
	assert.Contains(t, got.Error(), createErr.Error())
	assert.NotEqual(t, createErr, got)
}
