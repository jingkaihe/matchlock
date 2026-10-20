//go:build linux

package guestfused

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jingkaihe/matchlock/pkg/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFuseMount_GitCommitWriteThrough is a zero-model, synthetic, host-only
// probe (no VM, no network, no credentials) that mounts a real FUSE filesystem
// backed by a host git repo and runs a real `git add && git commit`. It locks
// the go-fuse inode-lifecycle fix for git's loose-object write pattern.
//
// Git publishes a loose object via hard LINK + UNLINK (link tmp_obj_XXXX ->
// <sha>, then unlink tmp_obj_XXXX) rather than a rename. The fused Link handler
// correctly shares the target inode (hard links share an inode number, so go-fuse
// coalesces them), but it must repoint the shared node's cached path to the
// surviving link name. Without that, the published object resolves to the
// unlinked temp path and the next open returns ENOENT ("invalid object ... is
// not a valid object" from git).
//
// Requires /dev/fuse + fusermount on the host; skips when unavailable.
func TestFuseMount_GitCommitWriteThrough(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("FUSE mount test is linux-only")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on host")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("/dev/fuse not available; cannot mount a FUSE filesystem")
	}
	if _, err := exec.LookPath("fusermount"); err != nil {
		if _, err3 := exec.LookPath("fusermount3"); err3 != nil {
			t.Skip("neither fusermount nor fusermount3 available")
		}
	}

	// --- Host git repo fixture ---
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("# git write-through\n"), 0644))
	runGitIn(t, repo, "init", "-q")
	// Synthetic-fixture git policy: never spawn auto-maintenance background
	// children from this repo (they hold cwd+lock inside the mount and wedge
	// teardown). Repo-local config only — operator/global git config and product
	// behavior are untouched. The gate driver also sets process-scoped
	// maintenance.auto=false / gc.auto=0 for every child git it runs.
	runGitIn(t, repo, "config", "maintenance.auto", "false")
	runGitIn(t, repo, "config", "gc.auto", "0")
	runGitIn(t, repo, "config", "maintenance.autoDetach", "false")
	runGitIn(t, repo, "config", "user.email", "probe@example.invalid")
	runGitIn(t, repo, "config", "user.name", "Fuse Probe")
	runGitIn(t, repo, "add", "README.md")
	runGitIn(t, repo, "commit", "-q", "-m", "initial")

	// --- Host VFS server: router maps guest /guest/repo to the host repo. ---
	const guestRoot = "/guest/repo"
	router := vfs.NewMountRouter(map[string]vfs.Provider{
		guestRoot: vfs.NewRealFSProvider(repo),
	})
	server := vfs.NewVFSServer(router)

	sock := filepath.Join(t.TempDir(), "vfs.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	defer ln.Close()
	go server.Serve(ln)

	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	uc := conn.(*net.UnixConn)
	file, err := uc.File()
	require.NoError(t, err)
	defer file.Close()
	client := &VFSClient{fd: int(file.Fd())}

	// --- FUSE mount ---
	mnt := t.TempDir()
	require.NoError(t, os.MkdirAll(mnt, 0755))
	root := &VFSRoot{client: client, basePath: guestRoot}
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			FsName: "matchlock-repro",
			Name:   "fuse.matchlock-repro",
		},
		AttrTimeout:  &[]time.Duration{time.Second}[0],
		EntryTimeout: &[]time.Duration{time.Second}[0],
	}
	srv, err := fs.Mount(mnt, root, opts)
	if err != nil {
		t.Skipf("FUSE mount not available: %v", err)
	}
	// Cleanup order: unmount FIRST (while the VFS server/backing are still
	// responsive), then close the transports below (LIFO). A persistent unmount
	// failure is a test failure, never silently ignored.
	defer func() {
		unmountNow(t, srv)
		_ = file.Close()
		_ = ln.Close()
	}()

	// --- Real git write-through through the mount ---
	// Create a new file (create+write+release), like `echo x > new.txt`.
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "new.txt"), []byte("git commit payload\n"), 0644))

	// git add -- publish the blob (hash-object -w uses the link+unlink pattern).
	runGitIn(t, mnt, "add", "new.txt")

	// git commit -- write-tree must read the blob back.
	runGitIn(t, mnt, "commit", "-q", "-m", "fuse commit")

	// The committed blob must be readable back through the mount...
	blobOut := runGitIn(t, mnt, "cat-file", "-p", "HEAD:new.txt")
	assert.Equal(t, "git commit payload\n", blobOut)

	// ...and must be present on the host filesystem (write-through).
	hostBlob := runGitIn(t, repo, "cat-file", "-p", "HEAD:new.txt")
	assert.Equal(t, "git commit payload\n", hostBlob)

	// A fresh guest-side stat must succeed (not the '-?????????' symptom).
	fi, err := os.Stat(filepath.Join(mnt, "new.txt"))
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())
}

