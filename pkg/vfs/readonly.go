package vfs

import (
	"os"
	"syscall"
)

type ReadonlyProvider struct {
	inner Provider
}

func NewReadonlyProvider(inner Provider) *ReadonlyProvider {
	return &ReadonlyProvider{inner: inner}
}

func (p *ReadonlyProvider) Readonly() bool                          { return true }
func (p *ReadonlyProvider) Stat(path string) (FileInfo, error)      { return p.inner.Stat(path) }
func (p *ReadonlyProvider) ReadDir(path string) ([]DirEntry, error) { return p.inner.ReadDir(path) }

func (p *ReadonlyProvider) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	if flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, syscall.EROFS
	}
	h, err := p.inner.Open(path, flags, mode)
	if err != nil {
		return nil, err
	}
	return &readonlyHandle{inner: h}, nil
}

func (p *ReadonlyProvider) Create(path string, mode os.FileMode) (Handle, error) {
	return nil, syscall.EROFS
}

func (p *ReadonlyProvider) Mkdir(path string, mode os.FileMode) error { return syscall.EROFS }
func (p *ReadonlyProvider) Remove(path string) error                  { return syscall.EROFS }
func (p *ReadonlyProvider) RemoveAll(path string) error               { return syscall.EROFS }
func (p *ReadonlyProvider) Rename(oldPath, newPath string) error      { return syscall.EROFS }
func (p *ReadonlyProvider) Symlink(target, link string) error         { return syscall.EROFS }
func (p *ReadonlyProvider) Readlink(path string) (string, error)      { return p.inner.Readlink(path) }

type readonlyHandle struct {
	inner Handle
}

func (h *readonlyHandle) Read(p []byte) (int, error)                { return h.inner.Read(p) }
func (h *readonlyHandle) ReadAt(p []byte, off int64) (int, error)   { return h.inner.ReadAt(p, off) }
func (h *readonlyHandle) Seek(off int64, whence int) (int64, error) { return h.inner.Seek(off, whence) }
func (h *readonlyHandle) Stat() (FileInfo, error)                   { return h.inner.Stat() }
func (h *readonlyHandle) Close() error                              { return h.inner.Close() }
func (h *readonlyHandle) Sync() error                               { return nil }
func (h *readonlyHandle) Write(p []byte) (int, error)               { return 0, syscall.EROFS }
func (h *readonlyHandle) WriteAt(p []byte, off int64) (int, error)  { return 0, syscall.EROFS }
func (h *readonlyHandle) Truncate(size int64) error                 { return syscall.EROFS }
