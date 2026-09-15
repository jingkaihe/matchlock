package vfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// RealFSProvider serves host_fs mounts.
//
// A host_fs mount may name a directory or a single regular file.
//
// For a directory root every operation is confined beneath that directory
// using os.Root, so path components that traverse outside the configured root
// (for example through a symlink, a renamed ancestor, or a guest-created link)
// are rejected instead of escaping. os.Root is descriptor-based (openat
// relative to the root directory fd), so a mount whose root is renamed after
// construction keeps referring to the original directory in its new location
// rather than re-resolving a stale path string.
//
// For a single regular-file root the parent directory is pinned with os.Root
// and the leaf device/inode identity is captured at construction. Every
// operation resolves the leaf relative to the pinned parent and verifies it is
// still the admitted file, so replacing the source (or an ancestor) with a
// symlink or a different file after construction is refused instead of being
// followed. Because os.Root refuses symlinks that resolve outside the parent
// directory and the identity check rejects any other replacement, an escaping
// link cannot be followed. A file has no children, so any operation naming a
// non-root path is rejected.
type RealFSProvider struct {
	root     string
	isDir    bool
	rootDir  *os.Root
	rootErr  error
	ownerUID *uint32
	ownerGID *uint32

	// callerUID/callerGID are the requesting caller's identity, supplied per
	// request via withCaller. They are reported in preference to the 0/0
	// default (but below an explicit WithOwner override) so a non-root host
	// caller sees its own ownership through the FUSE mount.
	callerUID *uint32
	callerGID *uint32

	// Single-file mode state.
	parentRoot *os.Root
	leaf       string
	leafDev    uint64
	leafIno    uint64
}

// NewRealFSProvider opens the named root directory and confines every
// subsequent operation beneath it. If the root cannot be opened, the provider
// records the error and every operation returns it. A root that is a regular
// file (as opposed to a directory) selects single-file mode. The constructor
// keeps its nil-return signature for compatibility with existing callers.
func NewRealFSProvider(root string) *RealFSProvider {
	p := &RealFSProvider{root: root}
	if root == "" {
		p.rootErr = syscall.ENOENT
		return p
	}
	info, err := os.Stat(root)
	if err != nil {
		p.rootErr = err
		return p
	}
	if info.IsDir() {
		p.isDir = true
		p.rootDir, p.rootErr = os.OpenRoot(root)
		return p
	}

	// Single-file mode: pin the parent directory with os.Root and capture the
	// leaf identity so a later source/ancestor replacement cannot escape the
	// admitted file. Fail closed if the parent cannot be opened or the leaf
	// identity cannot be pinned.
	parent := filepath.Dir(root)
	leaf := filepath.Base(root)
	if leaf == "." || leaf == string(filepath.Separator) || leaf == "" {
		p.rootErr = syscall.ENOENT
		return p
	}
	p.parentRoot, p.rootErr = os.OpenRoot(parent)
	if p.rootErr != nil {
		return p
	}
	p.leaf = leaf

	// Open the admitted leaf and pin its device/inode. os.Root.OpenFile refuses
	// a symlink that resolves outside the parent, so a source that is already a
	// symlink is resolved once against the pinned parent and its target identity
	// is what is pinned; a later replacement still fails the identity check.
	f, err := p.parentRoot.OpenFile(leaf, os.O_RDONLY, 0)
	if err != nil {
		p.closeFileRoot()
		p.rootErr = err
		return p
	}
	lf, err := f.Stat()
	_ = f.Close()
	if err != nil {
		p.closeFileRoot()
		p.rootErr = err
		return p
	}
	if lf.IsDir() {
		p.closeFileRoot()
		p.rootErr = syscall.EISDIR
		return p
	}
	st, ok := lf.Sys().(*syscall.Stat_t)
	if !ok {
		p.closeFileRoot()
		p.rootErr = syscall.EINVAL
		return p
	}
	p.leafDev = uint64(st.Dev)
	p.leafIno = st.Ino
	return p
}

// closeFileRoot releases the single-file parent root, marking the provider as
// unusable. It is only called during construction when pinning fails.
func (p *RealFSProvider) closeFileRoot() {
	if p.parentRoot != nil {
		_ = p.parentRoot.Close()
		p.parentRoot = nil
	}
}

// WithOwner sets a fixed uid and gid reported for all files in this mount,
// overriding the actual host ownership. Returns the receiver for chaining.
func (p *RealFSProvider) WithOwner(uid, gid uint32) *RealFSProvider {
	p.ownerUID = &uid
	p.ownerGID = &gid
	return p
}

