package vfs

import (
	"os"
	"path/filepath"
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

func TestStatFromInfoIncludesOwnerUIDGID(t *testing.T) {
	info := NewFileInfo("file.txt", 0, 0644, time.Unix(1700000000, 0), false)
	info = info.WithOwner(1000, 2000)

	stat := statFromInfo("/workspace/file.txt", info)

	require.NotNil(t, stat)
	assert.Equal(t, uint32(1000), stat.UID)
	assert.Equal(t, uint32(2000), stat.GID)
}

func TestStatFromInfoDefaultsOwnerToZero(t *testing.T) {
	info := NewFileInfo("file.txt", 0, 0644, time.Unix(1700000000, 0), false)

	stat := statFromInfo("/workspace/file.txt", info)

	require.NotNil(t, stat)
	assert.Equal(t, uint32(0), stat.UID)
	assert.Equal(t, uint32(0), stat.GID)
}

func TestLinuxOpenFlagsToHostTranslatesWireFlags(t *testing.T) {
	flags := linuxOpenFlagsToHost(linuxOpenWriteOnly | linuxOpenAppend | linuxOpenCreate | linuxOpenTruncate)

	assert.NotZero(t, flags&os.O_WRONLY)
	assert.NotZero(t, flags&os.O_APPEND)
	assert.NotZero(t, flags&os.O_CREATE)
	assert.NotZero(t, flags&os.O_TRUNC)
}

func TestLinuxOpenFlagsToHostReadWrite(t *testing.T) {
	flags := linuxOpenFlagsToHost(linuxOpenReadWrite)

	assert.NotZero(t, flags&os.O_RDWR)
	assert.Zero(t, flags&os.O_APPEND)
}

func TestLinuxOpenFlagsToHostCreateAddsWriteAccess(t *testing.T) {
	flags := linuxOpenFlagsToHostCreate(linuxOpenAppend)

	assert.NotZero(t, flags&os.O_RDWR)
	assert.NotZero(t, flags&os.O_CREATE)
	assert.NotZero(t, flags&os.O_TRUNC)
	assert.NotZero(t, flags&os.O_APPEND)
}

func TestDispatchWriteAppend(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())

	createResp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "/log.txt", Mode: 0644})
	require.Equal(t, int32(0), createResp.Err)

	writeResp := s.dispatch(&VFSRequest{
		Op: OpWrite, Handle: createResp.Handle,
		Data: []byte("hello"), Offset: 0,
	})
	require.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: createResp.Handle})

	openResp := s.dispatch(&VFSRequest{
		Op: OpOpen, Path: "/log.txt",
		Flags: uint32(linuxOpenWriteOnly | linuxOpenAppend),
	})
	require.Equal(t, int32(0), openResp.Err)

	writeResp = s.dispatch(&VFSRequest{
		Op: OpWrite, Handle: openResp.Handle,
		Data: []byte(" world"), Offset: 0,
	})
	require.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: openResp.Handle})

	readResp := s.dispatch(&VFSRequest{
		Op:    OpOpen,
		Path:  "/log.txt",
		Flags: uint32(syscall.O_RDONLY),
	})
	require.Equal(t, int32(0), readResp.Err)

	dataResp := s.dispatch(&VFSRequest{
		Op: OpRead, Handle: readResp.Handle, Size: 100, Offset: 0,
	})
	assert.Equal(t, "hello world", string(dataResp.Data))
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: readResp.Handle})
}

func TestDispatchWriteAppendMultiple(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())

	createResp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "/log.txt", Mode: 0644})
	require.Equal(t, int32(0), createResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: createResp.Handle})

	openResp := s.dispatch(&VFSRequest{
		Op: OpOpen, Path: "/log.txt",
		Flags: uint32(linuxOpenWriteOnly | linuxOpenAppend),
	})
	require.Equal(t, int32(0), openResp.Err)

	for _, line := range []string{"line1\n", "line2\n", "line3\n"} {
		writeResp := s.dispatch(&VFSRequest{
			Op: OpWrite, Handle: openResp.Handle,
			Data: []byte(line), Offset: 0,
		})
		require.Equal(t, int32(0), writeResp.Err)
	}
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: openResp.Handle})

	readResp := s.dispatch(&VFSRequest{
		Op:    OpOpen,
		Path:  "/log.txt",
		Flags: uint32(syscall.O_RDONLY),
	})
	require.Equal(t, int32(0), readResp.Err)

	dataResp := s.dispatch(&VFSRequest{
		Op: OpRead, Handle: readResp.Handle, Size: 1000, Offset: 0,
	})
	assert.Equal(t, "line1\nline2\nline3\n", string(dataResp.Data))
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: readResp.Handle})
}

