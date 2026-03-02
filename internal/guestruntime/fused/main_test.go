package guestfused

import (
	"context"
	"encoding/binary"
	"errors"
	"syscall"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFillAttrIncludesInode(t *testing.T) {
	var attr fuse.Attr
	fillAttr(&attr, &VFSStat{
		Size:    12,
		Mode:    0755,
		ModTime: 1700000000,
		IsDir:   true,
		Ino:     12345,
	})

	assert.Equal(t, uint64(12345), attr.Ino)
	assert.Equal(t, uint32(syscall.S_IFDIR|0755), attr.Mode)
	assert.Equal(t, uint32(2), attr.Nlink)
}

func TestFillEntryAttrFallbackUsesProvidedInode(t *testing.T) {
	var out fuse.EntryOut
	fillEntryAttr(&out, nil, entryAttrDefaults{
		mode:  syscall.S_IFREG | 0644,
		ino:   4242,
		isDir: false,
	})

	assert.Equal(t, uint64(4242), out.Ino)
	assert.Equal(t, uint64(4242), out.Attr.Ino)
	assert.Equal(t, uint32(syscall.S_IFREG|0644), out.Attr.Mode)
	assert.Equal(t, uint32(1), out.Attr.Nlink)
}

func TestInodeForPathDeterministic(t *testing.T) {
	dirA := inodeForPath("/workspace/repo", true)
	dirB := inodeForPath("/workspace/repo", true)
	file := inodeForPath("/workspace/repo", false)

	assert.NotZero(t, dirA)
	assert.Equal(t, dirA, dirB)
	assert.NotEqual(t, dirA, file)
}

func TestRebasePathForRename(t *testing.T) {
	oldPath := "/workspace/repo/old"
	newPath := "/workspace/repo/new"

	assert.Equal(t, newPath, rebasePathForRename(oldPath, oldPath, newPath))
	assert.Equal(t, "/workspace/repo/new/sub/file.txt", rebasePathForRename("/workspace/repo/old/sub/file.txt", oldPath, newPath))
	assert.Equal(t, "/workspace/repo/other/file.txt", rebasePathForRename("/workspace/repo/other/file.txt", oldPath, newPath))
}

func TestUpdateCachedPathsAfterRenameRecursesSubtree(t *testing.T) {
	root := &VFSRoot{basePath: "/workspace/repo"}
	fs.NewNodeFS(root, &fs.Options{})

	ctx := context.Background()
	dirNode := &VFSNode{path: "/workspace/repo/old", isDir: true}
	dirInode := root.NewInode(ctx, dirNode, fs.StableAttr{
		Mode: syscall.S_IFDIR,
		Ino:  2,
	})
	require.True(t, root.AddChild("old", dirInode, true))

	subNode := &VFSNode{path: "/workspace/repo/old/sub", isDir: true}
	subInode := dirInode.NewInode(ctx, subNode, fs.StableAttr{
		Mode: syscall.S_IFDIR,
		Ino:  3,
	})
	require.True(t, dirInode.AddChild("sub", subInode, true))

	fileNode := &VFSNode{path: "/workspace/repo/old/sub/file.txt", isDir: false}
	fileInode := subInode.NewInode(ctx, fileNode, fs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  4,
	})
	require.True(t, subInode.AddChild("file.txt", fileInode, true))

	updateCachedPathsAfterRename(dirInode, "/workspace/repo/old", "/workspace/repo/new")

	assert.Equal(t, "/workspace/repo/new", dirNode.path)
	assert.Equal(t, "/workspace/repo/new/sub", subNode.path)
	assert.Equal(t, "/workspace/repo/new/sub/file.txt", fileNode.path)
}