// withCaller returns a shallow clone that reports the given caller uid/gid for
// host files without an explicit WithOwner override. The clone shares the
// underlying os.Root handles (descriptor-based, safe for concurrent use), so no
// host file is reopened. This is how the VFS server carries the FUSE caller's
// identity (VFSRequest.UID/GID) into the stat it returns, so a non-root caller
// sees its own ownership through the mount instead of a root-owned view.
func (p *RealFSProvider) withCaller(uid, gid int) Provider {
	if p == nil {
		return nil
	}
	clone := *p
	u := uint32(uid)
	g := uint32(gid)
	clone.callerUID = &u
	clone.callerGID = &g
	return &clone
}

// effectiveOwner resolves the uid/gid this provider reports for a stat. An
// explicit WithOwner override always wins; otherwise the caller identity from
// withCaller is used; when neither is configured the nil pointers preserve the
// upstream host_fs default of 0/0.
func (p *RealFSProvider) effectiveOwner() (uid, gid *uint32) {
	if p.ownerUID != nil || p.ownerGID != nil {
		return p.ownerUID, p.ownerGID
	}
	return p.callerUID, p.callerGID
}

// Close releases the root handle (directory or single-file parent) obtained at
// construction. It is safe to call multiple times; later calls are a no-op.
func (p *RealFSProvider) Close() error {
	if p == nil {
		return nil
	}
	var err error
	if p.rootDir != nil {
		err = p.rootDir.Close()
		p.rootDir = nil
	}
	if p.parentRoot != nil {
		if cerr := p.parentRoot.Close(); err == nil {
			err = cerr
		}
		p.parentRoot = nil
	}
	return err
}

func applyOwnerPtrs(fi FileInfo, uid, gid *uint32) FileInfo {
	if uid == nil && gid == nil {
		return fi
	}
	u := fi.UID()
	g := fi.GID()
	if uid != nil {
		u = *uid
	}
	if gid != nil {
		g = *gid
	}
	return fi.WithOwner(u, g)
}

func (p *RealFSProvider) applyOwner(fi FileInfo) FileInfo {
	uid, gid := p.effectiveOwner()
	return applyOwnerPtrs(fi, uid, gid)
}

// newHandle wraps an opened file, carrying the ownership the provider should
// report for handle-based stats (OpCreate/OpGetattr-by-handle). An explicit
// WithOwner override wins; otherwise the caller identity is used; with neither,
// the handle reports the provider default 0/0.
func (p *RealFSProvider) newHandle(f *os.File) *realHandle {
	uid, gid := p.effectiveOwner()
	return &realHandle{file: f, ownerUID: uid, ownerGID: gid}
}

func (p *RealFSProvider) Readonly() bool { return false }

// rootPath normalizes the router-relative guest path into the bare,
// non-absolute name form that os.Root methods require. os.Root rejects
// absolute names ("path escapes from parent"), so the leading "/" that the
// MountRouter exposes for the mount root and its descendants is stripped and
// the empty/root name is represented as ".".
func (p *RealFSProvider) rootPath(path string) string {
	path = filepath.Clean(path)
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "."
	}
	return path
}

// isMountRoot reports whether a router-relative path refers to the mount root
// itself. Used by the single-file mode, where only the root is meaningful.
func isMountRoot(path string) bool {
	c := filepath.Clean(path)
	return c == "." || c == "/" || c == ""
}

func (p *RealFSProvider) requireRoot() (*os.Root, error) {
	if p == nil {
		return nil, syscall.ENOENT
	}
	if p.rootErr != nil {
		return nil, p.rootErr
	}
	if p.rootDir == nil {
		return nil, syscall.ENOENT
	}
	return p.rootDir, nil
}

// requireFileRoot returns the pinned parent root and leaf name for single-file
// mode, or an error when the provider is not usable.
func (p *RealFSProvider) requireFileRoot() (*os.Root, string, error) {
	if p == nil {
		return nil, "", syscall.ENOENT
	}
	if p.rootErr != nil {
		return nil, "", p.rootErr
	}
	if p.parentRoot == nil {
		return nil, "", syscall.ENOENT
	}
	return p.parentRoot, p.leaf, nil
}

// leafMatches reports whether fi refers to the admitted file the provider was
// constructed for, by comparing the device/inode identity captured at
// construction. A symlink or a differently-inoded file does not match, so a
// source replaced after construction is refused.
func (p *RealFSProvider) leafMatches(fi os.FileInfo) bool {
	if fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return uint64(st.Dev) == p.leafDev && st.Ino == p.leafIno
}

