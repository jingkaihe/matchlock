package vfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDispatchSymlinkReadlink proves the VFS server dispatches OpSymlink and
// OpReadlink to the provider (so a guest can create and read a symlink). It uses
// a real confined host_fs root so the symlink is genuinely materialized.
func TestDispatchSymlinkReadlink(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	// In-root relative symlink: create /link -> "target".
	resp := s.dispatch(&VFSRequest{Op: OpSymlink, Path: "/link", Data: []byte("target")})
	require.Equal(t, int32(0), resp.Err, "symlink dispatch failed")

	// The symlink must exist as a link (Lstat shows ModeSymlink), not a copy.
	fi, err := os.Lstat(filepath.Join(dir, "link"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink)

	target := s.dispatch(&VFSRequest{Op: OpReadlink, Path: "/link"})
	require.Equal(t, int32(0), target.Err, "readlink dispatch failed")
	assert.Equal(t, "target", string(target.Data))
}

// TestDispatchSymlinkReportsSymlinkMode proves a symlink lookup on the wire
// advertises S_IFLNK (ModeSymlink bit), so the guest FUSE daemon reports the
// node as a symlink rather than a regular file.
func TestDispatchSymlinkReportsSymlinkMode(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	require.Equal(t, int32(0), s.dispatch(&VFSRequest{Op: OpSymlink, Path: "/link", Data: []byte("target")}).Err)

	stat := s.dispatch(&VFSRequest{Op: OpLookup, Path: "/link"})
	require.Equal(t, int32(0), stat.Err)
	require.NotNil(t, stat.Stat)
	assert.NotZero(t, stat.Stat.Mode&uint32(os.ModeSymlink), "lookup must report symlink mode")
	assert.False(t, stat.Stat.IsDir)
}

// TestDispatchLinkHardProvesHardLink proves OpLink materializes a real hard link
// within the confined root (same inode), which npm/Git can rely on.
func TestDispatchLinkHardProvesHardLink(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	create := s.dispatch(&VFSRequest{Op: OpCreate, Path: "/file", Mode: 0644})
	require.Equal(t, int32(0), create.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: create.Handle})

	linkResp := s.dispatch(&VFSRequest{Op: OpLink, Path: "/file", NewPath: "/hard"})
	require.Equal(t, int32(0), linkResp.Err, "hard link dispatch failed")

	orig, err := os.Stat(filepath.Join(dir, "file"))
	require.NoError(t, err)
	hard, err := os.Stat(filepath.Join(dir, "hard"))
	require.NoError(t, err)
	sOrig := orig.Sys().(*syscall.Stat_t)
	sHard := hard.Sys().(*syscall.Stat_t)
	assert.Equal(t, sOrig.Ino, sHard.Ino, "hard link must share the inode")
}

// TestDispatchLinkOutsideRootGivesENOSYS proves Link falls back to ENOSYS for a
// provider (memory) that does not implement hard links, rather than panicking or
// silently succeeding.
func TestDispatchLinkOutsideRootGivesENOSYS(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())
	resp := s.dispatch(&VFSRequest{Op: OpLink, Path: "/file", NewPath: "/hard"})
	require.Equal(t, -int32(syscall.ENOSYS), resp.Err)
}
