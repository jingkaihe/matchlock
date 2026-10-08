package guestfused

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
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

func TestVFSRootFsyncUsesBasePath(t *testing.T) {
	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		return verifyFsyncPathRequest(req, "/workspace")
	})
	defer cleanup()

	root := &VFSRoot{client: client, basePath: "/workspace"}

	errno := root.Fsync(context.Background(), nil, 0)
	assert.Equal(t, syscall.Errno(0), errno)
}

func TestVFSNodeFsyncUsesNodePath(t *testing.T) {
	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		return verifyFsyncPathRequest(req, "/workspace/repo")
	})
	defer cleanup()

	node := &VFSNode{client: client, path: "/workspace/repo", isDir: true}

	errno := node.Fsync(context.Background(), nil, 0)
	assert.Equal(t, syscall.Errno(0), errno)
}

func TestVFSRootCreateForwardsFlags(t *testing.T) {
	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		return verifyCreateRequest(req, "/workspace/file.txt", syscall.O_WRONLY|syscall.O_APPEND, 0644)
	})
	defer cleanup()

	root := &VFSRoot{client: client, basePath: "/workspace"}
	fs.NewNodeFS(root, &fs.Options{})

	var out fuse.EntryOut
	_, _, _, errno := root.Create(context.Background(), "file.txt", syscall.O_WRONLY|syscall.O_APPEND, 0644, &out)
	assert.Equal(t, syscall.Errno(0), errno)
}

func TestVFSNodeCreateForwardsFlags(t *testing.T) {
	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		return verifyCreateRequest(req, "/workspace/dir/file.txt", syscall.O_WRONLY|syscall.O_APPEND, 0600)
	})
	defer cleanup()

	node := &VFSNode{client: client, path: "/workspace/dir", isDir: true}
	fs.NewNodeFS(node, &fs.Options{})

	var out fuse.EntryOut
	_, _, _, errno := node.Create(context.Background(), "file.txt", syscall.O_WRONLY|syscall.O_APPEND, 0600, &out)
	assert.Equal(t, syscall.Errno(0), errno)
}

func newSingleRequestClient(t *testing.T, validate func(*VFSRequest) error) (*VFSClient, func()) {
	t.Helper()
	return newNRequestClient(t, 1, validate)
}

// newNRequestClient serves exactly n requests over one socketpair, validating
// each with validate and replying with an empty successful response. It is used
// when a handler sequence (for example Link then Unlink) issues more than one
// host request.
func newNRequestClient(t *testing.T, n int, validate func(*VFSRequest) error) (*VFSClient, func()) {
	t.Helper()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- serveNRequests(fds[1], n, validate)
	}()

	client := &VFSClient{fd: fds[0]}

	cleanup := func() {
		_ = client.Close()
		require.NoError(t, <-done)
	}

	return client, cleanup
}

