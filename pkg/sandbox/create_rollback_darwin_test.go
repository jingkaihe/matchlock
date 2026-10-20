//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installStubBackend is the Darwin adapter for the shared create-rollback
// tests. It replaces the backend constructor so no test reaches
// Virtualization.framework.
func installStubBackend(t *testing.T) *stubBackend {
	t.Helper()
	b := &stubBackend{machine: &stubMachine{}}
	prev := newDarwinVMBackend
	newDarwinVMBackend = func() vm.Backend { return b }
	t.Cleanup(func() { newDarwinVMBackend = prev })
	return b
}

// TestDarwinBackendCreateFailureKeepsStoppedRecord pins the rollback boundary
// on the Darwin path: a failure AT backend.Create keeps the stopped row and the
// state directory, mirroring the Linux boundary test. (The Darwin success path
// cannot be driven by the stub machine - New() type-asserts the concrete
// *darwin.DarwinMachine - so it needs the authorized native Mac gate; it is NOT
// covered here and NOT claimed as verified.)
func TestDarwinBackendCreateFailureKeepsStoppedRecord(t *testing.T) {
	const id = "vm-n5c-darwin-createfail"
	config, opts := noN5CCreateEnv(t, id)
	stubProvisioningSuccess(t)
	backend := installStubBackend(t)
	backend.createErr = errors.New("stub backend create failure")

	sb, err := New(context.Background(), config, opts)
	require.Error(t, err)
	require.Nil(t, sb)
	assert.Equal(t, 1, backend.createCalls)

	requireVMRecord(t, id, "stopped")
}
