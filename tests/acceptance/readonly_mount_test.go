//go:build acceptance

package acceptance

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCLIRunReadonlyVolumeDeniesWrite verifies a `:ro` host_fs mount denies
// guest writes (touch/rm/mkdir) with a read-only error while reads succeed.
// A writable control host_fs mount proves the denial is real: the guest CAN
// write there AND the write persists to the host dir.
//
//	-v <roHost>:readonly:ro        -> /workspace/readonly   (host_fs, read-only)
//	-v <rwHost>:writable:host_fs   -> /workspace/writable   (host_fs, direct rw)
func TestCLIRunReadonlyVolumeDeniesWrite(t *testing.T) {
	roHost := t.TempDir()
	rwHost := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(roHost, "README.md"), []byte("readable-content"), 0644), "write probe file")
	require.NoError(t, os.WriteFile(filepath.Join(rwHost, "seed.txt"), []byte("seed"), 0644), "write seed file")

	stdout, stderr, exitCode := runCLIWithTimeout(
		t,
		2*time.Minute,
		"run",
		"--image", "alpine:latest",
		"--workspace", "/workspace",
		"-v", roHost+":readonly:ro",
		"-v", rwHost+":writable:host_fs",
		"--",
		"sh", "-c",
		"cat /workspace/readonly/README.md && echo READ_OK; "+
			"touch /workspace/writable/control.txt && echo CONTROL_WRITE_OK; "+
			"if touch /workspace/readonly/newfile 2>/dev/null; then echo WRITE_UNEXPECTED_OK; else echo WRITE_DENIED; fi; "+
			"if rm /workspace/readonly/README.md 2>/dev/null; then echo RM_UNEXPECTED_OK; else echo RM_DENIED; fi; "+
			"if mkdir /workspace/readonly/newdir 2>/dev/null; then echo MKDIR_UNEXPECTED_OK; else echo MKDIR_DENIED; fi",
	)
	require.Equal(t, 0, exitCode, "stdout: %s\nstderr: %s", stdout, stderr)
	assert.Contains(t, stdout, "readable-content", "read on readonly mount should succeed")
	assert.Contains(t, stdout, "READ_OK", "read on readonly mount should succeed")
	assert.Contains(t, stdout, "CONTROL_WRITE_OK", "writable control host_fs mount should accept a write")
	assert.Contains(t, stdout, "WRITE_DENIED", "touch on readonly mount should be denied")
	assert.Contains(t, stdout, "RM_DENIED", "rm on readonly mount should be denied")
	assert.Contains(t, stdout, "MKDIR_DENIED", "mkdir on readonly mount should be denied")
	for _, unexpected := range []string{"WRITE_UNEXPECTED_OK", "RM_UNEXPECTED_OK", "MKDIR_UNEXPECTED_OK"} {
		assert.NotContains(t, stdout, unexpected)
	}
	// Read-only host_mount stays byte-for-byte; the writable host_fs mount
	// persisted the guest's new file to the host dir.
	assert.FileExists(t, filepath.Join(roHost, "README.md"), "readonly host file must remain")
	_, err := os.Stat(filepath.Join(roHost, "newfile"))
	assert.True(t, os.IsNotExist(err), "readonly dir must not gain newfile (got %v)", err)
	assert.FileExists(t, filepath.Join(rwHost, "control.txt"), "writable host_fs mount should persist the guest's file to the host")
}
