package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRealFS_SymlinkEscapeOutsideRoot proves a guest-created or pre-existing
// symlink whose target resolves outside the mounted root cannot be followed.
func TestRealFS_SymlinkEscapeOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("TOP SECRET"), 0600))

	// Pre-existing escape: a symlink inside the root pointing at the file.
	require.NoError(t, os.Symlink(secret, filepath.Join(root, "esc.txt")))

	p := NewRealFSProvider(root).WithOwner(1000, 2000)

	// A read-only Stat through the escaping link must not leak the target.
	_, err := p.Stat("esc.txt")
	require.Error(t, err, "Stat through an escaping symlink must fail")

	// Open/read through the link must fail.
	h, err := p.Open("esc.txt", os.O_RDONLY, 0)
	require.Error(t, err, "Open through an escaping symlink must fail")
	if h != nil {
		_ = h.Close()
	}
}

// TestRealFS_GuestCreatedSymlinkEscapeOutsideRoot drives the same escape
// through the provider exactly as it would apply to a hosted worktree that
// already contains an escaping link (the VFS protocol does not dispatch
// OpSymlink, so symlinks are created on the host side before or outside the
// protocol; the provider must still confine a later traversal).
func TestRealFS_GuestCreatedSymlinkEscapeOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("TOP SECRET"), 0600))

	p := NewRealFSProvider(root)

	// Create an escaping symlink via the provider, as a guest would.
	require.NoError(t, p.Symlink(secret, "esc.txt"))

	// Symlink target must be preserved (Readlink returns the exact target).
	target, err := p.Readlink("esc.txt")
	require.NoError(t, err)
	assert.Equal(t, secret, target, "symlink target must be preserved")

	// But a lookup / open through the link must be refused.
	_, err = p.Stat("esc.txt")
	require.Error(t, err, "lookup through escaping symlink must fail")

	h, err := p.Open("esc.txt", os.O_RDONLY, 0)
	require.Error(t, err, "open through escaping symlink must fail")
	if h != nil {
		_ = h.Close()
	}
}

// TestRealFS_RelativeInRootSymlinkStillWorks ensures we did not weaken normal
// in-root behavior: a relative symlink that stays within the root is followed.
func TestRealFS_RelativeInRootSymlinkStillWorks(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "f.txt"), []byte("hello"), 0644))

	p := NewRealFSProvider(root)
	require.NoError(t, p.Symlink("sub/f.txt", "rel.txt"))

	_, err := p.Stat("rel.txt")
	require.NoError(t, err, "in-root relative symlink should be followed")
	assert.Equal(t, "hello", fileContents(t, "rel.txt", p), "in-root symlink content readable")
}

func fileContents(t *testing.T, path string, p *RealFSProvider) string {
	t.Helper()
	h, err := p.Open(path, os.O_RDONLY, 0)
	require.NoError(t, err)
	defer h.Close()
	var b [64]byte
	n, err := h.Read(b[:])
	require.NoError(t, err)
	return string(b[:n])
}

// TestRealFS_MutationsConfinedWithinRoot verifies write operations are confined
// to the root and root removal is refused.
func TestRealFS_MutationsConfinedWithinRoot(t *testing.T) {
	root := t.TempDir()
	p := NewRealFSProvider(root)

	// Create a file.
	h, err := p.Create("new.txt", 0644)
	require.NoError(t, err)
	_, err = h.Write([]byte("data"))
	require.NoError(t, err)
	require.NoError(t, h.Close())

	// Read it back.
	h2, err := p.Open("new.txt", os.O_RDONLY, 0)
	require.NoError(t, err)
	var b [64]byte
	n, _ := h2.Read(b[:])
	_ = h2.Close()
	assert.Equal(t, "data", string(b[:n]))

	// Rename within root.
	require.NoError(t, p.Rename("new.txt", "renamed.txt"))
	assert.FileExists(t, filepath.Join(root, "renamed.txt"))

	// Mkdir + Remove within root.
	require.NoError(t, p.Mkdir("subdir", 0755))
	assert.DirExists(t, filepath.Join(root, "subdir"))
	require.NoError(t, p.Remove("subdir"))
	assert.NoDirExists(t, filepath.Join(root, "subdir"))

	// Remove inside root is allowed; RemoveAll("/") refuses to destroy the root.
	require.NoError(t, p.Remove("renamed.txt"))
	require.ErrorIs(t, p.RemoveAll("/"), syscall.EBUSY, "removing the mount root must be refused")
	require.DirExists(t, root, "mount root must survive RemoveAll('/')")
}

