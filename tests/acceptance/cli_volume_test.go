//go:build acceptance

package acceptance

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIRunDiskMountNamedVolumePersists(t *testing.T) {
	requireVolumeFormatTool(t)

	volumeName := uniqueVolumeName("acc-disk")
	t.Cleanup(func() {
		_, _, _ = runCLI(t, "volume", "rm", volumeName)
	})

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"volume", "create", volumeName, "--size", "32",
	)
	require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)

	stdout, stderr, exitCode = runCLIWithTimeout(
		t,
		3*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--no-network",
		"--disk", "@"+volumeName+":/foo",
		"--", "sh", "-c", "echo persisted > /foo/abc",
	)
	require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)

	stdout, stderr, exitCode = runCLIWithTimeout(
		t,
		3*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--no-network",
		"--disk", "@"+volumeName+":/foo",
		"cat", "/foo/abc",
	)
	require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Equal(t, "persisted", strings.TrimSpace(stdout))
}

func TestCLIVolumeCreateLsRm(t *testing.T) {
	requireVolumeFormatTool(t)

	volumeName := uniqueVolumeName("acc-volume")
	t.Cleanup(func() {
		_, _, _ = runCLI(t, "volume", "rm", volumeName)
	})

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"volume", "create", volumeName, "--size", "16",
	)
	require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "Created volume "+volumeName)
	assert.NotContains(t, stdout, "Path:")

	stdout, stderr, exitCode = runCLIWithTimeout(
		t,
		2*time.Minute,
		"volume", "ls",
	)
	require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "NAME")
	assert.Contains(t, stdout, volumeName)
	assert.NotContains(t, stdout, "PATH")

	stdout, stderr, exitCode = runCLIWithTimeout(
		t,
		2*time.Minute,
		"volume", "rm", volumeName,
	)
	require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "Removed volume "+volumeName)

	stdout, stderr, exitCode = runCLIWithTimeout(
		t,
		2*time.Minute,
		"volume", "ls",
	)
	require.Equalf(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.NotContains(t, stdout, volumeName)

	_, stderr, exitCode = runCLIWithTimeout(
		t,
		2*time.Minute,
		"volume", "rm", volumeName,
	)
	require.NotEqual(t, 0, exitCode)
	assert.Contains(t, stderr, "named volume not found")
}

func requireVolumeFormatTool(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		if _, err := exec.LookPath("mke2fs"); err != nil {
			t.Skip("mkfs.ext4/mke2fs not found")
		}
	}
}

func uniqueVolumeName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
