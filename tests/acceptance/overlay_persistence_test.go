//go:build acceptance

package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOverlayMountGuestWriteIsolatedFromSrc verifies the overlay snapshot
// contract: a guest write to an overlay-mounted host dir persists within that
// sandbox run (readable by the guest) but does NOT propagate to the source host
// dir (which stays byte-for-byte unchanged). This is the isolation guarantee of
// `-v host:guest` (default overlay) mounts and is the QEMU/Firecracker-shared
// overlay_snapshot backend path.
func TestOverlayMountGuestWriteIsolatedFromSrc(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "seed.txt"), []byte("seed"), 0644), "seed file")

	builder := sdk.New("alpine:latest").
		WithCPUs(acceptanceDefaultCPUs).
		WithWorkspace("/workspace").
		MountOverlay("/workspace/data", srcDir)

	client := launchWithBuilder(t, builder)
	// launchWithBuilder already registers Close(0)+Remove() cleanup; do NOT
	// defer them again here (double-release can deadlock teardown).

	// Guest reads the snapshot seed and writes a new file into the overlay.
	resRead, err := client.Exec(ctx, "cat /workspace/data/seed.txt")
	require.NoError(t, err, "guest reads seed")
	assert.Contains(t, resRead.Stdout, "seed", "guest sees the snapshot seed")

	resWrite, err := client.Exec(ctx, "echo NEW > /workspace/data/guest.txt && cat /workspace/data/guest.txt")
	require.NoError(t, err, "guest writes into overlay")
	assert.Contains(t, resWrite.Stdout, "NEW", "guest's overlay write must be readable back in the run")

	// The source host dir stays byte-for-byte unchanged: seed identical, guest.txt absent.
	_, err = os.Stat(filepath.Join(srcDir, "guest.txt"))
	assert.True(t, os.IsNotExist(err), "overlay write must NOT propagate to the source host dir (got %v)", err)
	seedData, err := os.ReadFile(filepath.Join(srcDir, "seed.txt"))
	require.NoError(t, err, "read src seed")
	assert.Equal(t, "seed", string(seedData), "source seed must be byte-identical")
	entries, err := os.ReadDir(srcDir)
	require.NoError(t, err, "read src dir")
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"seed.txt"}, names, "src dir must contain only the original seed")
}