// TestFuseMount_HardLinkThenUnlinkRead locks the exact go-fuse node-path fix
// without relying on git: create a file, link it to a second name, unlink the
// original, then open the surviving name. Before the fix the surviving name
// resolved to the unlinked original path and returned ENOENT.
func TestFuseMount_HardLinkThenUnlinkRead(t *testing.T) {
	mnt, backing := mountVFSRepro(t)
	_ = backing

	// write the temp file
	tmp := filepath.Join(mnt, "obj", "tmp_obj_abc")
	require.NoError(t, os.WriteFile(tmp, []byte("payload"), 0644))
	// hard-link it to a published name
	final := filepath.Join(mnt, "obj", "6e91c306")
	require.NoError(t, os.Link(tmp, final))
	// unlink the temp name (git loose-object publish)
	require.NoError(t, os.Remove(tmp))

	// The published name must now be readable (it resolves to the same inode).
	data, err := os.ReadFile(final)
	require.NoError(t, err, "published hard link must be readable after unlink of temp name")
	assert.Equal(t, "payload", string(data))
}

// TestFuseMount_HardLinkBothUnlinkDirections locks the fix in BOTH unlink
// directions at the real-FUSE layer:
//
//   - git direction: create tmp, link tmp->sha, unlink tmp; the surviving name
//     (sha) must stay readable and writable.
//   - reverse direction: create OLD, link OLD->NEW, unlink NEW while OLD
//     survives; the surviving name (OLD) must stay readable and writable.
//
// A single mutable cached path that is always repointed to the newest link name
// passes the git direction but fails the reverse one (the surviving OLD name
// resolves to the removed NEW path). The node instead registers every link name
// and only promotes when the current name is removed.
func TestFuseMount_HardLinkBothUnlinkDirections(t *testing.T) {
	mnt, backing := mountVFSRepro(t)

	// --- git direction: link tmp->sha, unlink tmp, survive sha ---
	gitDir := filepath.Join(mnt, "objgit")
	require.NoError(t, os.MkdirAll(gitDir, 0755))
	tmp := filepath.Join(gitDir, "tmp_obj_g")
	require.NoError(t, os.WriteFile(tmp, []byte("git-payload"), 0644))
	sha := filepath.Join(gitDir, "aaaa1111")
	require.NoError(t, os.Link(tmp, sha))
	require.NoError(t, os.Remove(tmp))

	data, err := os.ReadFile(sha)
	require.NoError(t, err, "git direction: sha must be readable after tmp unlink")
	assert.Equal(t, "git-payload", string(data))
	// write through the surviving name and confirm the host backing dir sees it.
	require.NoError(t, os.WriteFile(sha, []byte("git-payload-2"), 0644))
	hostData, err := os.ReadFile(filepath.Join(backing, "objgit", "aaaa1111"))
	require.NoError(t, err)
	assert.Equal(t, "git-payload-2", string(hostData))

	// --- reverse direction: link OLD->NEW, unlink NEW, survive OLD ---
	revDir := filepath.Join(mnt, "objrev")
	require.NoError(t, os.MkdirAll(revDir, 0755))
	oldName := filepath.Join(revDir, "OLD")
	require.NoError(t, os.WriteFile(oldName, []byte("old-payload"), 0644))
	newName := filepath.Join(revDir, "NEW")
	require.NoError(t, os.Link(oldName, newName))
	require.NoError(t, os.Remove(newName))

	data, err = os.ReadFile(oldName)
	require.NoError(t, err, "reverse direction: OLD must be readable after NEW unlink")
	assert.Equal(t, "old-payload", string(data))
	require.NoError(t, os.WriteFile(oldName, []byte("old-payload-2"), 0644))
	hostData, err = os.ReadFile(filepath.Join(backing, "objrev", "OLD"))
	require.NoError(t, err)
	assert.Equal(t, "old-payload-2", string(hostData))
}