func serveNRequests(fd int, n int, validate func(*VFSRequest) error) error {
	defer func() {
		_ = syscall.Close(fd)
	}()
	for i := 0; i < n; i++ {
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

func verifyFsyncPathRequest(req *VFSRequest, expectedPath string) error {
	if req.Op != OpFsyncPath {
		return errors.New("unexpected op")
	}
	if req.Path != expectedPath {
		return errors.New("unexpected fsync path: " + req.Path)
	}
	return nil
}

func verifyCreateRequest(req *VFSRequest, expectedPath string, expectedFlags uint32, expectedMode uint32) error {
	if req.Op != OpCreate {
		return errors.New("unexpected op")
	}
	if req.Path != expectedPath {
		return errors.New("unexpected create path: " + req.Path)
	}
	if req.Flags != expectedFlags {
		return errors.New("unexpected create flags")
	}
	if req.Mode != expectedMode {
		return errors.New("unexpected create mode")
	}
	return nil
}

func TestFillAttrSymlinkMode(t *testing.T) {
	var attr fuse.Attr
	fillAttr(&attr, &VFSStat{
		Size:    0,
		Mode:    uint32(os.ModeSymlink) | 0777,
		IsDir:   false,
		Ino:     99,
		ModTime: 1700000000,
	})

	assert.Equal(t, uint32(syscall.S_IFLNK|0777), attr.Mode, "symlink must be advertised as S_IFLNK")
	assert.Equal(t, uint32(1), attr.Nlink)
}

func TestVFSRootSymlinkForwardsRequest(t *testing.T) {
	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		if req.Op != OpSymlink {
			return errors.New("unexpected op")
		}
		if req.Path != "/workspace/link" {
			return errors.New("unexpected link path: " + req.Path)
		}
		if string(req.Data) != "target" {
			return errors.New("unexpected symlink target: " + string(req.Data))
		}
		return nil
	})
	defer cleanup()

	root := &VFSRoot{client: client, basePath: "/workspace"}
	fs.NewNodeFS(root, &fs.Options{})

	var out fuse.EntryOut
	inode, errno := root.Symlink(context.Background(), "target", "link", &out)
	require.Equal(t, syscall.Errno(0), errno)
	assert.NotNil(t, inode)
	assert.Equal(t, uint32(syscall.S_IFLNK|0777), out.Attr.Mode)
}

func TestVFSNodeSymlinkForwardsRequest(t *testing.T) {
	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		if req.Op != OpSymlink {
			return errors.New("unexpected op")
		}
		if req.Path != "/workspace/dir/link" {
			return errors.New("unexpected link path: " + req.Path)
		}
		return nil
	})
	defer cleanup()

	node := &VFSNode{client: client, path: "/workspace/dir", isDir: true}
	fs.NewNodeFS(node, &fs.Options{})

	var out fuse.EntryOut
	inode, errno := node.Symlink(context.Background(), "target", "link", &out)
	require.Equal(t, syscall.Errno(0), errno)
	assert.NotNil(t, inode)
}

func TestVFSNodeReadlinkReturnsData(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- serveSingleResponse(fds[1], func(req *VFSRequest) (*VFSResponse, error) {
			if req.Op != OpReadlink {
				return nil, errors.New("unexpected op")
			}
			if req.Path != "/workspace/link" {
				return nil, errors.New("unexpected readlink path: " + req.Path)
			}
			return &VFSResponse{Data: []byte("target")}, nil
		})
	}()

	client := &VFSClient{fd: fds[0]}
	defer func() {
		_ = client.Close()
		require.NoError(t, <-done)
	}()

	node := &VFSNode{client: client, path: "/workspace/link", isDir: false}
	data, errno := node.Readlink(context.Background())
	require.Equal(t, syscall.Errno(0), errno)
	assert.Equal(t, "target", string(data))
}

func TestVFSNodeLinkForwardsRequest(t *testing.T) {
	client, cleanup := newSingleRequestClient(t, func(req *VFSRequest) error {
		if req.Op != OpLink {
			return errors.New("unexpected op")
		}
		if req.Path != "/workspace/dir/file.txt" {
			return errors.New("unexpected link target path: " + req.Path)
		}
		if req.NewPath != "/workspace/dir/hard.txt" {
			return errors.New("unexpected hard-link path: " + req.NewPath)
		}
		return nil
	})
	defer cleanup()

	node := &VFSNode{client: client, path: "/workspace/dir", isDir: true}
	fs.NewNodeFS(node, &fs.Options{})

	targetNode := &VFSNode{client: client, path: "/workspace/dir/file.txt", isDir: false}
	_ = node.NewInode(context.Background(), targetNode, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 7})

	var out fuse.EntryOut
	inode, errno := node.Link(context.Background(), targetNode, "hard.txt", &out)
	require.Equal(t, syscall.Errno(0), errno)
	assert.NotNil(t, inode)
}

