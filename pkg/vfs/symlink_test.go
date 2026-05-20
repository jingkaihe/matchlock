package vfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RealFSProvider

func TestRealFSProvider_Stat_SymlinkNotFollowed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "target.txt"), []byte("content"), 0644))
	require.NoError(t, os.Symlink("target.txt", filepath.Join(dir, "link")))

	p := NewRealFSProvider(dir)
	info, err := p.Stat("/link")
	require.NoError(t, err)
	assert.True(t, info.Mode()&os.ModeSymlink != 0, "Stat should report symlink mode, not follow the link")
	assert.False(t, info.IsDir())
}

func TestRealFSProvider_Symlink_Readlink(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)

	require.NoError(t, p.Symlink("sub/target", "/link"))
	target, err := p.Readlink("/link")
	require.NoError(t, err)
	assert.Equal(t, "sub/target", target)
}

func TestRealFSProvider_Symlink_RejectsAbsoluteTarget(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)

	err := p.Symlink("/etc/passwd", "/link")
	assert.ErrorIs(t, err, syscall.EPERM)

	_, statErr := os.Lstat(filepath.Join(dir, "link"))
	assert.True(t, os.IsNotExist(statErr), "rejected symlink must not exist on disk")
}

func TestRealFSProvider_Symlink_RejectsTargetEscapingRoot(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)

	err := p.Symlink("../escape", "/link")
	assert.ErrorIs(t, err, syscall.EPERM)
}

func TestRealFSProvider_Symlink_AllowsRelativeTargetInsideRoot(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules"), 0755))
	require.NoError(t, p.Symlink("../.pnpm/pkg/node_modules", "/node_modules/pkg"))

	got, err := os.Readlink(filepath.Join(dir, "node_modules", "pkg"))
	require.NoError(t, err)
	assert.Equal(t, "../.pnpm/pkg/node_modules", got)
}

func TestRealFSProvider_Symlink_AppearsInReadDir(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hi"), 0644))
	require.NoError(t, p.Symlink("file.txt", "/link"))

	entries, err := p.ReadDir("/")
	require.NoError(t, err)

	byName := make(map[string]DirEntry)
	for _, e := range entries {
		byName[e.Name()] = e
	}
	require.Contains(t, byName, "link")
	info, err := byName["link"].Info()
	require.NoError(t, err)
	assert.True(t, info.Mode()&os.ModeSymlink != 0, "dir entry for symlink should carry ModeSymlink")
}

// Server dispatch

func TestDispatch_OpSymlink_CreatesSymlinkAndReturnsStat(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "target.txt"), []byte("hi"), 0644))

	s := NewVFSServer(NewRealFSProvider(dir))

	resp := s.dispatch(&VFSRequest{Op: OpSymlink, Path: "/link", NewPath: "target.txt"})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat, "should return symlink stat")
	assert.True(t, resp.Stat.Mode&uint32(os.ModeSymlink) != 0, "stat should carry ModeSymlink bit")

	got, err := os.Readlink(filepath.Join(dir, "link"))
	require.NoError(t, err)
	assert.Equal(t, "target.txt", got)
}

func TestDispatch_OpReadlink_ReturnsTarget(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Symlink("../some/target", filepath.Join(dir, "link")))

	s := NewVFSServer(NewRealFSProvider(dir))

	resp := s.dispatch(&VFSRequest{Op: OpReadlink, Path: "/link"})
	require.Equal(t, int32(0), resp.Err)
	assert.Equal(t, "../some/target", string(resp.Data))
}

func TestDispatch_OpReadlink_ErrOnMissingPath(t *testing.T) {
	s := NewVFSServer(NewRealFSProvider(t.TempDir()))

	resp := s.dispatch(&VFSRequest{Op: OpReadlink, Path: "/nonexistent"})
	assert.NotEqual(t, int32(0), resp.Err)
}

