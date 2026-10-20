package testutil

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShortTempDirExistsAndIsShort(t *testing.T) {
	dir := ShortTempDir(t)

	info, err := os.Stat(dir)
	require.NoError(t, err, "ShortTempDir must return an existing directory")
	require.True(t, info.IsDir(), "ShortTempDir must return a directory")

	// The path must be comfortably short so a socket beneath it fits
	// sockaddr_un.sun_path on every platform.
	assert.LessOrEqual(t, len(dir), 64, "temp dir must be short enough for a Unix socket path")
}

func TestShortTempDirCleansUp(t *testing.T) {
	var dir string
	t.Run("create", func(t *testing.T) {
		dir = ShortTempDir(t)
		_, err := os.Stat(dir)
		require.NoError(t, err)
	})

	// After the subtest (and its cleanup) completes the directory must be gone.
	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "ShortTempDir cleanup must remove the directory")
}

func TestRequireSockPathFits(t *testing.T) {
	RequireSockPathFits(t, "/tmp/ml-abcd/s.sock")
}