func TestVFSNodeLinkKeepsSurvivingPathBothUnlinkDirections(t *testing.T) {
	// A hard link shares the target inode (same stable inode number), so go-fuse
	// coalesces the new link name onto the target's existing single VFSNode.
	// The cached path of that shared node must survive EITHER unlink direction:
	//
	//   * git publishes a loose object: create tmp, link tmp->sha, unlink tmp.
	//     After the unlink the cached path must point at sha (the surviving
	//     name), not the removed tmp path.
	//   * reverse direction: create old, link old->new, unlink NEW while OLD
	//     survives. After the unlink the cached path must still point at OLD,
	//     not at the removed NEW path.
	//
	// A fix that only repoints the cached path to the newest link name at Link
	// time fails the second direction; the node therefore registers the new name
	// as an alternate and only promotes when the current name is actually
	// removed.
	//
	// Direction 1 (git): tmp -> sha, unlink tmp.
	t.Run("git-direction-unlink-tmp", func(t *testing.T) {
		client, cleanup := newNRequestClient(t, 2, func(req *VFSRequest) error {
			if req.Op == OpLink {
				if req.Path != "/workspace/dir/tmp_obj_x" || req.NewPath != "/workspace/dir/6e91c306" {
					return errors.New("unexpected link request")
				}
				return nil
			}
			if req.Op == OpUnlink {
				if req.Path != "/workspace/dir/tmp_obj_x" {
					return errors.New("unexpected unlink request")
				}
				return nil
			}
			return errors.New("unexpected op")
		})
		defer cleanup()

		node := &VFSNode{client: client, path: "/workspace/dir", isDir: true}
		fs.NewNodeFS(node, &fs.Options{})

		targetNode := &VFSNode{client: client, path: "/workspace/dir/tmp_obj_x", isDir: false}
		targetInode := node.NewInode(context.Background(), targetNode, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 42})
		// Put the temp name in the go-fuse tree so Unlink can find the child.
		require.True(t, node.AddChild("tmp_obj_x", targetInode, true))

		var out fuse.EntryOut
		inode, errno := node.Link(context.Background(), targetNode, "6e91c306", &out)
		require.Equal(t, syscall.Errno(0), errno)
		assert.NotNil(t, inode)

		// Link registers sha as an alternate; the cached path is unchanged while
		// tmp still exists.
		assert.Equal(t, "/workspace/dir/tmp_obj_x", nodePath(targetNode))

		// Unlink tmp: after the host unlink the cached path must move to sha.
		errno = node.Unlink(context.Background(), "tmp_obj_x")
		require.Equal(t, syscall.Errno(0), errno)
		assert.Equal(t, "/workspace/dir/6e91c306", nodePath(targetNode))
	})

	// Direction 2 (reverse): old -> new, unlink NEW while OLD survives.
	t.Run("reverse-unlink-new", func(t *testing.T) {
		client, cleanup := newNRequestClient(t, 2, func(req *VFSRequest) error {
			if req.Op == OpLink {
				if req.Path != "/workspace/dir/OLD" || req.NewPath != "/workspace/dir/NEW" {
					return errors.New("unexpected link request")
				}
				return nil
			}
			if req.Op == OpUnlink {
				if req.Path != "/workspace/dir/NEW" {
					return errors.New("unexpected unlink request")
				}
				return nil
			}
			return errors.New("unexpected op")
		})
		defer cleanup()

		node := &VFSNode{client: client, path: "/workspace/dir", isDir: true}
		fs.NewNodeFS(node, &fs.Options{})

		oldNode := &VFSNode{client: client, path: "/workspace/dir/OLD", isDir: false}
		oldInode := node.NewInode(context.Background(), oldNode, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 7})
		require.True(t, node.AddChild("OLD", oldInode, true))

		var out fuse.EntryOut
		inode, errno := node.Link(context.Background(), oldNode, "NEW", &out)
		require.Equal(t, syscall.Errno(0), errno)
		assert.NotNil(t, inode)
		// Simulate go-fuse coalescing: the NEW name maps to the same shared
		// inode under the parent, so Unlink("NEW") can find the child node.
		require.True(t, node.AddChild("NEW", oldInode, true))

		// Link registers NEW as an alternate; OLD is still the cached path.
		assert.Equal(t, "/workspace/dir/OLD", nodePath(oldNode))

		// Unlink NEW: the surviving OLD name must still resolve (cached path
		// stays OLD, it is not repointed to the removed NEW).
		errno = node.Unlink(context.Background(), "NEW")
		require.Equal(t, syscall.Errno(0), errno)
		assert.Equal(t, "/workspace/dir/OLD", nodePath(oldNode))
	})
}

