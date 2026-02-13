package vfs

import (
	"os"
	"path/filepath"
)

type RealFSProvider struct {
	root string
}

func NewRealFSProvider(root string) *RealFSProvider {
	return &RealFSProvider{root: root}
}

func (p *RealFSProvider) Readonly() bool { return false }

func (p *RealFSProvider) realPath(path string) string {
	return filepath.Join(p.root, filepath.Clean(path))
}

func (p *RealFSProvider) Stat(path string) (FileInfo, error) {
	info, err := os.Stat(p.realPath(path))
	if err != nil {
		return FileInfo{}, err
	}
	return NewFileInfoWithSys(info.Name(), info.Size(), info.Mode(), info.ModTime(), info.IsDir(), info.Sys()), nil
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
			NewFileInfoWithSys(e.Name(), info.Size(), info.Mode(), info.ModTime(), e.IsDir(), info.Sys()),
		))
	}
	return result, nil
}

func (p *RealFSProvider) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	f, err := os.OpenFile(p.realPath(path), flags, mode)
	if err != nil {
		return nil, err
	}
	return &realHandle{file: f}, nil
}

func (p *RealFSProvider) Create(path string, mode os.FileMode) (Handle, error) {
	f, err := os.OpenFile(p.realPath(path), os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
	if err != nil {
		return nil, err
	}
	return &realHandle{file: f}, nil
}

func (p *RealFSProvider) Mkdir(path string, mode os.FileMode) error {
	return os.Mkdir(p.realPath(path), mode)
}

func (p *RealFSProvider) Chmod(path string, mode os.FileMode) error {
	return os.Chmod(p.realPath(path), mode)
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

func (p *RealFSProvider) Symlink(target, link string) error {
	return os.Symlink(target, p.realPath(link))
}

func (p *RealFSProvider) Readlink(path string) (string, error) {
	return os.Readlink(p.realPath(path))
}

type realHandle struct {
	file *os.File
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
	return NewFileInfoWithSys(info.Name(), info.Size(), info.Mode(), info.ModTime(), info.IsDir(), info.Sys()), nil
}
