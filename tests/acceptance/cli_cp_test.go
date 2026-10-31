//go:build acceptance

package acceptance

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCopySource creates a small tree with a nested file, a symlink, and a
// binary blob large enough to span many transfer frames.
func writeCopySource(t *testing.T) (dir string, blob []byte) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("world\n"), 0o644))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(dir, "link")))
	blob = make([]byte, 3<<20)
	_, err := rand.Read(blob)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blob.bin"), blob, 0o644))
	return dir, blob
}

func TestCLICopy(t *testing.T) {
	src, blob := writeCopySource(t)
	stdout, stderr, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "--user", "1000:1000", "-d")
	require.Equalf(t, 0, exitCode, "run -d: %s", stderr)
	vmID := strings.TrimSpace(stdout)
	t.Cleanup(func() {
		runCLI(t, "kill", vmID)
		runCLI(t, "rm", vmID)
	})
	exec := func(script string) string {
		t.Helper()
		stdout, stderr, exitCode := runCLI(t, "exec", vmID, "--", "sh", "-c", script)
		require.Equalf(t, 0, exitCode, "%s: %s", script, stderr)
		return stdout
	}

	// Copying in creates /workspace from the directory's contents, owned by
	// the sandbox user, so that user can work in it.
	_, stderr, exitCode = runCLI(t, "cp", src+"/.", vmID+":/workspace")
	require.Equalf(t, 0, exitCode, "cp in: %s", stderr)
	assert.Equal(t, "1000\nhello\n", exec("stat -c %u /workspace/a.txt && cat /workspace/link"))
	exec("mkdir /workspace/dist && echo built > /workspace/dist/out.txt && ln -s /etc/passwd /workspace/escape")

	// Copying out writes the tree to a new host directory and skips the
	// symlink that points outside it.
	out := filepath.Join(t.TempDir(), "out")
	_, stderr, exitCode = runCLI(t, "cp", vmID+":/workspace", out)
	require.Equalf(t, 0, exitCode, "cp out: %s", stderr)
	assert.Contains(t, stderr, "skipped workspace/escape")
	got, err := os.ReadFile(filepath.Join(out, "blob.bin"))
	require.NoError(t, err)
	assert.True(t, string(got) == string(blob), "blob corrupted in round trip")
	got, err = os.ReadFile(filepath.Join(out, "dist", "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "built\n", string(got))
	assert.NoFileExists(t, filepath.Join(out, "escape"))

	stdout, _, exitCode = runCLI(t, "cp", vmID+":/workspace/dist", "-")
	require.Equal(t, 0, exitCode)
	assert.Contains(t, stdout, "dist/out.txt")

	_, stderr, exitCode = runCLI(t, "cp", vmID+":/missing", filepath.Join(t.TempDir(), "x"))
	assert.NotEqual(t, 0, exitCode)
	assert.Contains(t, stderr, "no such file or directory")
}

func TestCLIRunCopyInAndOut(t *testing.T) {
	src, _ := writeCopySource(t)
	out := filepath.Join(t.TempDir(), "dist")
	_, stderr, exitCode := runCLIWithTimeout(t, 2*time.Minute, "run", "--image", "alpine:latest", "--no-network",
		"--copy-in", src+"/.:/workspace", "--copy-out", "/workspace/dist:"+out, "-w", "/workspace",
		"--", "sh", "-c", "mkdir dist && cp a.txt dist/ && exit 3")
	// Results are copied out even when the command fails, and its exit code is kept.
	require.Equalf(t, 3, exitCode, "stderr: %s", stderr)
	got, err := os.ReadFile(filepath.Join(out, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", string(got))
}