// TestVFSNodeLinkRegistryBothDirections exercises the node-level registry that
// backs the Link/Unlink handlers: a shared inode (hard links coalesce onto one
// VFSNode) remembers every known name, drops names as they are unlinked, and
// only promotes the cached path when the current name is actually removed. Both
// unlink directions (git temp-publish and reverse) must keep a surviving name.
func TestVFSNodeLinkRegistryBothDirections(t *testing.T) {
	newNode := func(path string) *VFSNode {
		return &VFSNode{path: path, isDir: false}
	}
	t.Run("git-direction", func(t *testing.T) {
		n := newNode("/repo/.git/objects/ab/tmp_obj_xxx")
		n.registerLinkPath("/repo/.git/objects/ab/6e91c306")
		assert.Equal(t, "/repo/.git/objects/ab/tmp_obj_xxx", n.currentPath())
		// unlink the temp name -> promote the surviving published object name.
		n.dropLinkPath("/repo/.git/objects/ab/tmp_obj_xxx")
		assert.Equal(t, "/repo/.git/objects/ab/6e91c306", n.currentPath())
	})
	t.Run("reverse-direction", func(t *testing.T) {
		n := newNode("/repo/OLD")
		n.registerLinkPath("/repo/NEW")
		assert.Equal(t, "/repo/OLD", n.currentPath())
		// unlink the NEW name while OLD survives -> cached path stays OLD.
		n.dropLinkPath("/repo/NEW")
		assert.Equal(t, "/repo/OLD", n.currentPath())
	})
	t.Run("multi-link-order", func(t *testing.T) {
		n := newNode("/repo/A")
		n.registerLinkPath("/repo/B")
		n.registerLinkPath("/repo/C")
		n.dropLinkPath("/repo/B")
		assert.Equal(t, "/repo/A", n.currentPath())
		// remove A while C survives -> promote C
		n.dropLinkPath("/repo/A")
		assert.Equal(t, "/repo/C", n.currentPath())
		n.dropLinkPath("/repo/C")
	})
	t.Run("rename-rebase-alt", func(t *testing.T) {
		n := newNode("/repo/dir1/file")
		n.registerLinkPath("/repo/dir2/file")
		n.registerLinkPath("/repo/dir2/other")
		// Rename dir2 -> dir3: the alt names under dir2 must rebase.
		n.rebasePaths("/repo/dir2", "/repo/dir3")
		assert.Equal(t, "/repo/dir1/file", n.currentPath())
		assert.Equal(t, "/repo/dir3/file", n.altPaths[0])
		assert.Equal(t, "/repo/dir3/other", n.altPaths[1])
		// Rename dir1 -> dir1b: the current path rebases too.
		n.rebasePaths("/repo/dir1", "/repo/dir1b")
		assert.Equal(t, "/repo/dir1b/file", n.currentPath())
	})
	t.Run("unlink-unknown-name-noop", func(t *testing.T) {
		n := newNode("/repo/A")
		n.registerLinkPath("/repo/B")
		n.dropLinkPath("/repo/never-existed")
		assert.Equal(t, "/repo/A", n.currentPath())
		assert.Equal(t, []string{"/repo/B"}, n.altPaths)
	})
}