// openLeaf opens the admitted single-file leaf relative to the pinned parent
// and verifies it is still the pinned file. Because os.Root refuses symlinks
// that resolve outside the parent and the identity check rejects any other
// replacement, a source swapped after construction is never followed to an
// outside file. Callers must NOT pass O_CREATE/O_EXCL here to avoid creating a
// new inode that would not match the pinned source.
func (p *RealFSProvider) openLeaf(flags int, mode os.FileMode) (*os.File, error) {
	parent, leaf, err := p.requireFileRoot()
	if err != nil {
		return nil, err
	}
	f, err := parent.OpenFile(leaf, flags, mode)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !p.leafMatches(fi) {
		_ = f.Close()
		return nil, syscall.EBUSY
	}
	return f, nil
}

func (p *RealFSProvider) Stat(path string) (FileInfo, error) {
	if !p.isDir {
		return p.fileStat(path)
	}
	root, err := p.requireRoot()
	if err != nil {
		return FileInfo{}, err
	}
	info, err := root.Stat(p.rootPath(path))
	if err != nil {
		return FileInfo{}, err
	}
	return p.applyOwner(NewFileInfoWithSys(info.Name(), info.Size(), info.Mode(), info.ModTime(), info.IsDir(), info.Sys())), nil
}

func (p *RealFSProvider) ReadDir(path string) ([]DirEntry, error) {
	if !p.isDir {
		return nil, syscall.ENOTDIR
	}
	root, err := p.requireRoot()
	if err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(root.FS(), p.rootPath(path))
	if err != nil {
		return nil, err
	}

	result := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		result = append(result, NewDirEntry(
			e.Name(),
			e.IsDir(),
			info.Mode(),
			p.applyOwner(NewFileInfoWithSys(e.Name(), info.Size(), info.Mode(), info.ModTime(), e.IsDir(), info.Sys())),
		))
	}
	return result, nil
}

func (p *RealFSProvider) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	if !p.isDir {
		if !isMountRoot(path) {
			return nil, syscall.ENOTDIR
		}
		// Open must not create or truncate a file that no longer matches the
		// pinned source; creation/truncation is handled by Create below.
		f, err := p.openLeaf(flags&^(os.O_CREATE|os.O_TRUNC), mode)
		if err != nil {
			return nil, err
		}
		return p.newHandle(f), nil
	}
	root, err := p.requireRoot()
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(p.rootPath(path), flags, mode)
	if err != nil {
		return nil, err
	}
	return p.newHandle(f), nil
}

