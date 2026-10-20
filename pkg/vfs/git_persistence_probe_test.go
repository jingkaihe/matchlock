package vfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRealFS_GitWorktreePersistenceProbe is a zero-model synthetic probe of the
// write-through host_fs mount against a real Git repository on the same host
// directory. It proves:
//   - a file written through the provider is immediately visible to Git (and to
//     the host filesystem), i.e. persistence is live, not staged at close;
//   - a file added/committed by Git is readable back through the provider;
//   - Git's atomic index.lock create / rename sequence works through the VFS;
//   - mkdir-based lock creation (EEXIST on a second create) works through the VFS.
func TestRealFS_GitWorktreePersistenceProbe(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on host")
	}

	// A worktree-like fixture owned by the test.
	worktree := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "README.md"), []byte("# worktree\n"), 0644))

	// Initialize a real Git repo on the same directory the provider will mount.
	runGit(t, worktree, "init", "-q")
	runGit(t, worktree, "config", "user.email", "probe@example.invalid")
	runGit(t, worktree, "config", "user.name", "VFS Probe")

	p := NewRealFSProvider(worktree)
	s := NewVFSServer(p)

	// --- Write through the provider: persistence must be live. ---
	// Create the parent directory first, as Git subdirectory creation would.
	require.Equal(t, int32(0), s.dispatch(&VFSRequest{Op: OpMkdir, Path: "src", Mode: 0755}).Err)
	createResp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "src/app.txt", Flags: 0, Mode: 0644})
	require.Equal(t, int32(0), createResp.Err, "create src/app.txt through provider")
	writeResp := s.dispatch(&VFSRequest{Op: OpWrite, Handle: createResp.Handle, Data: []byte("payload"), Offset: 0})
	require.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: createResp.Handle})

	// The write must be on the host filesystem already (write-through), visible
	// to Git without any explicit export step.
	assert.FileExists(t, filepath.Join(worktree, "src", "app.txt"))
	status := runGit(t, worktree, "status", "--porcelain", "--untracked-files=all")
	assert.Contains(t, status, "src/app.txt", "git must see the provider-written file")

	// --- Git writes a commit; the provider must read it back. ---
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-q", "-m", "probe commit")
	readResp := s.dispatch(&VFSRequest{Op: OpGetattr, Path: "src/app.txt"})
	require.Equal(t, int32(0), readResp.Err, "committed file must be visible through the provider")

	// --- Git index.lock atomic create/rename via the VFS. ---
	lockCreate := s.dispatch(&VFSRequest{Op: OpCreate, Path: "index.lock", Flags: uint32(linuxOpenCreate | linuxOpenExclusive | linuxOpenWriteOnly), Mode: 0644})
	require.Equal(t, int32(0), lockCreate.Err, "exclusive index.lock create must succeed")
	// A second exclusive create on the existing lock must fail with EEXIST.
	lockCreate2 := s.dispatch(&VFSRequest{Op: OpCreate, Path: "index.lock", Flags: uint32(linuxOpenCreate | linuxOpenExclusive | linuxOpenWriteOnly), Mode: 0644})
	require.Equal(t, int32(-int32(syscall.EEXIST)), lockCreate2.Err, "second exclusive create must fail with EEXIST")
	// Atomically publish the lock file over the target index.lock.
	lockRename := s.dispatch(&VFSRequest{Op: OpRename, Path: "index.lock", NewPath: "index"})
	require.Equal(t, int32(0), lockRename.Err, "rename index.lock -> index")
	assert.FileExists(t, filepath.Join(worktree, "index"))

	// --- mkdir-based lock semantics through the VFS. ---
	require.Equal(t, int32(0), s.dispatch(&VFSRequest{Op: OpMkdir, Path: ".git-lock", Mode: 0755}).Err, "mkdir lock dir")
	require.Equal(t, int32(-int32(syscall.EEXIST)), s.dispatch(&VFSRequest{Op: OpMkdir, Path: ".git-lock", Mode: 0755}).Err, "mkdir same lock dir must fail")
	require.Equal(t, int32(0), s.dispatch(&VFSRequest{Op: OpRmdir, Path: ".git-lock"}).Err, "rmdir lock dir")

	// --- Read-after-write visibility through a fresh handle. ---
	openResp := s.dispatch(&VFSRequest{Op: OpOpen, Path: "src/app.txt", Flags: 0})
	require.Equal(t, int32(0), openResp.Err)
	readResp = s.dispatch(&VFSRequest{Op: OpRead, Handle: openResp.Handle, Size: 64, Offset: 0})
	require.Equal(t, int32(0), readResp.Err)
	assert.Equal(t, "payload", string(readResp.Data))
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: openResp.Handle})

	require.NoError(t, CloseProvider(p))
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, string(out))
	return string(out)
}