func serveSingleResponse(fd int, handler func(*VFSRequest) (*VFSResponse, error)) error {
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
	resp, err := handler(&req)
	if err != nil {
		return err
	}
	if resp == nil {
		resp = &VFSResponse{}
	}
	respData, err := cbor.Marshal(resp)
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

// --- Lookup-discovered hard-link alias bookkeeping (unit level) ---

// TestVFSNodeDroppedPathPromotesOnDiscovery locks the promotion rule used when
// the cached alias of a hard-linked inode is unlinked BEFORE any other alias
// has been discovered: dropLinkPath marks the node dropped (no alternate known
// yet), and the next registerLinkPath (a Lookup discovering a pre-existing host
// alias) promotes the new name to the cached path because the old one no longer
// exists on the host.
func TestVFSNodeDroppedPathPromotesOnDiscovery(t *testing.T) {
	n := &VFSNode{path: "/repo/E", isDir: false}
	// Unlink the only known alias: no alternate to promote, node marked dropped.
	n.dropLinkPath("/repo/E")
	assert.Equal(t, "/repo/E", n.currentPath(), "cached path stays until a live name is discovered")
	// First discovery of the surviving alias F adopts it as the cached path.
	n.registerLinkPath("/repo/F")
	assert.Equal(t, "/repo/F", n.currentPath())
	assert.Empty(t, n.altPaths)
	assert.False(t, n.dropped)
}

// TestCoalescedOwnerReusesNodeAcrossParents proves that a Lookup-discovered
// alias (a hard link that pre-existed on the host) is registered on the first
// node created for the host inode even when the alias lives under a DIFFERENT
// parent directory, and that the surviving go-fuse inode is handed back so the
// kernel coalesces onto it.
func TestCoalescedOwnerReusesNodeAcrossParents(t *testing.T) {
	client, cleanup := newPathProbeClient(t, 1001)
	defer cleanup()
	root := &VFSRoot{client: client, basePath: "/ws"}
	fs.NewNodeFS(root, &fs.Options{})

	d1Node := &VFSNode{client: client, path: "/ws/d1", isDir: true}
	d1Inode := root.NewInode(context.Background(), d1Node, fs.StableAttr{Mode: syscall.S_IFDIR, Ino: 101})
	require.True(t, root.AddChild("d1", d1Inode, true))
	d2Node := &VFSNode{client: client, path: "/ws/d2", isDir: true}
	d2Inode := root.NewInode(context.Background(), d2Node, fs.StableAttr{Mode: syscall.S_IFDIR, Ino: 102})
	require.True(t, root.AddChild("d2", d2Inode, true))

	// First discovery of host inode 1001: name /ws/d1/A.
	aNode := &VFSNode{client: client, path: "/ws/d1/A", isDir: false}
	aInode := d1Inode.NewInode(context.Background(), aNode, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 1001})
	require.True(t, d1Inode.AddChild("A", aInode, true))
	client.setHardLinkOwner(1001, aNode)

	// Lookup of the cross-directory alias /ws/d2/B must reuse A's inode and
	// register B as an alternate so unlinking A keeps B resolvable. The alias
	// probe (pathHasInode) answers 1001 for the cached /ws/d1/A path, so the
	// node treats B as a genuine hard-link alias.
	reused := client.lookupOrReuse(1001, syscall.S_IFREG, "/ws/d2/B", func() (*VFSNode, *fs.Inode) {
		t.Fatal("existing canonical owner must be reused, not re-created")
		return nil, nil
	})
	require.Same(t, aInode, reused, "cross-directory alias must coalesce onto the first node")
	assert.Equal(t, []string{"/ws/d2/B"}, aNode.altPaths)

	// A repeat lookup of the cached name itself neither duplicates nor repoints.
	reused = client.lookupOrReuse(1001, syscall.S_IFREG, "/ws/d1/A", func() (*VFSNode, *fs.Inode) {
		t.Fatal("existing canonical owner must be reused, not re-created")
		return nil, nil
	})
	require.Same(t, aInode, reused)
	assert.Equal(t, []string{"/ws/d2/B"}, aNode.altPaths)
	assert.Equal(t, "/ws/d1/A", aNode.currentPath())

	// Unlinking the cached /ws/d1/A promotes the discovered /ws/d2/B.
	aNode.dropLinkPath("/ws/d1/A")
	assert.Equal(t, "/ws/d2/B", aNode.currentPath())
}

