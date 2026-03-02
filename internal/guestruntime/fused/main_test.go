package guestfused

import (
	"context"
	"syscall"
	"testing"

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
