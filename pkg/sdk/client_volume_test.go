package sdk

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVolumeCreate(t *testing.T) {
	bin := writeFakeMatchlockCLI(t, `if [ "$1" = "volume" ] && [ "$2" = "create" ]; then
	echo "Created volume $3 ($5 MB)"
	echo "Path: /tmp/$3.ext4"
	exit 0
fi
exit 1
`)
	client := &Client{binaryPath: bin}

	info, err := client.VolumeCreate("cache", 32)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "cache", info.Name)
	assert.Equal(t, "32.0 MB", info.Size)
	assert.Equal(t, "/tmp/cache.ext4", info.Path)
}

func TestVolumeList(t *testing.T) {
	bin := writeFakeMatchlockCLI(t, `if [ "$1" = "volume" ] && [ "$2" = "ls" ]; then
	echo "NAME  SIZE  PATH"
	echo "cache 16.0 MB /tmp/cache.ext4"
	echo "data  32.0 MB /tmp/data.ext4"
	exit 0
fi
exit 1
`)
	client := &Client{binaryPath: bin}

	vols, err := client.VolumeList()
	require.NoError(t, err)
	require.Len(t, vols, 2)
	assert.Equal(t, VolumeInfo{Name: "cache", Size: "16.0 MB", Path: "/tmp/cache.ext4"}, vols[0])
	assert.Equal(t, VolumeInfo{Name: "data", Size: "32.0 MB", Path: "/tmp/data.ext4"}, vols[1])
}

func TestVolumeRemove(t *testing.T) {
	bin := writeFakeMatchlockCLI(t, `if [ "$1" = "volume" ] && [ "$2" = "rm" ] && [ "$3" = "cache" ]; then
	exit 0
fi
exit 1
`)
	client := &Client{binaryPath: bin}

	require.NoError(t, client.VolumeRemove("cache"))
}

func TestParseVolumeCreatePathMissingPath(t *testing.T) {
	_, err := parseVolumeCreatePath("Created volume cache (32 MB)\n")
	require.ErrorIs(t, err, ErrParseVolumeCreateResult)
}

func TestParseVolumeListOutputInvalidLine(t *testing.T) {
	_, err := parseVolumeListOutput("NAME SIZE PATH\ninvalid-line\n")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrParseVolumeListResult)
}

func TestParseVolumeListOutputPathWithSpaces(t *testing.T) {
	vols, err := parseVolumeListOutput("NAME SIZE PATH\ncache 16.0 MB /Users/Jane Doe/.cache/matchlock/volumes/cache.ext4\n")
	require.NoError(t, err)
	require.Len(t, vols, 1)
	assert.Equal(t, VolumeInfo{
		Name: "cache",
		Size: "16.0 MB",
		Path: "/Users/Jane Doe/.cache/matchlock/volumes/cache.ext4",
	}, vols[0])
}

func TestVolumeCommandRequiresBinaryPath(t *testing.T) {
	client := &Client{}
	_, err := client.VolumeList()
	require.ErrorIs(t, err, ErrBinaryPathRequired)
}

func writeFakeMatchlockCLI(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "matchlock")
	script := "#!/bin/sh\nset -eu\n" + body
	require.NoError(t, os.WriteFile(path, []byte(script), 0755))
	return path
}