// TestReconcileRenameSameInodeKeepsBothNames covers rename A->B where A and B
// are already hard links to the same host inode (POSIX no-op: the host keeps
// both names). The shared node must keep BOTH names registered so whichever
// alias survives a later unlink still resolves.
func TestReconcileRenameSameInodeKeepsBothNames(t *testing.T) {
	client := &VFSClient{}
	root := &VFSRoot{client: client, basePath: "/ws"}
	fs.NewNodeFS(root, &fs.Options{})

	dirNode := &VFSNode{client: client, path: "/ws/dir", isDir: true}
	dirInode := root.NewInode(context.Background(), dirNode, fs.StableAttr{Mode: syscall.S_IFDIR, Ino: 201})
	require.True(t, root.AddChild("dir", dirInode, true))

	// One shared node reached through alias names A and B (same host inode).
	sharedNode := &VFSNode{client: client, path: "/ws/dir/A", isDir: false}
	sharedInode := dirInode.NewInode(context.Background(), sharedNode, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 2001})
	require.True(t, dirInode.AddChild("A", sharedInode, true))
	require.True(t, dirInode.AddChild("B", sharedInode, true))
	client.setHardLinkOwner(2001, sharedNode)
	sharedNode.registerLinkPath("/ws/dir/B")

	reconcilePathsAfterRename(dirInode, "A", "/ws/dir/A", dirInode, "B", "/ws/dir/B")

	// Both names stay tracked on the shared node.
	paths := map[string]bool{sharedNode.currentPath(): true}
	for _, a := range sharedNode.altPaths {
		paths[a] = true
	}
	assert.True(t, paths["/ws/dir/A"], "A must remain registered after the same-inode rename")
	assert.True(t, paths["/ws/dir/B"], "B must remain registered after the same-inode rename")

	// Unlink the cached A: the surviving B must promote so it keeps resolving.
	sharedNode.dropLinkPath("/ws/dir/A")
	assert.Equal(t, "/ws/dir/B", sharedNode.currentPath())
	// Unlink the last name: node is dropped (no alternate), handles keep working.
	sharedNode.dropLinkPath("/ws/dir/B")
	assert.True(t, sharedNode.dropped)
}

