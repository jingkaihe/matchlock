package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/pkg/state"
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

func TestDiskMountShadowedByWorkspace(t *testing.T) {
	assert.True(t, diskMountShadowedByWorkspace("/workspace/cache", "/workspace"))
	assert.True(t, diskMountShadowedByWorkspace("/workspace", "/workspace"))
}

func TestDiskMountShadowedByWorkspaceOutsideWorkspace(t *testing.T) {
	assert.False(t, diskMountShadowedByWorkspace("/var/lib/buildkit", "/workspace"))
	assert.False(t, diskMountShadowedByWorkspace("/workspace-cache", "/workspace"))
	assert.False(t, diskMountShadowedByWorkspace("/var/lib/buildkit", ""))
}

func TestValidateDetachFlags(t *testing.T) {
	assert.NoError(t, validateDetachFlags(false, false, false))
	assert.NoError(t, validateDetachFlags(true, false, false))

	err := validateDetachFlags(true, true, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--detach cannot be combined")

	err = validateDetachFlags(true, false, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--detach cannot be combined")

	err = validateDetachFlags(true, true, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--detach cannot be combined")
}

func TestDetachedChildArgs(t *testing.T) {
	assert.Equal(
		t,
		[]string{"run", "--image", "alpine:latest", "-d", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "--image", "alpine:latest", "-d"}),
	)

	assert.Equal(
		t,
		[]string{"run", "--image", "alpine:latest", "-d", "--detach=false", "--rm=false", "--", "sh", "-c", "echo hi"},
		detachedChildArgs([]string{"run", "--image", "alpine:latest", "-d", "--", "sh", "-c", "echo hi"}),
	)

	assert.Equal(
		t,
		[]string{"run", "-dit", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "-dit"}),
	)

	assert.Equal(
		t,
		[]string{"run", "-itd", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "-itd"}),
	)

	assert.Equal(
		t,
		[]string{"run", "--image", "alpine:latest", "-w/tmp/data", "-eMODE=prod", "--detach=false", "--rm=false"},
		detachedChildArgs([]string{"run", "--image", "alpine:latest", "-w/tmp/data", "-eMODE=prod"}),
	)
}

func TestWaitDetachedVMIDFindsRunningState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	mgr := state.NewManager()
	vmID := "vm-detached-find"
	require.NoError(t, mgr.Register(vmID, map[string]any{"image": "alpine:latest"}))
	t.Cleanup(func() {
		_ = mgr.Unregister(vmID)
		_ = mgr.Remove(vmID)
	})

	got, err := waitDetachedVMID(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, vmID, got)
}

func TestWaitDetachedVMIDReturnsErrorWhenProcessIsNotRunning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	_, err := waitDetachedVMID(0)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFindDetachedVM)
}
