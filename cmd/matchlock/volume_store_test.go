package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateVolumeName(t *testing.T) {
	require.NoError(t, validateVolumeName("cache"))
	require.NoError(t, validateVolumeName("cache-1"))
	require.NoError(t, validateVolumeName("cache_1"))

	require.Error(t, validateVolumeName(""))
	require.Error(t, validateVolumeName("bad name"))
	require.Error(t, validateVolumeName("-bad"))
	require.Error(t, validateVolumeName("bad/name"))
}

func TestVolumePathForNameTrimsWhitespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	trimmed, err := volumePathForName("cache")
	require.NoError(t, err)
	padded, err := volumePathForName(" cache ")
	require.NoError(t, err)
	assert.Equal(t, trimmed, padded)
}

func TestListNamedVolumes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(volumeDir, "b.ext4"), []byte("b"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(volumeDir, "a.ext4"), []byte("aa"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(volumeDir, "ignore.txt"), []byte("x"), 0644))

	vols, err := listNamedVolumes()
	require.NoError(t, err)
	require.Len(t, vols, 2)
	assert.Equal(t, "a", vols[0].Name)
	assert.Equal(t, "b", vols[1].Name)
	assert.Equal(t, int64(2), vols[0].SizeBytes)
	assert.Equal(t, int64(1), vols[1].SizeBytes)
}

func TestCreateNamedVolume(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		if _, err := exec.LookPath("mke2fs"); err != nil {
			t.Skip("mkfs.ext4/mke2fs not found")
		}
	}

	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := createNamedVolume("cache", 16)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(16*1024*1024), info.Size())
}

func TestCreateNamedVolumeExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	path := filepath.Join(volumeDir, "cache.ext4")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0644))

	_, err := createNamedVolume("cache", 16)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVolumeExists)
}

func TestRemoveNamedVolume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	path := filepath.Join(volumeDir, "cache.ext4")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0644))

	require.NoError(t, removeNamedVolume("cache"))
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRemoveNamedVolumeNotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := removeNamedVolume("missing")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVolumeNotFound)
}

func TestRemoveNamedVolumeTrimsWhitespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	path := filepath.Join(volumeDir, "cache.ext4")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0644))

	require.NoError(t, removeNamedVolume(" cache "))
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCopyNamedVolume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	srcPath := filepath.Join(volumeDir, "src.ext4")
	require.NoError(t, os.WriteFile(srcPath, []byte("volume-bytes"), 0600))
	require.NoError(t, os.Chmod(srcPath, 0600))

	require.NoError(t, copyNamedVolume("src", "dest"))

	destPath := filepath.Join(volumeDir, "dest.ext4")
	destBytes, err := os.ReadFile(destPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("volume-bytes"), destBytes)

	srcInfo, err := os.Stat(srcPath)
	require.NoError(t, err)
	destInfo, err := os.Stat(destPath)
	require.NoError(t, err)
	assert.Equal(t, srcInfo.Mode().Perm(), destInfo.Mode().Perm())
	assert.WithinDuration(t, srcInfo.ModTime(), destInfo.ModTime(), 2*time.Second)

	srcStat, srcOK := srcInfo.Sys().(*syscall.Stat_t)
	destStat, destOK := destInfo.Sys().(*syscall.Stat_t)
	require.True(t, srcOK)
	require.True(t, destOK)
	assert.Equal(t, srcStat.Uid, destStat.Uid)
	assert.Equal(t, srcStat.Gid, destStat.Gid)
}

func TestCopyNamedVolumeSourceMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := copyNamedVolume("missing", "dest")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVolumeNotFound)
}

func TestCopyNamedVolumeDestinationExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(volumeDir, "src.ext4"), []byte("source"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(volumeDir, "dest.ext4"), []byte("dest"), 0644))

	err := copyNamedVolume("src", "dest")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVolumeExists)
}

func TestCopyNamedVolumeSameSourceAndDestination(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(volumeDir, "src.ext4"), []byte("source"), 0644))

	err := copyNamedVolume("src", "src")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCopyVolume)
}

func TestCopyNamedVolumeTrimsWhitespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(volumeDir, "src.ext4"), []byte("source"), 0644))

	require.NoError(t, copyNamedVolume(" src ", " dest "))
	_, err := os.Stat(filepath.Join(volumeDir, "dest.ext4"))
	require.NoError(t, err)
}