// TestReconcileRenameOverwriteDropsDestinationAlias covers rename A->B where B
// is an alias of a DIFFERENT inode that also has a surviving alias D. After the
// host replaces B with the moved A, the destination node must drop B (B now
// holds the moved file) and promote the surviving alias D so reads through D
// never return the moved file's content.
func TestReconcileRenameOverwriteDropsDestinationAlias(t *testing.T) {
	client := &VFSClient{}
	root := &VFSRoot{client: client, basePath: "/ws"}
	fs.NewNodeFS(root, &fs.Options{})

	dirNode := &VFSNode{client: client, path: "/ws/dir", isDir: true}
	dirInode := root.NewInode(context.Background(), dirNode, fs.StableAttr{Mode: syscall.S_IFDIR, Ino: 301})
	require.True(t, root.AddChild("dir", dirInode, true))

	// A is its own inode.
	aNode := &VFSNode{client: client, path: "/ws/dir/A", isDir: false}
	aInode := dirInode.NewInode(context.Background(), aNode, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 3001})
	require.True(t, dirInode.AddChild("A", aInode, true))

	// B and D are aliases of destination inode 3002; B is the cached path.
	destNode := &VFSNode{client: client, path: "/ws/dir/B", isDir: false}
	destInode := dirInode.NewInode(context.Background(), destNode, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 3002})
	require.True(t, dirInode.AddChild("B", destInode, true))
	require.True(t, dirInode.AddChild("D", destInode, true))
	client.setHardLinkOwner(3002, destNode)
	destNode.registerLinkPath("/ws/dir/D")

	// rename A -> B (overwrite of B).
	reconcilePathsAfterRename(dirInode, "A", "/ws/dir/A", dirInode, "B", "/ws/dir/B")

	// The moved A node's cached path follows to B.
	assert.Equal(t, "/ws/dir/B", aNode.currentPath())
	// The destination node drops B and promotes the surviving alias D.
	assert.Equal(t, "/ws/dir/D", destNode.currentPath())
	assert.NotContains(t, destNode.altPaths, "/ws/dir/B")
}

// --- host_fs coherence: cached-node validation (unit level) ---

// newTestOwnerNode builds a VFSNode whose embedded go-fuse inode is fully
// initialized with the supplied stable attr, mirroring what parent.NewInode
// does in production. lookupOrReuse validates StableAttr().Mode, so a test
// fixture that skips initialization (StableAttr zero) would look like a type
// mismatch; initialization here keeps the fixtures faithful.
func newTestOwnerNode(t *testing.T, client *VFSClient, ino uint64, mode uint32, path string) *VFSNode {
	t.Helper()
	root := &VFSRoot{}
	fs.NewNodeFS(root, &fs.Options{})
	n := &VFSNode{client: client, path: path, isDir: mode&syscall.S_IFMT == syscall.S_IFDIR}
	root.NewInode(context.Background(), n, fs.StableAttr{Mode: mode, Ino: ino})
	return n
}

// newPathProbeClient returns a VFSClient backed by a socketpair whose server
// answers every OpLookup with lookupIno (and every other op with an empty
// success), which is what lookupOrReuse's hard-link alias probe needs. The
// server runs until the client is closed; cleanup closes the client and waits
// for the server goroutine to exit. It keeps the deterministic registry tests
// off a real vsock/file descriptor.
func newPathProbeClient(t *testing.T, lookupIno uint64) (*VFSClient, func()) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- serveLookupProbe(fds[1], lookupIno)
	}()

	client := &VFSClient{fd: fds[0]}
	cleanup := func() {
		_ = client.Close()
		require.NoError(t, <-done)
	}
	return client, cleanup
}

// serveLookupProbe answers VFS requests on fd until the peer closes. OpLookup
// replies report lookupIno so pathHasInode resolves a cached path as still
// naming the inode; all other ops get a default success response.
func serveLookupProbe(fd int, lookupIno uint64) error {
	defer func() {
		_ = syscall.Close(fd)
	}()
	for {
		var lenBuf [4]byte
		if _, err := readFull(fd, lenBuf[:]); err != nil {
			// The client closed the socket; this is the normal shutdown path.
			return nil
		}
		reqData := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
		if _, err := readFull(fd, reqData); err != nil {
			return err
		}
		var req VFSRequest
		if err := cbor.Unmarshal(reqData, &req); err != nil {
			return err
		}

		resp := &VFSResponse{}
		if req.Op == OpLookup {
			resp.Stat = &VFSStat{Ino: lookupIno}
		}
		respData, err := cbor.Marshal(resp)
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
	}
}