// TestFuseMount_HardLinkExistingFileHandles proves that file handles opened
// BEFORE a hard link and unlink keep working through the surviving name in both
// directions, and that a fully unlinked-but-open inode (git temp file pattern)
// remains writable/readable through the open descriptor.
func TestFuseMount_HardLinkExistingFileHandles(t *testing.T) {
	mnt, backing := mountVFSRepro(t)

	// --- reverse direction with an open handle on OLD ---
	revDir := filepath.Join(mnt, "hdlrev")
	require.NoError(t, os.MkdirAll(revDir, 0755))
	oldName := filepath.Join(revDir, "OLD")
	require.NoError(t, os.WriteFile(oldName, []byte("old-v1"), 0644))
	oldFh, err := os.OpenFile(oldName, os.O_RDWR, 0)
	require.NoError(t, err)
	// Link OLD->NEW then unlink NEW while the OLD handle is open.
	require.NoError(t, os.Link(oldName, filepath.Join(revDir, "NEW")))
	require.NoError(t, os.Remove(filepath.Join(revDir, "NEW")))

	// Read/write through the surviving-name handle.
	_, err = oldFh.Seek(0, 0)
	require.NoError(t, err)
	buf := make([]byte, 6)
	_, err = oldFh.Read(buf)
	require.NoError(t, err, "reverse: read through handle opened before unlink")
	assert.Equal(t, "old-v1", string(buf))

	// --- git direction with an open handle on the temp name ---
	gitDir := filepath.Join(mnt, "hdlgit")
	require.NoError(t, os.MkdirAll(gitDir, 0755))
	tmp := filepath.Join(gitDir, "tmp_h")
	require.NoError(t, os.WriteFile(tmp, []byte("tmp-v1"), 0644))
	tmpFh, err := os.OpenFile(tmp, os.O_RDWR, 0)
	require.NoError(t, err)
	sha := filepath.Join(gitDir, "bbbb2222")
	require.NoError(t, os.Link(tmp, sha))
	// git keeps the temp fd open while publishing; unlink the temp name.
	require.NoError(t, os.Remove(tmp))

	// Write through the still-open temp fd: POSIX semantics keep the inode
	// alive; the host must observe the write on the surviving link.
	_, err = tmpFh.WriteAt([]byte("tmp-v2!!"), 0)
	require.NoError(t, err, "write through open handle after unlink of its name")
	require.NoError(t, tmpFh.Sync())
	hostData, err := os.ReadFile(filepath.Join(backing, "hdlgit", "bbbb2222"))
	require.NoError(t, err, "host must see the write-through via the surviving name")
	assert.Equal(t, "tmp-v2!!", string(hostData))

	require.NoError(t, oldFh.Close())
	require.NoError(t, tmpFh.Close())
}

// mountVFSRepro mounts a real FUSE filesystem backed by a temp host directory,
// returning the guest mountpoint and the host backing directory. Skips when
// FUSE/fusermount are unavailable.
func mountVFSRepro(t *testing.T) (string, string) {
	t.Helper()
	backing := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(backing, "obj"), 0755))
	return mountVFSWithBacking(t, backing)
}

