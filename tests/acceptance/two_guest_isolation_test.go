//go:build acceptance

package acceptance

import (
	"context"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTwoGuestVFSWorkspacesAreIsolated launches two sandboxes concurrently and
// verifies that each guest's VFS workspace is strictly private: a file written
// in A's workspace is invisible to B, and vice versa. This is the end-to-end
// counterpart of the peer-CID isolation predicate: each sandbox's VFS server is
// per-sandbox (kernel-assigned vsock port + peer-CID filter), so a guest can
// only reach its OWN workspace, never a sibling's.
//
// The test is backend-agnostic (the VFS isolation mechanism is shared); on QEMU
// it additionally exercises the per-sandbox vsock port + peer-CID path.
func TestTwoGuestVFSWorkspacesAreIsolated(t *testing.T) {

	// Sandbox A: memory workspace with a file A only. Two concurrent QEMU TCG
	// boots are slower than the default 45s launch window, so grant a generous
	// per-launch timeout.
	clientA := launchWithBuilderTimeout(t, sdkNewWorkspace("alpine:latest"), 150*time.Second)

	// Sandbox B: memory workspace with a file B only.
	clientB := launchWithBuilderTimeout(t, sdkNewWorkspace("alpine:latest"), 150*time.Second)

	// launchWithBuilder already registers Close(0)+Remove() cleanup for both, so
	// do NOT defer them again here (double-release can deadlock teardown).

	// Genuinely two distinct, concurrently-live sandboxes (not the same VM twice).
	require.NotEqual(t, clientA.VMID(), clientB.VMID(),
		"the two sandboxes must be distinct VMs, else the isolation assertion is vacuous")

	// Exec context covering the read/write verifications (launches already done).
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A writes a unique marker and lists; B writes its own unique marker.
	_, err := clientA.Exec(ctx, "echo A_SECRET > /workspace/a.txt")
	require.NoError(t, err, "A writes its marker")

	_, err = clientB.Exec(ctx, "echo B_SECRET > /workspace/b.txt")
	require.NoError(t, err, "B writes its marker")

	// Each guest must see ONLY its own file and never the sibling's.
	lsA, err := clientA.Exec(ctx, "ls /workspace")
	require.NoError(t, err, "A lists workspace")
	assert.Contains(t, lsA.Stdout, "a.txt", "A must see its own file")
	assert.NotContains(t, lsA.Stdout, "b.txt", "A must NOT see B's file")

	lsB, err := clientB.Exec(ctx, "ls /workspace")
	require.NoError(t, err, "B lists workspace")
	assert.Contains(t, lsB.Stdout, "b.txt", "B must see its own file")
	assert.NotContains(t, lsB.Stdout, "a.txt", "B must NOT see A's file")

	// And each can read only its own content.
	catA, err := clientA.Exec(ctx, "cat /workspace/a.txt")
	require.NoError(t, err, "A reads its marker")
	assert.Contains(t, catA.Stdout, "A_SECRET")

	catB, err := clientB.Exec(ctx, "cat /workspace/b.txt")
	require.NoError(t, err, "B reads its marker")
	assert.Contains(t, catB.Stdout, "B_SECRET")
}

// sdkNewWorkspace builds a sandbox with a real workspace + a memory mount at
// /workspace, so guest writes land in the per-sandbox VFS namespace.
func sdkNewWorkspace(image string) *sdk.SandboxBuilder {
	return sdk.New(image).
		WithCPUs(acceptanceDefaultCPUs).
		WithWorkspace("/workspace").
		MountMemory("/workspace")
}