func TestVFSRootRenameDefersTreeMoveToGoFuse(t *testing.T) {
	ctx := context.Background()
	oldPath := "/workspace/repo/old"
	newPath := "/workspace/repo/new"

	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		return verifyRenameRequest(req, oldPath, newPath)
	})
	defer cleanup()

	root := &VFSRoot{client: client, basePath: "/workspace/repo"}
	fs.NewNodeFS(root, &fs.Options{})

	dirNode := &VFSNode{client: client, path: oldPath, isDir: true}
	dirInode := root.NewInode(ctx, dirNode, fs.StableAttr{
		Mode: syscall.S_IFDIR,
		Ino:  11,
	})
	require.True(t, root.AddChild("old", dirInode, true))

	fileNode := &VFSNode{client: client, path: "/workspace/repo/old/file.txt", isDir: false}
	fileInode := dirInode.NewInode(ctx, fileNode, fs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  12,
	})
	require.True(t, dirInode.AddChild("file.txt", fileInode, true))

	errno := root.Rename(ctx, "old", root, "new", 0)
	require.Equal(t, syscall.Errno(0), errno)

	// Simulate rawBridge.Rename cache update (single tree move by go-fuse).
	root.MvChild("old", root.EmbeddedInode(), "new", true)

	assert.Nil(t, root.GetChild("old"))
	assert.Same(t, dirInode, root.GetChild("new"))
	assert.Equal(t, "/workspace/repo/new", dirNode.path)
	assert.Equal(t, "/workspace/repo/new/file.txt", fileNode.path)
}

func TestVFSNodeRenameDefersTreeMoveToGoFuse(t *testing.T) {
	ctx := context.Background()
	oldPath := "/workspace/repo/dir/old.txt"
	newPath := "/workspace/repo/dir/new.txt"

	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		return verifyRenameRequest(req, oldPath, newPath)
	})
	defer cleanup()

	root := &VFSRoot{client: client, basePath: "/workspace/repo"}
	fs.NewNodeFS(root, &fs.Options{})

	parentNode := &VFSNode{client: client, path: "/workspace/repo/dir", isDir: true}
	parentInode := root.NewInode(ctx, parentNode, fs.StableAttr{
		Mode: syscall.S_IFDIR,
		Ino:  21,
	})
	require.True(t, root.AddChild("dir", parentInode, true))

	childNode := &VFSNode{client: client, path: oldPath, isDir: false}
	childInode := parentInode.NewInode(ctx, childNode, fs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  22,
	})
	require.True(t, parentInode.AddChild("old.txt", childInode, true))

	errno := parentNode.Rename(ctx, "old.txt", parentNode, "new.txt", 0)
	require.Equal(t, syscall.Errno(0), errno)

	// Simulate rawBridge.Rename cache update (single tree move by go-fuse).
	parentNode.MvChild("old.txt", parentNode.EmbeddedInode(), "new.txt", true)

	assert.Nil(t, parentNode.GetChild("old.txt"))
	assert.Same(t, childInode, parentNode.GetChild("new.txt"))
	assert.Equal(t, newPath, childNode.path)
}

func newSingleRequestClient(t *testing.T, validate func(*VFSRequest) error) (*VFSClient, func()) {
	t.Helper()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- serveSingleRequest(fds[1], validate)
	}()

	client := &VFSClient{fd: fds[0]}

	cleanup := func() {
		_ = client.Close()
		require.NoError(t, <-done)
	}

	return client, cleanup
}

func serveSingleRequest(fd int, validate func(*VFSRequest) error) error {
	defer func() {
		_ = syscall.Close(fd)
	}()

	var lenBuf [4]byte
	if _, err := readFull(fd, lenBuf[:]); err != nil {
		return err
	}

	reqData := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
	if _, err := readFull(fd, reqData); err != nil {
		return err
	}

	var req VFSRequest
	if err := cbor.Unmarshal(reqData, &req); err != nil {
		return err
	}

	if err := validate(&req); err != nil {
		return err
	}

	respData, err := cbor.Marshal(&VFSResponse{})
	if err != nil {
		return err
	}

	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(respData)))
	if _, err := writeFull(fd, lenBuf[:]); err != nil {
		return err
	}
	if _, err := writeFull(fd, respData); err != nil {
		return err
	}

	return nil
}

func verifyRenameRequest(req *VFSRequest, expectedOldPath string, expectedNewPath string) error {
	if req.Op != OpRename {
		return errors.New("unexpected op")
	}
	if req.Path != expectedOldPath {
		return errors.New("unexpected rename source path: " + req.Path)
	}
	if req.NewPath != expectedNewPath {
		return errors.New("unexpected rename destination path: " + req.NewPath)
	}
	return nil
}
