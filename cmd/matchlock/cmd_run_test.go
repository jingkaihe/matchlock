package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDiskMountSpec(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "cache.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	got, err := parseDiskMountSpec(hostDisk + ":/var/lib/buildkit")
	require.NoError(t, err)
	assert.Equal(t, hostDisk, got.HostPath)
	assert.Equal(t, "/var/lib/buildkit", got.GuestMount)
	assert.False(t, got.ReadOnly)
}

func TestParseDiskMountSpecRelativeHostPath(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "data.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	oldWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	got, err := parseDiskMountSpec("data.ext4:/mnt/data:ro")
	require.NoError(t, err)
	expectedHostPath, err := filepath.EvalSymlinks(hostDisk)
	require.NoError(t, err)
	actualHostPath, err := filepath.EvalSymlinks(got.HostPath)
	require.NoError(t, err)
	assert.Equal(t, expectedHostPath, actualHostPath)
	assert.Equal(t, "/mnt/data", got.GuestMount)
	assert.True(t, got.ReadOnly)
}

func TestParseDiskMountSpecRejectsInvalidGuestMount(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "cache.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	_, err := parseDiskMountSpec(hostDisk + ":relative/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid guest mount path")
}

func TestParseDiskMountSpecRejectsUnknownOption(t *testing.T) {
	dir := t.TempDir()
	hostDisk := filepath.Join(dir, "cache.ext4")
	require.NoError(t, os.WriteFile(hostDisk, []byte("disk"), 0644))

	_, err := parseDiskMountSpec(hostDisk + ":/var/lib/buildkit:wat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown disk option")
}

func TestParseDiskMountSpecNamedVolume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	volumeDir := filepath.Join(home, ".cache", "matchlock", "volumes")
	require.NoError(t, os.MkdirAll(volumeDir, 0755))
	volumePath := filepath.Join(volumeDir, "cache.ext4")
	require.NoError(t, os.WriteFile(volumePath, []byte("disk"), 0644))

	got, err := parseDiskMountSpec("@cache:/var/lib/buildkit:ro")
	require.NoError(t, err)
	assert.Equal(t, volumePath, got.HostPath)
	assert.Equal(t, "/var/lib/buildkit", got.GuestMount)
	assert.True(t, got.ReadOnly)
}

func TestParseDiskMountSpecNamedVolumeMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := parseDiskMountSpec("@missing:/var/lib/buildkit")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "named volume not found")
}
