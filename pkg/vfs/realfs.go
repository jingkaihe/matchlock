package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type RealFSProvider struct {
	root     string
	ownerUID *uint32
	ownerGID *uint32
}

func NewRealFSProvider(root string) *RealFSProvider {
	return &RealFSProvider{root: root}
}

// WithOwner sets a fixed uid and gid reported for all files in this mount,
// overriding the actual host ownership. Returns the receiver for chaining.
func (p *RealFSProvider) WithOwner(uid, gid uint32) *RealFSProvider {
	p.ownerUID = &uid
	p.ownerGID = &gid
	return p
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
	return applyOwnerPtrs(fi, p.ownerUID, p.ownerGID)
}

func (p *RealFSProvider) Readonly() bool { return false }

func (p *RealFSProvider) realPath(path string) string {
	return filepath.Join(p.root, filepath.Clean(path))
}

// Stat uses lstat semantics: when path is a symlink, it returns information
// about the link itself, not its target. FUSE Getattr/Lookup require this so
// the kernel can dispatch Readlink rather than transparently follow the link.
// Callers that need follow-symlink semantics should Open the path and Stat
// the handle.
func (p *RealFSProvider) Stat(path string) (FileInfo, error) {
	info, err := os.Lstat(p.realPath(path))
	if err != nil {
		return FileInfo{}, err
	}
	return p.applyOwner(NewFileInfoWithSys(info.Name(), info.Size(), info.Mode(), info.ModTime(), info.IsDir(), info.Sys())), nil
}

func (p *RealFSProvider) ReadDir(path string) ([]DirEntry, error) {
	entries, err := os.ReadDir(p.realPath(path))
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

// Open refuses to follow a symlink at the final path component (O_NOFOLLOW).
// FUSE normally dispatches Readlink for symlink-typed inodes, so Open is only
// expected on regular files; rejecting the symlink case removes a
// confused-deputy path if any caller delivers a guest-influenced path here.
func (p *RealFSProvider) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	f, err := os.OpenFile(p.realPath(path), flags|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	return &realHandle{file: f, ownerUID: p.ownerUID, ownerGID: p.ownerGID}, nil
}

func (p *RealFSProvider) Create(path string, mode os.FileMode) (Handle, error) {
	f, err := os.OpenFile(p.realPath(path), os.O_CREATE|os.O_RDWR|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	return &realHandle{file: f, ownerUID: p.ownerUID, ownerGID: p.ownerGID}, nil
}

func (p *RealFSProvider) Mkdir(path string, mode os.FileMode) error {
	return os.Mkdir(p.realPath(path), mode)
}

// Chmod refuses to operate on a symlink. Linux does not support changing the
// mode of a symlink (fchmodat with AT_SYMLINK_NOFOLLOW returns ENOTSUP), and
// the default os.Chmod follows the link — which would silently chmod the
// target. Return EPERM so callers see an explicit refusal.
func (p *RealFSProvider) Chmod(path string, mode os.FileMode) error {
	realPath := p.realPath(path)
	info, err := os.Lstat(realPath)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return syscall.EPERM
	}
	return os.Chmod(realPath, mode)
}

func (p *RealFSProvider) Remove(path string) error {
	return os.Remove(p.realPath(path))
}

func (p *RealFSProvider) RemoveAll(path string) error {
	return os.RemoveAll(p.realPath(path))
}

func (p *RealFSProvider) Rename(oldPath, newPath string) error {
	return os.Rename(p.realPath(oldPath), p.realPath(newPath))
}

// Symlink rejects targets that, when resolved relative to the link's parent,
// escape the provider root. The link's content is opaque to FUSE (the guest
// kernel resolves targets in its own namespace), but any host-side tool that
// later walks the workspace would dereference an unconstrained target —
// turning guest-writable symlinks into a host read/write-anywhere primitive.
func (p *RealFSProvider) Symlink(target, link string) error {
	linkPath := p.realPath(link)
	if err := validateSymlinkTarget(p.root, linkPath, target); err != nil {
		return err
	}
	return os.Symlink(target, linkPath)
}

func validateSymlinkTarget(root, linkPath, target string) error {
	if filepath.IsAbs(target) {
		return syscall.EPERM
	}
	rootClean := filepath.Clean(root)
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), target))
	if resolved == rootClean {
		return nil
	}
	if !strings.HasPrefix(resolved, rootClean+string(filepath.Separator)) {
		return syscall.EPERM
	}
	return nil
}

func (p *RealFSProvider) Readlink(path string) (string, error) {
	return os.Readlink(p.realPath(path))
}

func (p *RealFSProvider) Fsync(path string) error {
	f, err := os.Open(p.realPath(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
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