// TestRealFS_RootRenamedAncestor tracks the mount root across a rename. This
// proves the descriptor-based root is robust against the root directory itself
// being moved, instead of re-resolving the stale host path string.
func TestRealFS_RootRenamedAncestor(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	require.NoError(t, os.MkdirAll(root, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("hello"), 0644))

	p := NewRealFSProvider(root)

	newRoot := filepath.Join(base, "root-moved")
	require.NoError(t, os.Rename(root, newRoot))

	// The provider still refers to the (moved) root directory.
	h, err := p.Open("f.txt", os.O_RDONLY, 0)
	require.NoError(t, err, "opening through a renamed root must still work")
	defer h.Close()
	var b [64]byte
	n, _ := h.Read(b[:])
	assert.Equal(t, "hello", string(b[:n]))
}

// TestRealFS_SingleFileMount treats a regular-file source as a single-file
// mount whose root maps to exactly that file.
func TestRealFS_SingleFileMount(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("note"), 0644))

	p := NewRealFSProvider(file)

	info, err := p.Stat("/")
	require.NoError(t, err)
	assert.False(t, info.IsDir(), "a file source must not be reported as a directory")

	// Reading the mount root returns the file's bytes.
	h, err := p.Open("/", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer h.Close()
	var b [64]byte
	n, _ := h.Read(b[:])
	assert.Equal(t, "note", string(b[:n]))
}

// TestRealFS_MutationThroughEscapingSymlink proves a write operation that
// would traverse an escaping symlink is refused, so a guest-created (or
// pre-existing) link cannot be used to create, open-with-O_CREATE, or mkdir a
// file outside the root.
func TestRealFS_MutationThroughEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "dir"), 0755))
	// An escaping symlink inside the root pointing at an outside directory.
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "esc")))

	p := NewRealFSProvider(root)

	// Create through the link must NOT create a file outside.
	_, err := p.Create("esc/newfile.txt", 0644)
	require.Error(t, err, "Create through escaping symlink must fail")
	assert.NoFileExists(t, filepath.Join(outside, "newfile.txt"), "must not create outside the root")

	// Open with O_CREATE through the link must fail.
	_, err = p.Open("esc/aaa.txt", os.O_RDONLY|os.O_CREATE, 0644)
	require.Error(t, err, "Open with O_CREATE through escaping symlink must fail")
	assert.NoFileExists(t, filepath.Join(outside, "aaa.txt"), "must not create outside the root")

	// Mkdir through the link must fail.
	require.Error(t, p.Mkdir("esc/newdir", 0755), "Mkdir through escaping symlink must fail")
	assert.NoDirExists(t, filepath.Join(outside, "newdir"))
}

// TestRealFS_RootAncestorRenamed proves the descriptor-based root survives a
// rename of an ancestor of the mounted root (not just the root itself).
func TestRealFS_RootAncestorRenamed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	require.NoError(t, os.MkdirAll(root, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("hello"), 0644))

	p := NewRealFSProvider(root)

	newBase := filepath.Join(t.TempDir(), "moved-base")
	require.NoError(t, os.Rename(base, newBase))

	h, err := p.Open("f.txt", os.O_RDONLY, 0)
	require.NoError(t, err, "open through a renamed ancestor must still work")
	defer h.Close()
	var b [64]byte
	n, _ := h.Read(b[:])
	assert.Equal(t, "hello", string(b[:n]))
}

// TestRealFS_RootAncestorReplacedWithSymlink proves that replacing an ancestor
// of the mounted root with a symlink to an outside directory does not make the
// provider follow it: the pinned directory fd is used, so the original (moved)
// directory is read instead of the outside target.
func TestRealFS_RootAncestorReplacedWithSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	require.NoError(t, os.MkdirAll(root, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("hello"), 0644))

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "f.txt"), []byte("TOP SECRET"), 0644))

	p := NewRealFSProvider(root)

	// Move the real ancestor away and replace its path with a symlink to an
	// outside directory containing a same-named secret file.
	require.NoError(t, os.Rename(base, base+"-orig"))
	require.NoError(t, os.Symlink(outside, base))

	h, err := p.Open("f.txt", os.O_RDONLY, 0)
	require.NoError(t, err, "open through a symlink-replaced ancestor must use the pinned dir")
	var b [64]byte
	n, _ := h.Read(b[:])
	_ = h.Close()
	assert.Equal(t, "hello", string(b[:n]), "must read the pinned dir file, not the outside secret")
}

