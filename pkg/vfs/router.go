package vfs

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type MountRouter struct {
	mounts []mount
}

type mount struct {
	path     string
	provider Provider
}

func NewMountRouter(mounts map[string]Provider) *MountRouter {
	r := &MountRouter{}
	for path, provider := range mounts {
		r.mounts = append(r.mounts, mount{path: filepath.Clean(path), provider: provider})
	}
	sort.Slice(r.mounts, func(i, j int) bool {
		return len(r.mounts[i].path) > len(r.mounts[j].path)
	})
	return r
}

// withCaller returns a clone whose mount providers carry the requesting caller
// identity where they support it. The VFS server invokes this per request so a
// host_fs stat through the router reports the FUSE caller's ownership instead
// of the provider's 0/0 default (an explicit WithOwner override still wins).
// Only the lightweight provider wrappers are copied; the underlying roots are
// shared.
func (r *MountRouter) withCaller(uid, gid int) Provider {
	if r == nil {
		return nil
	}
	clone := &MountRouter{mounts: make([]mount, len(r.mounts))}
	for i, m := range r.mounts {
		p := m.provider
		if aware, ok := p.(interface {
			withCaller(uid, gid int) Provider
		}); ok {
			p = aware.withCaller(uid, gid)
		}
		clone.mounts[i] = mount{path: m.path, provider: p}
	}
	return clone
}

func (r *MountRouter) Readonly() bool { return false }

func (r *MountRouter) resolve(path string) (Provider, string, error) {
	path = filepath.Clean(path)
	for _, m := range r.mounts {
		if path == m.path || strings.HasPrefix(path, m.path+"/") {
			rel := strings.TrimPrefix(path, m.path)
			if rel == "" {
				rel = "/"
			}
			return m.provider, rel, nil
		}
	}
	return nil, "", syscall.ENOENT
}

func (r *MountRouter) Stat(path string) (FileInfo, error) {
	path = filepath.Clean(path)
	p, rel, err := r.resolve(path)
	if err != nil {
		if r.hasDescendantMount(path) {
			return syntheticDirInfo(filepath.Base(path)), nil
		}
		return FileInfo{}, err
	}
	info, err := p.Stat(rel)
	if err == nil {
		return info, nil
	}
	if !isMissingPathError(err) || !r.hasDescendantMount(path) {
		return FileInfo{}, err
	}
	return syntheticDirInfo(filepath.Base(path)), nil
}

func (r *MountRouter) ReadDir(path string) ([]DirEntry, error) {
	path = filepath.Clean(path)
	p, rel, err := r.resolve(path)
	var (
		entries []DirEntry
		readErr error
	)
	if err != nil {
		readErr = err
	} else {
		entries, readErr = p.ReadDir(rel)
		if readErr != nil && !isMissingPathError(readErr) {
			return nil, readErr
		}
		if readErr != nil {
			entries = nil
		}
	}

	mountEntries := r.descendantMountEntries(path)
	if readErr != nil && len(mountEntries) == 0 {
		return nil, readErr
	}

	// Merge mountpoint entries so nested mounts are visible in directory
	// listings even when intermediate directories don't exist in the base
	// provider (for example /workspace/.host/example).
	byName := make(map[string]DirEntry, len(entries)+len(mountEntries))
	baseNames := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries)+len(mountEntries))
	for _, e := range entries {
		name := e.Name()
		byName[name] = e
		if !seen[name] {
			baseNames = append(baseNames, name)
			seen[name] = true
		}
	}

	var mountOnlyNames []string
	for name, entry := range mountEntries {
		byName[name] = entry
		if !seen[name] {
			mountOnlyNames = append(mountOnlyNames, name)
			seen[name] = true
		}
	}

	sort.Strings(mountOnlyNames)
	names := append(baseNames, mountOnlyNames...)

	merged := make([]DirEntry, 0, len(names))
	for _, name := range names {
		merged = append(merged, byName[name])
	}
	return merged, nil
}

func (r *MountRouter) descendantMountEntries(path string) map[string]DirEntry {
	entries := make(map[string]DirEntry)
	for _, m := range r.mounts {
		rel, ok := relativeDescendantPath(m.path, path)
		if !ok {
			continue
		}

		name := rel
		hasNested := false
		if idx := strings.Index(rel, "/"); idx >= 0 {
			name = rel[:idx]
			hasNested = true
		}
		if name == "" {
			continue
		}

		if hasNested {
			entries[name] = syntheticDirEntry(name)
			continue
		}

		if existing, ok := entries[name]; ok && existing.IsDir() {
			continue
		}

		info, err := m.provider.Stat("/")
		if err != nil {
			entries[name] = syntheticDirEntry(name)
			continue
		}
		entryInfo := NewFileInfo(name, info.Size(), info.Mode(), info.ModTime(), info.IsDir())
		entries[name] = NewDirEntry(name, info.IsDir(), info.Mode(), entryInfo)
	}
	return entries
}

