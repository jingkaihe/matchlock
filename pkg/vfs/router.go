package vfs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
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
	p, rel, err := r.resolve(path)
	if err != nil {
		return FileInfo{}, err
	}
	return p.Stat(rel)
}

func (r *MountRouter) ReadDir(path string) ([]DirEntry, error) {
	path = filepath.Clean(path)
	p, rel, err := r.resolve(path)
	if err != nil {
		return nil, err
	}
	entries, err := p.ReadDir(rel)
	if err != nil {
		return nil, err
	}

	// Merge direct child mountpoints so nested mounts are visible in parent
	// directory listings (for example a file mount at /workspace/file.txt).
	byName := make(map[string]DirEntry, len(entries))
	baseNames := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		byName[name] = e
		if !seen[name] {
			baseNames = append(baseNames, name)
			seen[name] = true
		}
	}

	var mountOnlyNames []string
	for _, m := range r.mounts {
		if m.path == path || filepath.Dir(m.path) != path {
			continue
		}

		name := filepath.Base(m.path)
		info, err := m.provider.Stat("/")
		if err != nil {
			continue
		}

		entryInfo := NewFileInfo(name, info.Size(), info.Mode(), info.ModTime(), info.IsDir())
		byName[name] = NewDirEntry(name, info.IsDir(), info.Mode(), entryInfo)
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
