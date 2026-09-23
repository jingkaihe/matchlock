// Package testutil holds small test-only helpers shared across packages.
package testutil

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// UnixSocketPathMax is the bindable-length cap for a Unix socket path.
// macOS stores the path in sockaddr_un.sun_path, which is only 104 bytes;
// Linux allows 108. Enforcing the stricter macOS limit keeps tests that bind
// Unix sockets valid on darwin/arm64.
const UnixSocketPathMax = 104

// ShortTempDir returns a freshly created temporary directory whose absolute
// path is short enough for a Unix socket to be bound beneath it. Cleanup is
// registered with t.Cleanup.
//
// Do not use t.TempDir() for socket directories: it appends the full test name
// (plus a counter) to os.TempDir(). On macOS os.TempDir() resolves under
// /var/folders/..., and TMPDIR itself may already be long on Linux, so the
// resulting path can exceed sockaddr_un.sun_path (104 bytes on macOS, 108 on
// Linux) and net.Listen("unix", ...) then fails with EINVAL ("bind: invalid
// argument"). For the same reason this helper does not build the directory
// under os.TempDir()/TMPDIR: it uses the conventionally short /tmp base (with a
// fallback to os.TempDir() only if /tmp is unusable). The short "ml-*" prefix
// keeps the resulting path well under the cap on every platform.
func ShortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ml-*")
	if err != nil {
		// /tmp is unusual but not guaranteed; fall back to the process temp
		// dir. A long TMPDIR can still produce a path over the cap, which
		// RequireSockPathFits will surface explicitly.
		dir, err = os.MkdirTemp("", "ml-*")
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// RequireSockPathFits fails the test if path cannot be bound as a Unix socket
// on the strictest supported platform (macOS: 104-byte sockaddr_un.sun_path).
func RequireSockPathFits(t *testing.T, path string) {
	t.Helper()
	require.LessOrEqual(t, len(path), UnixSocketPathMax, "unix socket path must fit macOS sockaddr_un.sun_path")
}