// TestRealFS_SingleFileSourceReplacedWithSymlink proves a single-file mount
// cannot be redirected to an outside file by replacing the admitted source with
// a symlink after construction.
func TestRealFS_SingleFileSourceReplacedWithSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("TOP SECRET"), 0600))

	file := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("note"), 0644))

	p := NewRealFSProvider(file)

	// Replace the admitted source with a symlink to the outside secret.
	require.NoError(t, os.Remove(file))
	require.NoError(t, os.Symlink(secret, file))

	_, err := p.Stat("/")
	require.Error(t, err, "Stat must fail after source replaced with symlink")

	h, err := p.Open("/", os.O_RDONLY, 0)
	require.Error(t, err, "Open must fail and must NOT return outside contents")
	if h != nil {
		_ = h.Close()
	}

	_, err = p.Create("/", 0644)
	require.Error(t, err, "Create must fail after replacement")
	require.Error(t, p.Chmod("/", 0600), "Chmod must fail after replacement")
	require.Error(t, p.Fsync("/"), "Fsync must fail after replacement")
	require.Error(t, p.Remove("/"), "Remove must fail after replacement")
	_, err = p.Readlink("/")
	require.Error(t, err, "Readlink must fail after replacement")
}

// TestRealFS_SingleFileRejectsNonRootPath proves a single-file mount rejects
// any operation naming a path other than the mount root, instead of silently
// operating on the file (the old open-time re-resolution hole).
func TestRealFS_SingleFileRejectsNonRootPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("note"), 0644))
	p := NewRealFSProvider(file)

	_, err := p.Open("/some/subpath/not/real", os.O_RDONLY, 0)
	require.Error(t, err, "Open non-root path must fail")
	assert.True(t, errors.Is(err, syscall.ENOTDIR), "want ENOTDIR, got %v", err)

	_, err = p.Create("/unexpected/create", 0644)
	require.Error(t, err, "Create non-root path must fail")
	assert.True(t, errors.Is(err, syscall.ENOTDIR), "want ENOTDIR, got %v", err)

	require.Error(t, p.Fsync("/not/root"), "Fsync non-root path must fail")
	_, err = p.Stat("/not/root")
	require.Error(t, err, "Stat non-root path must fail")
}

// TestRealFS_SingleFileMissingRootFailsClosed proves that once the admitted
// source is removed, operations fail closed instead of creating or serving a
// freshly-created inode.
func TestRealFS_SingleFileMissingRootFailsClosed(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("note"), 0644))
	p := NewRealFSProvider(file)

	require.NoError(t, os.Remove(file))

	_, err := p.Stat("/")
	require.Error(t, err, "Stat must fail when root is missing")
	_, err = p.Open("/", os.O_RDONLY, 0)
	require.Error(t, err, "Open must fail when root is missing")
	_, err = p.Create("/", 0644)
	require.Error(t, err, "Create must fail (must not recreate a new inode) when root is missing")
	assert.NoFileExists(t, file, "Create must not silently recreate the missing root")
}

// TestRealFS_SingleFileRenamedParent proves a single-file mount refers to the
// admitted file (via its pinned parent fd) even when the parent directory is
// renamed after construction.
func TestRealFS_SingleFileRenamedParent(t *testing.T) {
	base := t.TempDir()
	orig := filepath.Join(base, "orig")
	require.NoError(t, os.MkdirAll(orig, 0755))
	file := filepath.Join(orig, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("note"), 0644))

	p := NewRealFSProvider(file)
	require.NoError(t, os.Rename(orig, filepath.Join(base, "moved")))

	h, err := p.Open("/", os.O_RDONLY, 0)
	require.NoError(t, err, "open through renamed parent must still work")
	defer h.Close()
	var b [64]byte
	n, _ := h.Read(b[:])
	assert.Equal(t, "note", string(b[:n]))
}

// TestRealFS_SingleFileCloseLifecycle verifies that after Close a single-file
// mount no longer serves the admitted file (all operations fail).
func TestRealFS_SingleFileCloseLifecycle(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("note"), 0644))
	p := NewRealFSProvider(file)

	require.NoError(t, p.Close())
	_, err := p.Stat("/")
	require.Error(t, err, "Stat after Close must fail")
	_, err = p.Open("/", os.O_RDONLY, 0)
	require.Error(t, err, "Open after Close must fail")
	_, err = p.Create("/", 0644)
	require.Error(t, err, "Create after Close must fail")
}

// TestRealFS_CloseLifecycle verifies os.Root handle closure: after Close, all
// operations fail and CloseProvider recurses through a ReadonlyProvider.
func TestRealFS_CloseLifecycle(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("hi"), 0644))

	wrapped := NewReadonlyProvider(NewRealFSProvider(root))
	require.NoError(t, CloseProvider(wrapped), "closing a readonly-wrapped realfs provider")

	p := NewRealFSProvider(root)
	require.NoError(t, p.Close())
	_, err := p.Stat("f.txt")
	require.Error(t, err, "operations after Close must fail")
}

// TestRealFS_MountedRootCloseThroughRouter verifies CloseProvider walks the
// router and closes every mounted provider's root handle.
func TestRealFS_MountedRootCloseThroughRouter(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	router := NewMountRouter(map[string]Provider{
		"/a": NewRealFSProvider(rootA),
		"/b": NewRealFSProvider(rootB),
	})
	require.NoError(t, CloseProvider(router))
}
