package vfs

import "os"

type interceptProvider struct {
	inner Provider
	hooks *HookEngine
}

func NewInterceptProvider(inner Provider, hooks *HookEngine) Provider {
	if inner == nil || hooks == nil {
		return inner
	}
	return &interceptProvider{inner: inner, hooks: hooks}
}

func (p *interceptProvider) Readonly() bool {
	return p.inner.Readonly()
}

func (p *interceptProvider) Stat(path string) (FileInfo, error) {
	req := HookRequest{Op: HookOpStat, Path: path}
	if err := p.hooks.Before(&req); err != nil {
		return FileInfo{}, err
	}
	info, err := p.inner.Stat(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return info, err
}

func (p *interceptProvider) ReadDir(path string) ([]DirEntry, error) {
	req := HookRequest{Op: HookOpReadDir, Path: path}
	if err := p.hooks.Before(&req); err != nil {
		return nil, err
	}
	entries, err := p.inner.ReadDir(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return entries, err
}

func (p *interceptProvider) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	req := HookRequest{Op: HookOpOpen, Path: path, Flags: flags, Mode: mode}
	if err := p.hooks.Before(&req); err != nil {
		return nil, err
	}
	h, err := p.inner.Open(req.Path, req.Flags, req.Mode)
	p.hooks.After(req, HookResult{Err: err})
	if err != nil {
		return nil, err
	}
	return &interceptHandle{inner: h, hooks: p.hooks, path: req.Path}, nil
}

func (p *interceptProvider) Create(path string, mode os.FileMode) (Handle, error) {
	req := HookRequest{Op: HookOpCreate, Path: path, Mode: mode}
	if err := p.hooks.Before(&req); err != nil {
		return nil, err
	}
	h, err := p.inner.Create(req.Path, req.Mode)
	p.hooks.After(req, HookResult{Err: err})
	if err != nil {
		return nil, err
	}
	return &interceptHandle{inner: h, hooks: p.hooks, path: req.Path}, nil
}

func (p *interceptProvider) Mkdir(path string, mode os.FileMode) error {
	req := HookRequest{Op: HookOpMkdir, Path: path, Mode: mode}
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Mkdir(req.Path, req.Mode)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Chmod(path string, mode os.FileMode) error {
	req := HookRequest{Op: HookOpChmod, Path: path, Mode: mode}
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Chmod(req.Path, req.Mode)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Remove(path string) error {
	req := HookRequest{Op: HookOpRemove, Path: path}
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Remove(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) RemoveAll(path string) error {
	req := HookRequest{Op: HookOpRemoveAll, Path: path}
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.RemoveAll(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Rename(oldPath, newPath string) error {
	req := HookRequest{Op: HookOpRename, Path: oldPath, NewPath: newPath}
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Rename(req.Path, req.NewPath)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Symlink(target, link string) error {
	req := HookRequest{Op: HookOpSymlink, Path: link, NewPath: target}
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Symlink(target, req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Readlink(path string) (string, error) {
	req := HookRequest{Op: HookOpReadlink, Path: path}
	if err := p.hooks.Before(&req); err != nil {
		return "", err
	}
	result, err := p.inner.Readlink(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return result, err
}

type interceptHandle struct {
	inner Handle
	hooks *HookEngine
	path  string
}

func (h *interceptHandle) Read(p []byte) (int, error) {
	req := HookRequest{Op: HookOpRead, Path: h.path}
	if err := h.hooks.Before(&req); err != nil {
		return 0, err
	}
	n, err := h.inner.Read(p)
	h.hooks.After(req, HookResult{Err: err, Bytes: n})
	return n, err
}

func (h *interceptHandle) ReadAt(p []byte, off int64) (int, error) {
	req := HookRequest{Op: HookOpRead, Path: h.path, Offset: off}
	if err := h.hooks.Before(&req); err != nil {
		return 0, err
	}
	n, err := h.inner.ReadAt(p, off)
	h.hooks.After(req, HookResult{Err: err, Bytes: n})
	return n, err
}

func (h *interceptHandle) Write(p []byte) (int, error) {
	req := HookRequest{Op: HookOpWrite, Path: h.path, Data: append([]byte(nil), p...)}
	if err := h.hooks.Before(&req); err != nil {
		return 0, err
	}
	origLen := len(p)
	n, err := h.inner.Write(req.Data)
	if err == nil && n == len(req.Data) {
		n = origLen
	}
	h.hooks.After(req, HookResult{Err: err, Bytes: n})
	return n, err
}

func (h *interceptHandle) WriteAt(p []byte, off int64) (int, error) {
	req := HookRequest{Op: HookOpWrite, Path: h.path, Offset: off, Data: append([]byte(nil), p...)}
	if err := h.hooks.Before(&req); err != nil {
		return 0, err
	}
	origLen := len(p)
	n, err := h.inner.WriteAt(req.Data, off)
	if err == nil && n == len(req.Data) {
		n = origLen
	}
	h.hooks.After(req, HookResult{Err: err, Bytes: n})
	return n, err
}

func (h *interceptHandle) Seek(off int64, whence int) (int64, error) {
	return h.inner.Seek(off, whence)
}

func (h *interceptHandle) Close() error {
	req := HookRequest{Op: HookOpClose, Path: h.path}
	if err := h.hooks.Before(&req); err != nil {
		return err
	}
	err := h.inner.Close()
	h.hooks.After(req, HookResult{Err: err})
	return err
}

func (h *interceptHandle) Stat() (FileInfo, error) {
	return h.inner.Stat()
}

func (h *interceptHandle) Sync() error {
	req := HookRequest{Op: HookOpSync, Path: h.path}
	if err := h.hooks.Before(&req); err != nil {
		return err
	}
	err := h.inner.Sync()
	h.hooks.After(req, HookResult{Err: err})
	return err
}

func (h *interceptHandle) Truncate(size int64) error {
	req := HookRequest{Op: HookOpTruncate, Path: h.path, Offset: size}
	if err := h.hooks.Before(&req); err != nil {
		return err
	}
	err := h.inner.Truncate(size)
	h.hooks.After(req, HookResult{Err: err})
	return err
}