func (r *MountRouter) hasDescendantMount(path string) bool {
	for _, m := range r.mounts {
		if _, ok := relativeDescendantPath(m.path, path); ok {
			return true
		}
	}
	return false
}

func relativeDescendantPath(path, parent string) (string, bool) {
	path = filepath.Clean(path)
	parent = filepath.Clean(parent)

	if path == parent {
		return "", false
	}

	if parent == "/" {
		rel := strings.TrimPrefix(path, "/")
		if rel == "" {
			return "", false
		}
		return rel, true
	}

	prefix := parent + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rel := strings.TrimPrefix(path, prefix)
	if rel == "" {
		return "", false
	}
	return rel, true
}

func isMissingPathError(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR)
}

func syntheticDirEntry(name string) DirEntry {
	info := syntheticDirInfo(name)
	return NewDirEntry(name, true, info.Mode(), info)
}

func syntheticDirInfo(name string) FileInfo {
	return NewFileInfo(name, 0, os.ModeDir|0755, time.Now(), true)
}

func (r *MountRouter) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	p, rel, err := r.resolve(path)
	if err != nil {
		return nil, err
	}
	return p.Open(rel, flags, mode)
}

func (r *MountRouter) Create(path string, mode os.FileMode) (Handle, error) {
	p, rel, err := r.resolve(path)
	if err != nil {
		return nil, err
	}
	return p.Create(rel, mode)
}

func (r *MountRouter) Mkdir(path string, mode os.FileMode) error {
	p, rel, err := r.resolve(path)
	if err != nil {
		return err
	}
	return p.Mkdir(rel, mode)
}

func (r *MountRouter) Chmod(path string, mode os.FileMode) error {
	p, rel, err := r.resolve(path)
	if err != nil {
		return err
	}
	return p.Chmod(rel, mode)
}

func (r *MountRouter) Remove(path string) error {
	p, rel, err := r.resolve(path)
	if err != nil {
		return err
	}
	return p.Remove(rel)
}

func (r *MountRouter) RemoveAll(path string) error {
	p, rel, err := r.resolve(path)
	if err != nil {
		return err
	}
	return p.RemoveAll(rel)
}

func (r *MountRouter) Rename(oldPath, newPath string) error {
	oldP, oldRel, err := r.resolve(oldPath)
	if err != nil {
		return err
	}
	newP, newRel, err := r.resolve(newPath)
	if err != nil {
		return err
	}
	if oldP != newP {
		return syscall.EXDEV
	}
	return oldP.Rename(oldRel, newRel)
}

func (r *MountRouter) Symlink(target, link string) error {
	p, rel, err := r.resolve(link)
	if err != nil {
		return err
	}
	return p.Symlink(target, rel)
}

func (r *MountRouter) Readlink(path string) (string, error) {
	p, rel, err := r.resolve(path)
	if err != nil {
		return "", err
	}
	return p.Readlink(rel)
}

// Link hard-links oldName to newName within the confined mounts. Both paths are
// resolved via the router (so absolute guest paths map to their provider and a
// path outside the mounted tree fails closed), and the link is rejected when the
// two entries belong to different providers (EXDEV), just like a cross-device
// hard link. Providers that do not implement Link (e.g. memory) report ENOSYS.
func (r *MountRouter) Link(oldName, newName string) error {
	oldP, oldRel, err := r.resolve(oldName)
	if err != nil {
		return err
	}
	newP, newRel, err := r.resolve(newName)
	if err != nil {
		return err
	}
	if oldP != newP {
		return syscall.EXDEV
	}
	if linker, ok := oldP.(interface {
		Link(oldName, newName string) error
	}); ok {
		return linker.Link(oldRel, newRel)
	}
	return syscall.ENOSYS
}

func (r *MountRouter) Fsync(path string) error {
	p, rel, err := r.resolve(path)
	if err != nil {
		return err
	}
	return p.Fsync(rel)
}

func (r *MountRouter) AddMount(path string, provider Provider) {
	path = filepath.Clean(path)
	r.mounts = append(r.mounts, mount{path: path, provider: provider})
	sort.Slice(r.mounts, func(i, j int) bool {
		return len(r.mounts[i].path) > len(r.mounts[j].path)
	})
}

func (r *MountRouter) RemoveMount(path string) {
	path = filepath.Clean(path)
	for i, m := range r.mounts {
		if m.path == path {
			r.mounts = append(r.mounts[:i], r.mounts[i+1:]...)
			return
		}
	}
}