// TestLookupOrReuse_TypeChangeDropsStaleNode locks the file type half of the
// host_fs coherence fix: when the host reused the cached inode number for an
// object of a different type (ext4 frees and reuses inodes immediately),
// lookupOrReuse must drop the stale node and build a fresh one instead of
// stamping the old S_IFMT into LOOKUP replies (persistent EIO otherwise).
func TestLookupOrReuse_TypeChangeDropsStaleNode(t *testing.T) {
	const ino = uint64(0x5191)
	client := &VFSClient{}

	fileNode := newTestOwnerNode(t, client, ino, syscall.S_IFREG, "/ws/dir/c1")
	first := client.lookupOrReuse(ino, syscall.S_IFREG, "/ws/dir/c1", sameEmbedder(fileNode))
	require.Same(t, &fileNode.Inode, first)

	// The host reused ino for a directory of the same name.
	created := false
	dirNode := newTestOwnerNode(t, client, ino, syscall.S_IFDIR, "/ws/dir/c1")
	second := client.lookupOrReuse(ino, syscall.S_IFDIR, "/ws/dir/c1", func() (*VFSNode, *fs.Inode) {
		created = true
		return dirNode, &dirNode.Inode
	})
	require.True(t, created, "a type change must drop the stale node and run create")
	require.NotSame(t, first, second, "a type change must return a fresh go-fuse inode")
	require.Same(t, &dirNode.Inode, second)
	require.Equal(t, uint32(syscall.S_IFDIR), second.StableAttr().Mode&syscall.S_IFMT)
}

// TestLookupOrReuse_DeadPathReplacesInsteadOfAliasing locks the path half of the
// fix: when the cached path no longer resolves on the host to the looked-up
// inode (a host rename or delete+recreate), lookupOrReuse must repoint the node
// at the name that exists rather than registering it as a hard-link alternate
// and leaving open/readdir to address the dead path.
func TestLookupOrReuse_DeadPathReplacesInsteadOfAliasing(t *testing.T) {
	const ino = uint64(0x2468)
	// The probe answers a different inode, so the cached path is dead.
	client, cleanup := newPathProbeClient(t, ino+1)
	defer cleanup()

	node := newTestOwnerNode(t, client, ino, syscall.S_IFREG, "/ws/dir/old-name")
	client.lookupOrReuse(ino, syscall.S_IFREG, "/ws/dir/old-name", sameEmbedder(node))
	require.Equal(t, "/ws/dir/old-name", node.currentPath())

	// A later Lookup of the object's new name finds the same ino but the cached
	// path no longer resolves to it.
	reused := client.lookupOrReuse(ino, syscall.S_IFREG, "/ws/dir/new-name", func() (*VFSNode, *fs.Inode) {
		t.Fatal("an existing (but stale) node must be repointed, not re-created")
		return nil, nil
	})
	require.Same(t, &node.Inode, reused)
	assert.Equal(t, "/ws/dir/new-name", node.currentPath(), "a dead cached path must be replaced")
	assert.Empty(t, node.altPaths, "the dead path's alternates must be discarded")
	assert.False(t, node.dropped)
}

// TestLookupOrReuse_LivePathStillRegistersAlias is the positive companion: a
// cached path that still resolves to the inode is a genuine hard-link alias and
// stays the cached path while the new name is registered as an alternate.
func TestLookupOrReuse_LivePathStillRegistersAlias(t *testing.T) {
	const ino = uint64(0x1357)
	client, cleanup := newPathProbeClient(t, ino)
	defer cleanup()

	node := newTestOwnerNode(t, client, ino, syscall.S_IFREG, "/ws/dir/A")
	client.lookupOrReuse(ino, syscall.S_IFREG, "/ws/dir/A", sameEmbedder(node))

	reused := client.lookupOrReuse(ino, syscall.S_IFREG, "/ws/dir/B", func() (*VFSNode, *fs.Inode) {
		t.Fatal("a live alias must reuse the canonical node")
		return nil, nil
	})
	require.Same(t, &node.Inode, reused)
	assert.Equal(t, "/ws/dir/A", node.currentPath())
	assert.Equal(t, []string{"/ws/dir/B"}, node.altPaths)
}