// mountVFSWithBacking mounts a real FUSE filesystem backed by the given host
// directory and returns the guest mountpoint plus the backing dir. The caller
// may populate the backing directory (including pre-existing host hard links)
// BEFORE the mount, which is how Lookup-discovered (not guest-Link-created)
// hard-link aliases are exercised. Skips when FUSE/fusermount are unavailable.
func mountVFSWithBacking(t *testing.T, backing string) (string, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("FUSE mount test is linux-only")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("/dev/fuse not available")
	}
	if _, err := exec.LookPath("fusermount"); err != nil {
		if _, err3 := exec.LookPath("fusermount3"); err3 != nil {
			t.Skip("neither fusermount nor fusermount3 available")
		}
	}

	const guestRoot = "/guest/x"
	router := vfs.NewMountRouter(map[string]vfs.Provider{
		guestRoot: vfs.NewRealFSProvider(backing),
	})
	server := vfs.NewVFSServer(router)

	sock := filepath.Join(t.TempDir(), "vfs.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go server.Serve(ln)

	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	uc := conn.(*net.UnixConn)
	file, err := uc.File()
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	client := &VFSClient{fd: int(file.Fd())}

	mnt := t.TempDir()
	require.NoError(t, os.MkdirAll(mnt, 0755))
	root := &VFSRoot{client: client, basePath: guestRoot}
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{FsName: "matchlock-repro", Name: "fuse.matchlock-repro"},
		AttrTimeout:  &[]time.Duration{time.Second}[0],
		EntryTimeout: &[]time.Duration{time.Second}[0],
	}
	srv, err := fs.Mount(mnt, root, opts)
	if err != nil {
		t.Skipf("FUSE mount not available: %v", err)
	}
	// Cleanup order (t.Cleanup is LIFO): unmount FIRST while the VFS server and
	// the host backing are still responsive, then close the transports. A
	// persistent unmount failure is a test failure, never silently ignored.
	t.Cleanup(func() { unmountNow(t, srv) })
	return mnt, backing
}

// TestFuseMount_HardLinkMultipleNames proves that when an inode has more than
// two hard-link names, unlinking any subset in any order keeps a surviving name
// resolvable: create A, link A->B, link A->C, unlink A and B (C survives), read
// and write through C; then start again and unlink B and C (A survives).
func TestFuseMount_HardLinkMultipleNames(t *testing.T) {
	mnt, backing := mountVFSRepro(t)

	// A -> B -> C; remove A and B; C survives.
	dir := filepath.Join(mnt, "multi1")
	require.NoError(t, os.MkdirAll(dir, 0755))
	a := filepath.Join(dir, "A")
	require.NoError(t, os.WriteFile(a, []byte("payload-ABC"), 0644))
	require.NoError(t, os.Link(a, filepath.Join(dir, "B")))
	require.NoError(t, os.Link(a, filepath.Join(dir, "C")))
	require.NoError(t, os.Remove(a))
	require.NoError(t, os.Remove(filepath.Join(dir, "B")))
	c := filepath.Join(dir, "C")
	data, err := os.ReadFile(c)
	require.NoError(t, err, "C must resolve after A and B unlinked")
	assert.Equal(t, "payload-ABC", string(data))
	require.NoError(t, os.WriteFile(c, []byte("payload-C2"), 0644))
	hostData, err := os.ReadFile(filepath.Join(backing, "multi1", "C"))
	require.NoError(t, err)
	assert.Equal(t, "payload-C2", string(hostData))

	// A -> B -> C; remove B and C; A survives.
	dir2 := filepath.Join(mnt, "multi2")
	require.NoError(t, os.MkdirAll(dir2, 0755))
	a2 := filepath.Join(dir2, "A")
	require.NoError(t, os.WriteFile(a2, []byte("payload-A2"), 0644))
	require.NoError(t, os.Link(a2, filepath.Join(dir2, "B")))
	require.NoError(t, os.Link(a2, filepath.Join(dir2, "C")))
	require.NoError(t, os.Remove(filepath.Join(dir2, "B")))
	require.NoError(t, os.Remove(filepath.Join(dir2, "C")))
	data, err = os.ReadFile(a2)
	require.NoError(t, err, "A must resolve after B and C unlinked")
	assert.Equal(t, "payload-A2", string(data))
}

func runGitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v in %s failed: %s", args, dir, string(out))
	return string(out)
}
