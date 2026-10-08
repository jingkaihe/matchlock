package guestfused

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jingkaihe/matchlock/internal/testutil"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRequestCtxPropagatesCallerOwnerEndToEnd is the always-runnable
// integration check for the non-root git "dubious ownership" fix: a request
// carrying a go-fuse caller identity (as the kernel supplies for a non-root
// process) must arrive at the host VFS server with that uid/gid and come back
// reporting the caller's ownership for a host_fs stat with no owner override.
// Before the fix the server returned 0/0 and git refused the mount.
func TestRequestCtxPropagatesCallerOwnerEndToEnd(t *testing.T) {
	hostDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hostDir, "file.txt"), []byte("payload"), 0644))

	const guestRoot = "/guest/repo"
	router := vfs.NewMountRouter(map[string]vfs.Provider{
		guestRoot: vfs.NewRealFSProvider(hostDir),
	})
	server := vfs.NewVFSServer(router)

	// Bind the socket in a short MkdirTemp directory, not t.TempDir():
	// t.TempDir() appends this long test name (plus a counter) to os.TempDir(),
	// which on macOS resolves under /var/folders/... and pushes the path past
	// the 104-byte sockaddr_un.sun_path limit, making net.Listen fail with
	// "bind: invalid argument". The short "ml-*" prefix keeps it well under.
	sockDir := testutil.ShortTempDir(t)

	sock := filepath.Join(sockDir, "vfs.sock")
	testutil.RequireSockPathFits(t, sock)
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	defer ln.Close()
	go server.Serve(ln)

	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer conn.Close()
	uc := conn.(*net.UnixConn)
	file, err := uc.File()
	require.NoError(t, err)
	defer file.Close()
	client := &VFSClient{fd: int(file.Fd())}

	ctx := fuse.NewContext(context.Background(), &fuse.Caller{
		Owner: fuse.Owner{Uid: 1234, Gid: 5678},
		Pid:   4321,
	})

	resp, err := client.RequestCtx(ctx, &VFSRequest{Op: OpGetattr, Path: guestRoot + "/file.txt"})
	require.NoError(t, err)
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(1234), resp.Stat.UID)
	assert.Equal(t, uint32(5678), resp.Stat.GID)

	// A plain context (no caller) keeps the upstream 0/0 behavior.
	resp, err = client.RequestCtx(context.Background(), &VFSRequest{Op: OpGetattr, Path: guestRoot + "/file.txt"})
	require.NoError(t, err)
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(0), resp.Stat.UID)
	assert.Equal(t, uint32(0), resp.Stat.GID)
}