func TestDispatch_OpSymlink_ErrOnExistingEntry(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "link"), []byte(""), 0644))

	s := NewVFSServer(NewRealFSProvider(dir))

	resp := s.dispatch(&VFSRequest{Op: OpSymlink, Path: "/link", NewPath: "target"})
	assert.Equal(t, -int32(syscall.EEXIST), resp.Err)
}

func TestRealFSProvider_Symlink_RejectsMultiLevelEscape(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)

	err := p.Symlink("../../../etc/passwd", "/link")
	assert.ErrorIs(t, err, syscall.EPERM)
}

func TestRealFSProvider_Symlink_RejectsEmbeddedDotDotEscape(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0755))

	// Link sits at <root>/sub/link. Target "../../escape" resolves to
	// <root-parent>/escape, which is outside the mount.
	err := p.Symlink("../../escape", "/sub/link")
	assert.ErrorIs(t, err, syscall.EPERM)
}

func TestRealFSProvider_Symlink_AllowsDotDotThatStaysWithinDeepMount(t *testing.T) {
	dir := t.TempDir()
	p := NewRealFSProvider(dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0755))

	// Link at <root>/a/b/c/link with target "../../d" resolves to <root>/a/d.
	require.NoError(t, p.Symlink("../../d", "/a/b/c/link"))
	got, err := os.Readlink(filepath.Join(dir, "a", "b", "c", "link"))
	require.NoError(t, err)
	assert.Equal(t, "../../d", got)
}

func TestDispatch_OpSymlink_RejectsEscapingTarget(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	resp := s.dispatch(&VFSRequest{Op: OpSymlink, Path: "/link", NewPath: "../../../etc/passwd"})
	assert.Equal(t, -int32(syscall.EPERM), resp.Err)

	_, statErr := os.Lstat(filepath.Join(dir, "link"))
	assert.True(t, os.IsNotExist(statErr), "rejected symlink must not be created on disk")
}

func TestDispatch_OpSymlink_RejectsAbsoluteTarget(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	resp := s.dispatch(&VFSRequest{Op: OpSymlink, Path: "/link", NewPath: "/etc/passwd"})
	assert.Equal(t, -int32(syscall.EPERM), resp.Err)
}

// Pre-existing escaping symlinks must not be followed by file ops.

func TestRealFSProvider_Open_RejectsPreExistingSymlink(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("host-secret"), 0644))

	dir := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "sneak")))

	p := NewRealFSProvider(dir)
	_, err := p.Open("/sneak", os.O_RDONLY, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.ELOOP, "O_NOFOLLOW must reject a symlink as the final component")
}

func TestRealFSProvider_Create_RejectsPreExistingSymlink(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("host-secret"), 0644))

	dir := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "sneak")))

	p := NewRealFSProvider(dir)
	_, err := p.Create("/sneak", 0644)
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.ELOOP, "Create must refuse to truncate through a symlink")

	got, err := os.ReadFile(filepath.Join(outside, "secret"))
	require.NoError(t, err)
	assert.Equal(t, "host-secret", string(got), "outside file must remain untouched")
}

func TestRealFSProvider_Chmod_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("hi"), 0644))
	require.NoError(t, os.Symlink("real.txt", filepath.Join(dir, "link")))

	p := NewRealFSProvider(dir)
	err := p.Chmod("/link", 0600)
	assert.ErrorIs(t, err, syscall.EPERM)

	info, err := os.Stat(filepath.Join(dir, "real.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0644), info.Mode().Perm(), "Chmod on a symlink must not modify its target")
}

func TestDispatch_OpSymlink_SucceedsWhenFollowUpStatFails(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(denyStatProvider{Provider: NewRealFSProvider(dir)})

	resp := s.dispatch(&VFSRequest{Op: OpSymlink, Path: "/link", NewPath: "target"})
	require.Equal(t, int32(0), resp.Err)
	assert.Nil(t, resp.Stat)

	got, err := os.Readlink(filepath.Join(dir, "link"))
	require.NoError(t, err)
	assert.Equal(t, "target", got)
}