func (p *RealFSProvider) Create(path string, mode os.FileMode) (Handle, error) {
	if !p.isDir {
		if !isMountRoot(path) {
			return nil, syscall.ENOTDIR
		}
		// Truncate the admitted file in place. Fail closed if it was removed or
		// replaced: a recreated or swapped file no longer matches the pinned
		// source and must not be silently served.
		f, err := p.openLeaf(os.O_RDWR|os.O_TRUNC, mode)
		if err != nil {
			return nil, err
		}
		return p.newHandle(f), nil
	}
	root, err := p.requireRoot()
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(p.rootPath(path), os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
	if err != nil {
		return nil, err
	}
	return p.newHandle(f), nil
}

func (p *RealFSProvider) Mkdir(path string, mode os.FileMode) error {
	if !p.isDir {
		return syscall.ENOTDIR
	}
	root, err := p.requireRoot()
	if err != nil {
		return err
	}
	return root.Mkdir(p.rootPath(path), mode)
}

func (p *RealFSProvider) Chmod(path string, mode os.FileMode) error {
	if !p.isDir {
		if !isMountRoot(path) {
			return syscall.ENOTDIR
		}
		f, err := p.openLeaf(os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		// fchmod on the opened (identity-verified) handle avoids os.Root.Chmod's
		// regular-file-to-symlink swap race.
		return f.Chmod(mode)
	}
	root, err := p.requireRoot()
	if err != nil {
		return err
	}
	return root.Chmod(p.rootPath(path), mode)
}

func (p *RealFSProvider) Remove(path string) error {
	if !p.isDir {
		if !isMountRoot(path) {
			return syscall.ENOTDIR
		}
		parent, leaf, err := p.requireFileRoot()
		if err != nil {
			return err
		}
		fi, err := parent.Lstat(leaf)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return syscall.ELOOP
		}
		if !p.leafMatches(fi) {
			return syscall.EBUSY
		}
		return parent.Remove(leaf)
	}
	root, err := p.requireRoot()
	if err != nil {
		return err
	}
	return root.Remove(p.rootPath(path))
}

func (p *RealFSProvider) RemoveAll(path string) error {
	if !p.isDir {
		if isMountRoot(path) {
			return syscall.EBUSY
		}
		return syscall.ENOTDIR
	}
	root, err := p.requireRoot()
	if err != nil {
		return err
	}
	// Never allow a VFS request to remove the mount root itself: deleting the
	// entire host-fs fixture from below a live harness would destroy the
	// admitted RW root. Subdirectory removal stays fully supported.
	if p.rootPath(path) == "." {
		return syscall.EBUSY
	}
	return root.RemoveAll(p.rootPath(path))
}

func (p *RealFSProvider) Rename(oldPath, newPath string) error {
	if !p.isDir {
		return syscall.ENOTDIR
	}
	root, err := p.requireRoot()
	if err != nil {
		return err
	}
	return root.Rename(p.rootPath(oldPath), p.rootPath(newPath))
}

func (p *RealFSProvider) Symlink(target, link string) error {
	if !p.isDir {
		return syscall.ENOTDIR
	}
	root, err := p.requireRoot()
	if err != nil {
		return err
	}
	// os.Root permits creating a symlink whose stored target is any string
	// (including absolute), matching the historical "preserve the target"
	// behavior of the path-joined provider. Confinement is enforced on
	// traversal: a later operation that follows a link escaping the root
	// returns "path escapes from parent" instead of reading outside it.
	return root.Symlink(target, p.rootPath(link))
}

func (p *RealFSProvider) Readlink(path string) (string, error) {
	if !p.isDir {
		if !isMountRoot(path) {
			return "", syscall.ENOTDIR
		}
		parent, leaf, err := p.requireFileRoot()
		if err != nil {
			return "", err
		}
		fi, err := parent.Lstat(leaf)
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			// The admitted source was replaced with a symlink; do not reveal its
			// target (which could point outside the root).
			return "", syscall.ELOOP
		}
		// A single-file root is a regular file, not a symlink.
		return "", syscall.EINVAL
	}
	root, err := p.requireRoot()
	if err != nil {
		return "", err
	}
	return root.Readlink(p.rootPath(path))
}

func (p *RealFSProvider) Fsync(path string) error {
	if !p.isDir {
		if !isMountRoot(path) {
			return syscall.ENOTDIR
		}
		f, err := p.openLeaf(os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	}
	root, err := p.requireRoot()
	if err != nil {
		return err
	}
	f, err := root.Open(p.rootPath(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// fileStat implements Stat for single-file mounts: the only meaningful path is
// the mount root, which maps to the admitted host file.
func (p *RealFSProvider) fileStat(path string) (FileInfo, error) {
	if !isMountRoot(path) {
		return FileInfo{}, syscall.ENOTDIR
	}
	parent, leaf, err := p.requireFileRoot()
	if err != nil {
		return FileInfo{}, err
	}
	fi, err := parent.Lstat(leaf)
	if err != nil {
		return FileInfo{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return FileInfo{}, syscall.ELOOP
	}
	if !p.leafMatches(fi) {
		return FileInfo{}, syscall.EBUSY
	}
	return p.applyOwner(NewFileInfoWithSys(fi.Name(), fi.Size(), fi.Mode(), fi.ModTime(), fi.IsDir(), fi.Sys())), nil
}

type realHandle struct {
	file     *os.File
	ownerUID *uint32
	ownerGID *uint32
}

func (h *realHandle) Read(p []byte) (int, error)                { return h.file.Read(p) }
func (h *realHandle) ReadAt(p []byte, off int64) (int, error)   { return h.file.ReadAt(p, off) }
func (h *realHandle) Write(p []byte) (int, error)               { return h.file.Write(p) }
func (h *realHandle) WriteAt(p []byte, off int64) (int, error)  { return h.file.WriteAt(p, off) }
func (h *realHandle) Seek(off int64, whence int) (int64, error) { return h.file.Seek(off, whence) }
func (h *realHandle) Close() error                              { return h.file.Close() }
func (h *realHandle) Sync() error                               { return h.file.Sync() }
func (h *realHandle) Truncate(size int64) error                 { return h.file.Truncate(size) }

func (h *realHandle) Stat() (FileInfo, error) {
	info, err := h.file.Stat()
	if err != nil {
		return FileInfo{}, err
	}
	fi := NewFileInfoWithSys(info.Name(), info.Size(), info.Mode(), info.ModTime(), info.IsDir(), info.Sys())
	return applyOwnerPtrs(fi, h.ownerUID, h.ownerGID), nil
}
