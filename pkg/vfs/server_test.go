package vfs

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDispatchCreateReturnsStat(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())

	resp := s.dispatch(&VFSRequest{
		Op:   OpCreate,
		Path: "/file.txt",
		Mode: 0644,
	})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.False(t, resp.Stat.IsDir)
	assert.Equal(t, uint32(0644), resp.Stat.Mode&0777)
	assert.NotZero(t, resp.Handle)

	release := s.dispatch(&VFSRequest{Op: OpRelease, Handle: resp.Handle})
	require.Equal(t, int32(0), release.Err)
}

func TestDispatchMkdirReturnsStat(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())

	resp := s.dispatch(&VFSRequest{
		Op:   OpMkdir,
		Path: "/repo",
		Mode: 0755,
	})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.True(t, resp.Stat.IsDir)
	assert.Equal(t, uint32(0755), resp.Stat.Mode&0777)
}

func TestDispatchMkdirSucceedsWhenFollowUpStatDenied(t *testing.T) {
	base := NewMemoryProvider()
	s := NewVFSServer(denyStatProvider{Provider: base})

	resp := s.dispatch(&VFSRequest{
		Op:   OpMkdir,
		Path: "/repo",
		Mode: 0755,
	})
	require.Equal(t, int32(0), resp.Err)
	assert.Nil(t, resp.Stat)

	info, err := base.Stat("/repo")
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	retry := s.dispatch(&VFSRequest{
		Op:   OpMkdir,
		Path: "/repo",
		Mode: 0755,
	})
	require.Equal(t, -int32(syscall.EEXIST), retry.Err)
}

func TestStatFromInfoSynthesizesStableNonZeroInode(t *testing.T) {
	info := NewFileInfo("repo", 0, os.ModeDir|0755, time.Unix(1700000000, 0), true)

	first := statFromInfo("/workspace/repo", info)
	second := statFromInfo("/workspace/repo", info)
	other := statFromInfo("/workspace/other", info)

	require.NotNil(t, first)
	assert.NotZero(t, first.Ino)
	assert.Equal(t, first.Ino, second.Ino)
	assert.NotEqual(t, first.Ino, other.Ino)
}

func TestDirentsFromEntriesPreferProviderInode(t *testing.T) {
	st := &syscall.Stat_t{Dev: 7, Ino: 4242}
	info := NewFileInfoWithSys("file.txt", 3, 0644, time.Unix(1700000000, 0), false, st)
	entries := []DirEntry{
		NewDirEntry("file.txt", false, 0644, info),
	}

	dirents := direntsFromEntries("/workspace", entries)
	require.Len(t, dirents, 1)
	assert.Equal(t, namespacedInode(uint64(st.Dev), uint64(st.Ino)), dirents[0].Ino)
}

func TestInodeFromSysNamespacesByDevice(t *testing.T) {
	sameInoDevA := inodeFromSys(&syscall.Stat_t{Dev: 1, Ino: 2})
	sameInoDevB := inodeFromSys(&syscall.Stat_t{Dev: 2, Ino: 2})
	repeated := inodeFromSys(&syscall.Stat_t{Dev: 1, Ino: 2})

	assert.NotZero(t, sameInoDevA)
	assert.NotZero(t, sameInoDevB)
	assert.NotEqual(t, sameInoDevA, sameInoDevB)
	assert.Equal(t, sameInoDevA, repeated)
}

func TestDispatchWriteAppendMode(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	// Create a file via the VFS server
	createResp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "/test.txt", Mode: 0644})
	require.Equal(t, int32(0), createResp.Err)
	writeResp := s.dispatch(&VFSRequest{Op: OpWrite, Handle: createResp.Handle, Data: []byte("hello\n"), Offset: 0})
	require.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: createResp.Handle})

	// Open with O_APPEND and write — this is the bug scenario
	openResp := s.dispatch(&VFSRequest{
		Op:    OpOpen,
		Path:  "/test.txt",
		Flags: uint32(syscall.O_WRONLY | syscall.O_APPEND),
		Mode:  0644,
	})
	require.Equal(t, int32(0), openResp.Err)

	appendResp := s.dispatch(&VFSRequest{
		Op:     OpWrite,
		Handle: openResp.Handle,
		Data:   []byte("world\n"),
		Offset: 0,
	})
	assert.Equal(t, int32(0), appendResp.Err, "write on O_APPEND handle should succeed")
	assert.Equal(t, uint32(6), appendResp.Written)

	s.dispatch(&VFSRequest{Op: OpRelease, Handle: openResp.Handle})

	// Verify file contents: "hello\nworld\n"
	content, err := os.ReadFile(dir + "/test.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello\nworld\n", string(content))
}

func TestDispatchWriteNonAppendUsesOffset(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	// Create a file
	createResp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "/test.txt", Mode: 0644})
	require.Equal(t, int32(0), createResp.Err)
	s.dispatch(&VFSRequest{Op: OpWrite, Handle: createResp.Handle, Data: []byte("hello world\n"), Offset: 0})
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: createResp.Handle})

	// Open without O_APPEND, write at a specific offset
	openResp := s.dispatch(&VFSRequest{
		Op:    OpOpen,
		Path:  "/test.txt",
		Flags: uint32(syscall.O_RDWR),
		Mode:  0644,
	})
	require.Equal(t, int32(0), openResp.Err)

	writeResp := s.dispatch(&VFSRequest{
		Op:     OpWrite,
		Handle: openResp.Handle,
		Data:   []byte("WORLD"),
		Offset: 6,
	})
	assert.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: openResp.Handle})

	content, err := os.ReadFile(dir + "/test.txt")
	require.NoError(t, err)
	assert.Equal(t, "hello WORLD\n", string(content))
}

type denyStatProvider struct {
	Provider
}

func (p denyStatProvider) Stat(path string) (FileInfo, error) {
	return FileInfo{}, syscall.EACCES
}
