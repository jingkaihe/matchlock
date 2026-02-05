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
	p, rel, err := r.resolve(path)
	if err != nil {
		return nil, err
	}
	return p.ReadDir(rel)
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
