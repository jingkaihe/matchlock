package kernel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurrentArch(t *testing.T) {
	arch := CurrentArch()
	assert.Contains(t, []Architecture{ArchX86_64, ArchARM64}, arch)
}

func TestKernelFilename(t *testing.T) {
	tests := []struct {
		arch     Architecture
		expected string
	}{
		{ArchX86_64, "kernel"},
		{ArchARM64, "kernel-arm64"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, tt.arch.KernelFilename())
	}
}

func TestOCIPlatform(t *testing.T) {
	tests := []struct {
		arch     Architecture
		expected string
	}{
		{ArchX86_64, "linux/amd64"},
		{ArchARM64, "linux/arm64"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, tt.arch.OCIPlatform())
	}
}

func TestManagerKernelPath(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(WithCacheDir(tmpDir))

	path := mgr.KernelPath(ArchX86_64, "6.1.137")
	expected := filepath.Join(tmpDir, "kernels", "6.1.137", "kernel")
	assert.Equal(t, expected, path)

	path = mgr.KernelPath(ArchARM64, "")
	expected = filepath.Join(tmpDir, "kernels", Version, "kernel-arm64")
	assert.Equal(t, expected, path)
}

func TestListCachedVersions(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(WithCacheDir(tmpDir))

	versions, err := mgr.ListCachedVersions()
	require.NoError(t, err)
	assert.Len(t, versions, 0)

	os.MkdirAll(filepath.Join(tmpDir, "kernels", "6.1.137"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "kernels", "6.1.140"), 0755)

	versions, err = mgr.ListCachedVersions()
	require.NoError(t, err)
	assert.Len(t, versions, 2)
}

func TestImageReference(t *testing.T) {
	ref := ImageReference("")
	assert.Equal(t, DefaultRegistry+"/kernel:"+Version, ref)

	ref = ImageReference("6.1.140")
	assert.Equal(t, DefaultRegistry+"/kernel:6.1.140", ref)
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		ref      string
		expected string
	}{
		{"ghcr.io/jingkaihe/matchlock/kernel:6.1.137", "6.1.137"},
		{"kernel:6.1.140", "6.1.140"},
		{"kernel", Version},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.expected, ParseVersion(tt.ref))
	}
}
