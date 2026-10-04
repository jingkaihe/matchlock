//go:build acceptance

package acceptance

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIRunGuestDirectoryFsync(t *testing.T) {
	guestScript := `import os

assert not os.path.exists("/opt/matchlock/guest-fused")
with open("/proc/mounts") as mounts:
    assert not any(line.split()[2].startswith("fuse") for line in mounts)
os.makedirs("/workspace/sub", exist_ok=True)
for path in ("/workspace", "/workspace/sub"):
    fd = os.open(path, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)

print("FSYNC_OK")
`

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		3*time.Minute,
		"run",
		"--image", "python:3.12-alpine",
		"--no-network",
		"--", "python3", "-c", guestScript,
	)
	require.Equalf(t, 0, exitCode, "directory fsync failed\nstdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "FSYNC_OK")
}

func TestCLIRunGuestAppendOpen(t *testing.T) {
	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		3*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--no-network",
		"--", "sh", "-c", strings.Join([]string{
			"set -eu",
			"mkdir -p /workspace",
			"printf 'initial\\n' > /workspace/log.txt",
			"printf 'appended\\n' >> /workspace/log.txt",
			"cat /workspace/log.txt",
		}, "; "),
	)
	require.Equalf(t, 0, exitCode, "append open failed\nstdout: %s\nstderr: %s", stdout, stderr)
	assert.Equal(t, "initial\nappended\n", stdout)
}

func TestCLIRunGuestAppendCreateHandle(t *testing.T) {
	guestScript := `import os

os.makedirs("/workspace", exist_ok=True)
path = "/workspace/log.txt"
fd = os.open(path, os.O_CREAT | os.O_WRONLY | os.O_APPEND, 0o644)
try:
    os.write(fd, b"first\n")

    other = os.open(path, os.O_WRONLY | os.O_APPEND)
    try:
        os.write(other, b"second\n")
    finally:
        os.close(other)

    os.pwrite(fd, b"third\n", 0)
finally:
    os.close(fd)

print(open(path, "r").read(), end="")
`

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		3*time.Minute,
		"run",
		"--image", "python:3.12-alpine",
		"--no-network",
		"--", "python3", "-c", guestScript,
	)
	require.Equalf(t, 0, exitCode, "append create failed\nstdout: %s\nstderr: %s", stdout, stderr)
	assert.Equal(t, "first\nsecond\nthird\n", stdout)
}
