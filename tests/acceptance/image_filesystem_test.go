//go:build acceptance

package acceptance

import (
	"context"
	"strings"
	"testing"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageSymlinksPreserved(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "readlink /bin/sh")
	require.NoError(t, err, "Exec")
	got := strings.TrimSpace(result.Stdout)
	assert.Contains(t, got, "busybox")
}

func TestImageSymlinksInLib(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "ls -la / | grep '^l' | head -5")
	require.NoError(t, err, "Exec")
	assert.Equal(t, 0, result.ExitCode)
}

func TestPythonImageSymlinks(t *testing.T) {
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "readlink /usr/local/bin/python3")
	require.NoError(t, err, "Exec")
	got := strings.TrimSpace(result.Stdout)
	assert.Contains(t, got, "python")
}

func TestImageFileOwnershipRoot(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "stat -c '%u:%g' /etc/passwd")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "0:0", strings.TrimSpace(result.Stdout))
}

func TestImageFileOwnershipNonRoot(t *testing.T) {
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "stat -c '%u:%g' /etc/shadow")
	require.NoError(t, err, "Exec")
	got := strings.TrimSpace(result.Stdout)
	uid := strings.Split(got, ":")[0]
	assert.Equal(t, "0", uid)
}

func TestPythonImageOwnership(t *testing.T) {
	builder := sdk.New("python:3.12-alpine")
	client := launchWithBuilder(t, builder)

	result, err := client.Exec(context.Background(), "stat -c '%u:%g' /usr/local/bin/python3.12")
	require.NoError(t, err, "Exec")
	assert.Equal(t, "0:0", strings.TrimSpace(result.Stdout))
}

func TestImageFilePermissions(t *testing.T) {
	client := launchAlpine(t)

	tests := []struct {
		path string
		mode string
	}{
		{"/etc/passwd", "644"},
		{"/etc/shadow", "640"},
		{"/bin/busybox", "755"},
	}

	for _, tc := range tests {
		result, err := client.Exec(context.Background(), "stat -c '%a' "+tc.path)
		require.NoErrorf(t, err, "stat %s", tc.path)
		assert.Equalf(t, tc.mode, strings.TrimSpace(result.Stdout), "%s mode", tc.path)
	}
}

func TestBusyboxSymlinksWork(t *testing.T) {
	client := launchAlpine(t)

	// Alpine's /bin/ls, /bin/cat etc. are symlinks to busybox.
	// Verify the symlink chain resolves and commands execute correctly.
	for _, cmd := range []string{"ls /", "cat /etc/hostname", "id -u"} {
		result, err := client.Exec(context.Background(), cmd)
		require.NoErrorf(t, err, "Exec %q", cmd)
		assert.Equalf(t, 0, result.ExitCode, "%q exit code; stderr: %s", cmd, result.Stderr)
	}
}
