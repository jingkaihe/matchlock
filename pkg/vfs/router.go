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
	if err != nil {
		return nil, err
	}
	entries, readErr := p.ReadDir(rel)
	if readErr != nil && !isMissingPathError(readErr) {
		return nil, readErr
	}
	if readErr != nil {
		entries = nil
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