func TestDispatchWriteAppendAfterCreateUsesAppendFlag(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())

	createResp := s.dispatch(&VFSRequest{
		Op:    OpCreate,
		Path:  "/log.txt",
		Flags: uint32(linuxOpenWriteOnly | linuxOpenAppend),
		Mode:  0644,
	})
	require.Equal(t, int32(0), createResp.Err)

	writeResp := s.dispatch(&VFSRequest{
		Op: OpWrite, Handle: createResp.Handle,
		Data: []byte("first"), Offset: 0,
	})
	require.Equal(t, int32(0), writeResp.Err)

	otherResp := s.dispatch(&VFSRequest{
		Op:    OpOpen,
		Path:  "/log.txt",
		Flags: uint32(linuxOpenWriteOnly | linuxOpenAppend),
	})
	require.Equal(t, int32(0), otherResp.Err)
	writeResp = s.dispatch(&VFSRequest{
		Op: OpWrite, Handle: otherResp.Handle,
		Data: []byte(" second"), Offset: 0,
	})
	require.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: otherResp.Handle})

	writeResp = s.dispatch(&VFSRequest{
		Op: OpWrite, Handle: createResp.Handle,
		Data: []byte(" third"), Offset: 0,
	})
	require.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: createResp.Handle})

	readResp := s.dispatch(&VFSRequest{
		Op:    OpOpen,
		Path:  "/log.txt",
		Flags: uint32(linuxOpenReadOnly),
	})
	require.Equal(t, int32(0), readResp.Err)

	dataResp := s.dispatch(&VFSRequest{
		Op: OpRead, Handle: readResp.Handle, Size: 100, Offset: 0,
	})
	assert.Equal(t, "first second third", string(dataResp.Data))
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: readResp.Handle})
}

// TestDispatchCreateStripsFileTypeBits proves a FUSE create whose mode carries
// Unix file-type bits (S_IFREG, e.g. 0o100644) is stripped to permissions before
// reaching the provider. os.Root.OpenFile rejects a mode with any bit outside
// 0o777 with "unsupported file mode", so a raw wire-mode create would fail with
// EIO on a host_fs mount.
func TestDispatchCreateStripsFileTypeBits(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	// 0o100644 in the FUSE wire encoding (S_IFREG | 0644).
	resp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "/f.txt", Mode: 0o100644, Flags: uint32(linuxOpenWriteOnly | linuxOpenCreate | linuxOpenTruncate)})
	require.Equal(t, int32(0), resp.Err, "create with S_IFREG type bits must succeed")
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: resp.Handle})

	fi, err := os.Stat(filepath.Join(dir, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
}

func TestDispatchWriteNonAppendStillUsesOffset(t *testing.T) {
	s := NewVFSServer(NewMemoryProvider())

	createResp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "/file.txt", Mode: 0644})
	require.Equal(t, int32(0), createResp.Err)

	writeResp := s.dispatch(&VFSRequest{
		Op: OpWrite, Handle: createResp.Handle,
		Data: []byte("AAAAAAAAAA"), Offset: 0,
	})
	require.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: createResp.Handle})

	openResp := s.dispatch(&VFSRequest{
		Op:    OpOpen,
		Path:  "/file.txt",
		Flags: uint32(syscall.O_RDWR),
	})
	require.Equal(t, int32(0), openResp.Err)

	writeResp = s.dispatch(&VFSRequest{
		Op: OpWrite, Handle: openResp.Handle,
		Data: []byte("BB"), Offset: 3,
	})
	require.Equal(t, int32(0), writeResp.Err)
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: openResp.Handle})

	readResp := s.dispatch(&VFSRequest{
		Op:    OpOpen,
		Path:  "/file.txt",
		Flags: uint32(syscall.O_RDONLY),
	})
	require.Equal(t, int32(0), readResp.Err)

	dataResp := s.dispatch(&VFSRequest{
		Op: OpRead, Handle: readResp.Handle, Size: 100, Offset: 0,
	})
	assert.Equal(t, "AAABBAAAAA", string(dataResp.Data))
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: readResp.Handle})
}

type denyStatProvider struct {
	Provider
}

func (p denyStatProvider) Stat(path string) (FileInfo, error) {
	return FileInfo{}, syscall.EACCES
}
