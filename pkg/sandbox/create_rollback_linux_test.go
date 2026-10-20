//go:build linux

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/kvm"
	"github.com/jingkaihe/matchlock/pkg/state"
	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installStubBackend is the Linux adapter for the shared create-rollback tests.
// It replaces the backend factory AND the two backend-selection probes, so no
// test ever opens /dev/kvm or /dev/vhost-vsock on the host.
func installStubBackend(t *testing.T) *stubBackend {
	t.Helper()
	b := &stubBackend{machine: &stubMachine{}}
	prevFactory := defaultVMBackend
	prevKVM := checkKVMAvailability
	prevQEMU := qemuFallbackAvailable
	defaultVMBackend = func(backendKind) vm.Backend { return b }
	checkKVMAvailability = func() kvm.Result { return kvm.Result{Status: kvm.StatusAvailable} }
	qemuFallbackAvailable = func() (bool, string) { return true, "" }
	t.Cleanup(func() {
		defaultVMBackend = prevFactory
		checkKVMAvailability = prevKVM
		qemuFallbackAvailable = prevQEMU
	})
	return b
}

// TestCreateSuccessStillRegistersVMRecord is the control: a successful create
// still registers its row and state directory, and no rollback fires.
func TestCreateSuccessStillRegistersVMRecord(t *testing.T) {
	const id = "vm-n5c-success"
	config, opts := noN5CCreateEnv(t, id)
	stubProvisioningSuccess(t)
	backend := installStubBackend(t)

	sb, err := New(context.Background(), config, opts)
	require.NoError(t, err)
	require.NotNil(t, sb)
	assert.Equal(t, 1, backend.createCalls)

	requireVMRecord(t, id, "running")

	mgr := state.NewManager()
	t.Cleanup(func() { _ = mgr.Remove(id) })
	require.NoError(t, sb.Close(context.Background()))
}

// TestCreateRanThenStoppedVMRowPreserved is the no-over-cleanup control: a VM
// that actually started and then stopped keeps its stopped row and its state
// directory. Close (not Stop) is the product path that turns a running sandbox
// into a stopped record, so this drives Start -> Close; the stub machine records
// the Start call so "ran" is observable rather than assumed. (Stop -> Close is
// deliberately not chained: the lifecycle state machine rejects Stopped ->
// Stopping, a pre-existing quirk unrelated to this change.)
func TestCreateRanThenStoppedVMRowPreserved(t *testing.T) {
	const id = "vm-n5c-ran-stopped"
	config, opts := noN5CCreateEnv(t, id)
	stubProvisioningSuccess(t)
	backend := installStubBackend(t)

	ctx := context.Background()
	sb, err := New(ctx, config, opts)
	require.NoError(t, err)
	require.NotNil(t, sb)

	require.NoError(t, sb.Start(ctx))
	require.True(t, backend.machine.started, "the VM must have actually started")

	require.NoError(t, sb.Close(ctx))
	require.True(t, backend.machine.stopped, "Close of a started sandbox must stop the machine")

	requireVMRecord(t, id, "stopped")

	mgr := state.NewManager()
	t.Cleanup(func() { _ = mgr.Remove(id) })
}

// TestCreateBackendCreateFailureKeepsStoppedRecord pins the rollback boundary:
// a failure AT backend.Create keeps the stopped row (the machine may have
// claimed host resources such as a persistent TAP, and the row is reconcile's
// only handle for them).
func TestCreateBackendCreateFailureKeepsStoppedRecord(t *testing.T) {
	const id = "vm-n5c-createfail"
	config, opts := noN5CCreateEnv(t, id)
	stubProvisioningSuccess(t)
	backend := installStubBackend(t)
	backend.createErr = errors.New("stub backend create failure")

	sb, err := New(context.Background(), config, opts)
	require.Error(t, err)
	require.Nil(t, sb)
	assert.Equal(t, 1, backend.createCalls)
	assert.ErrorIs(t, err, ErrCreateVM)

	requireVMRecord(t, id, "stopped")

	mgr := state.NewManager()
	t.Cleanup(func() { _ = mgr.Remove(id) })
}

// TestCreatePostMachineFailureKeepsRecordForReconcile covers a failure after
// the machine boundary (overlay snapshot prep runs after backend.Create on
// Linux): the machine is closed, and the stopped row plus state directory are
// kept so reconcile/gc can find any host resources.
func TestCreatePostMachineFailureKeepsRecordForReconcile(t *testing.T) {
	const id = "vm-n5c-postmachine"
	config, opts := noN5CCreateEnv(t, id)
	config.VFS = &api.VFSConfig{
		Workspace: "/workspace",
		Mounts: map[string]api.MountConfig{
			// An overlay mount without host_path fails snapshot prep.
			"/workspace/data": {Type: api.MountTypeOverlay, HostPath: ""},
		},
	}
	stubProvisioningSuccess(t)
	backend := installStubBackend(t)

	sb, err := New(context.Background(), config, opts)
	require.Error(t, err)
	require.Nil(t, sb)
	assert.ErrorIs(t, err, ErrPrepareOverlayMount)
	assert.Equal(t, 1, backend.createCalls)
	assert.True(t, backend.machine.closed, "post-boundary cleanup must close the machine")

	requireVMRecord(t, id, "stopped")

	mgr := state.NewManager()
	t.Cleanup(func() { _ = mgr.Remove(id) })
}
