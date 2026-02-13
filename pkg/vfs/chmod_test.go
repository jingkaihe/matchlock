package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryProvider_Chmod_File(t *testing.T) {
	mp := NewMemoryProvider()
	h, _ := mp.Create("/file.txt", 0644)
	h.Close()

	require.NoError(t, mp.Chmod("/file.txt", 0755))

	info, _ := mp.Stat("/file.txt")
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

func TestMemoryProvider_Chmod_Dir(t *testing.T) {
	mp := NewMemoryProvider()
	mp.Mkdir("/dir", 0755)

	require.NoError(t, mp.Chmod("/dir", 0700))

	info, _ := mp.Stat("/dir")
	assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
}

func TestMemoryProvider_Chmod_DirDefaultMode(t *testing.T) {
	mp := NewMemoryProvider()
	mp.Mkdir("/dir", 0755)

	info, _ := mp.Stat("/dir")
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

func TestMemoryProvider_Chmod_NonExistent(t *testing.T) {
	mp := NewMemoryProvider()
	err := mp.Chmod("/nope", 0644)
	require.Error(t, err)
}

func TestMemoryProvider_Chmod_PreservesAfterRename(t *testing.T) {
	mp := NewMemoryProvider()
	mp.Mkdir("/src", 0755)
	mp.Chmod("/src", 0700)
	mp.Mkdir("/dst", 0755)

	mp.Rename("/src", "/renamed")

	info, _ := mp.Stat("/renamed")
	assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
}

func TestMemoryProvider_Mkdir_StoresMode(t *testing.T) {
	mp := NewMemoryProvider()
	mp.Mkdir("/dir", 0700)

	info, _ := mp.Stat("/dir")
	assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
}

func TestOverlayProvider_Chmod_UpperFile(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()
	h, _ := upper.Create("/file.txt", 0644)
	h.Close()

	overlay := NewOverlayProvider(upper, lower)
	require.NoError(t, overlay.Chmod("/file.txt", 0755))

	info, _ := upper.Stat("/file.txt")
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

func TestOverlayProvider_Chmod_CopiesUpFromLower(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	h, _ := lower.Create("/file.txt", 0644)
	h.Write([]byte("lower content"))
	h.Close()

	overlay := NewOverlayProvider(upper, lower)
	require.NoError(t, overlay.Chmod("/file.txt", 0755))

	// File should now exist in upper with new mode
	info, err := upper.Stat("/file.txt")
	require.NoError(t, err, "file should exist in upper after copy-up")
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}

func TestOverlayProvider_Chmod_NonExistent(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()
	overlay := NewOverlayProvider(upper, lower)

	err := overlay.Chmod("/nope", 0644)
	require.Error(t, err)
}

func TestRealFSProvider_Chmod(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)

	f, err := os.Create(filepath.Join(dir, "file.txt"))
	require.NoError(t, err)
	f.Close()

	require.NoError(t, p.Chmod("/file.txt", 0700))

	info, _ := p.Stat("/file.txt")
	assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
}

func TestRealFSProvider_Chmod_Dir(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)

	os.Mkdir(filepath.Join(dir, "subdir"), 0755)

	require.NoError(t, p.Chmod("/subdir", 0700))

	info, _ := p.Stat("/subdir")
	assert.Equal(t, os.FileMode(0700), info.Mode().Perm())
}

func TestRealFSProvider_Chmod_NonExistent(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)

	err := p.Chmod("/nope", 0644)
	require.Error(t, err)
}

func TestReadonlyProvider_Chmod_Blocked(t *testing.T) {
	base := NewMemoryProvider()
	h, _ := base.Create("/file.txt", 0644)
	h.Close()

	ro := NewReadonlyProvider(base)
	err := ro.Chmod("/file.txt", 0755)
	require.Error(t, err)
}

func TestRouterProvider_Chmod(t *testing.T) {
	mp := NewMemoryProvider()
	h, _ := mp.Create("/file.txt", 0644)
	h.Close()

	router := NewMountRouter(map[string]Provider{"/mnt": mp})

	require.NoError(t, router.Chmod("/mnt/file.txt", 0755))

	info, _ := mp.Stat("/file.txt")
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm())
}
