package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hasDebugfs() bool {
	_, err := exec.LookPath("debugfs")
	return err == nil
}

func hasMkfsExt4() bool {
	_, err := exec.LookPath("mkfs.ext4")
	return err == nil
}

func createTestExt4(t *testing.T, sizeMB int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ext4")
	f, err := os.Create(path)
	require.NoError(t, err)
	err = f.Truncate(int64(sizeMB) * 1024 * 1024)
	f.Close()
	require.NoError(t, err)

	cmd := exec.Command("mkfs.ext4", "-F", "-q", path)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "mkfs.ext4 failed: %s", out)
	return path
}

func debugfsStatMode(t *testing.T, rootfsPath, guestPath string) string {
	t.Helper()
	cmd := exec.Command("debugfs", "-R", "stat "+guestPath, rootfsPath)
	out, err := cmd.Output()
	require.NoError(t, err, "debugfs stat failed")
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Mode:") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func debugfsCat(t *testing.T, rootfsPath, guestPath string) string {
	t.Helper()
	cmd := exec.Command("debugfs", "-R", "cat "+guestPath, rootfsPath)
	out, err := cmd.Output()
	require.NoError(t, err, "debugfs cat failed")
	return string(out)
}

func TestInjectConfigFileIntoRootfs(t *testing.T) {
	if !hasDebugfs() || !hasMkfsExt4() {
		t.Skip("debugfs or mkfs.ext4 not available")
	}

	rootfs := createTestExt4(t, 10)
	content := []byte("test-ca-cert-content")

	require.NoError(t, injectConfigFileIntoRootfs(rootfs, "/etc/ssl/certs/ca.crt", content), "injectConfigFileIntoRootfs failed")

	got := debugfsCat(t, rootfs, "/etc/ssl/certs/ca.crt")
	assert.Equal(t, string(content), got)
}

func TestInjectConfigFileIntoRootfs_Mode0644(t *testing.T) {
	if !hasDebugfs() || !hasMkfsExt4() {
		t.Skip("debugfs or mkfs.ext4 not available")
	}

	rootfs := createTestExt4(t, 10)

	require.NoError(t, injectConfigFileIntoRootfs(rootfs, "/etc/test.conf", []byte("data")))

	modeLine := debugfsStatMode(t, rootfs, "/etc/test.conf")
	assert.Contains(t, modeLine, "0644", "expected mode 0644")
}

func TestInjectConfigFileIntoRootfs_CreatesParentDirs(t *testing.T) {
	if !hasDebugfs() || !hasMkfsExt4() {
		t.Skip("debugfs or mkfs.ext4 not available")
	}

	rootfs := createTestExt4(t, 10)

	require.NoError(t, injectConfigFileIntoRootfs(rootfs, "/a/b/c/file.txt", []byte("deep")))

	got := debugfsCat(t, rootfs, "/a/b/c/file.txt")
	assert.Equal(t, "deep", got)
}

func TestInjectConfigFileIntoRootfs_Overwrites(t *testing.T) {
	if !hasDebugfs() || !hasMkfsExt4() {
		t.Skip("debugfs or mkfs.ext4 not available")
	}

	rootfs := createTestExt4(t, 10)

	require.NoError(t, injectConfigFileIntoRootfs(rootfs, "/etc/test.conf", []byte("first")))
	require.NoError(t, injectConfigFileIntoRootfs(rootfs, "/etc/test.conf", []byte("second")))

	got := debugfsCat(t, rootfs, "/etc/test.conf")
	assert.Equal(t, "second", got)
}
