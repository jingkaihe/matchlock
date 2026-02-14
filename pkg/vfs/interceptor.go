package vfs

import (
	"os"
	"syscall"
)

type interceptProvider struct {
	inner      Provider
	hooks      *HookEngine
	defaultUID int
	defaultGID int
}

func NewInterceptProvider(inner Provider, hooks *HookEngine) Provider {
	if inner == nil || hooks == nil {
		return inner
	}
	return &interceptProvider{
		inner:      inner,
		hooks:      hooks,
		defaultUID: os.Geteuid(),
		defaultGID: os.Getegid(),
	}
}

func (p *interceptProvider) withCaller(uid, gid int) Provider {
	if p == nil {
		return nil
	}
	clone := *p
	clone.defaultUID = uid
	clone.defaultGID = gid
	return &clone
}

func (p *interceptProvider) baseRequest(op HookOp, path string) HookRequest {
	return HookRequest{
		Op:   op,
		Path: path,
		UID:  p.defaultUID,
		GID:  p.defaultGID,
	}
}

func (p *interceptProvider) Readonly() bool {
	return p.inner.Readonly()
}

func (p *interceptProvider) Stat(path string) (FileInfo, error) {
	req := p.baseRequest(HookOpStat, path)
	if err := p.hooks.Before(&req); err != nil {
		return FileInfo{}, err
	}
	info, err := p.inner.Stat(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return info, err
}

func (p *interceptProvider) ReadDir(path string) ([]DirEntry, error) {
	req := p.baseRequest(HookOpReadDir, path)
	if err := p.hooks.Before(&req); err != nil {
		return nil, err
	}
	entries, err := p.inner.ReadDir(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return entries, err
}

func (p *interceptProvider) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	op := HookOpOpen
	if flags&os.O_CREATE != 0 {
		op = HookOpCreate
	}
	req := p.baseRequest(op, path)
	req.Flags = flags
	req.Mode = mode
	if err := p.hooks.Before(&req); err != nil {
		return nil, err
	}
	h, err := p.inner.Open(req.Path, req.Flags, req.Mode)
	p.hooks.After(req, HookResult{Err: err})
	if err != nil {
		return nil, err
	}
	return &interceptHandle{
		inner: h,
		hooks: p.hooks,
		path:  req.Path,
		uid:   req.UID,
		gid:   req.GID,
	}, nil
}

func (p *interceptProvider) Create(path string, mode os.FileMode) (Handle, error) {
	req := p.baseRequest(HookOpCreate, path)
	req.Mode = mode
	if err := p.hooks.Before(&req); err != nil {
		return nil, err
	}
	h, err := p.inner.Create(req.Path, req.Mode)
	p.hooks.After(req, HookResult{Err: err})
	if err != nil {
		return nil, err
	}
	return &interceptHandle{
		inner: h,
		hooks: p.hooks,
		path:  req.Path,
		uid:   req.UID,
		gid:   req.GID,
	}, nil
}

func (p *interceptProvider) Mkdir(path string, mode os.FileMode) error {
	req := p.baseRequest(HookOpMkdir, path)
	req.Mode = mode
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Mkdir(req.Path, req.Mode)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Chmod(path string, mode os.FileMode) error {
	req := p.baseRequest(HookOpChmod, path)
	req.Mode = mode
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Chmod(req.Path, req.Mode)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Remove(path string) error {
	req := p.baseRequest(HookOpRemove, path)
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Remove(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) RemoveAll(path string) error {
	req := p.baseRequest(HookOpRemoveAll, path)
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.RemoveAll(req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Rename(oldPath, newPath string) error {
	req := p.baseRequest(HookOpRename, oldPath)
	req.NewPath = newPath
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Rename(req.Path, req.NewPath)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Symlink(target, link string) error {
	req := p.baseRequest(HookOpSymlink, link)
	req.NewPath = target
	if err := p.hooks.Before(&req); err != nil {
		return err
	}
	err := p.inner.Symlink(target, req.Path)
	p.hooks.After(req, HookResult{Err: err})
	return err
}

func (p *interceptProvider) Readlink(path string) (string, error) {
	req := p.baseRequest(HookOpReadlink, path)
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
	uid   int
	gid   int
}

func (h *interceptHandle) Read(p []byte) (int, error) {
	req := HookRequest{Op: HookOpRead, Path: h.path, UID: h.uid, GID: h.gid}
	if err := h.hooks.Before(&req); err != nil {
		return 0, err
	}
	n, err := h.inner.Read(p)
	h.hooks.After(req, HookResult{Err: err, Bytes: n})
	return n, err
}

func (h *interceptHandle) ReadAt(p []byte, off int64) (int, error) {
	req := HookRequest{Op: HookOpRead, Path: h.path, Offset: off, UID: h.uid, GID: h.gid}
	if err := h.hooks.Before(&req); err != nil {
		return 0, err
	}
	n, err := h.inner.ReadAt(p, off)
	h.hooks.After(req, HookResult{Err: err, Bytes: n})
	return n, err
}

func (h *interceptHandle) Write(p []byte) (int, error) {
	req := HookRequest{Op: HookOpWrite, Path: h.path, Data: append([]byte(nil), p...), UID: h.uid, GID: h.gid}
	h.populateWriteMetadata(&req)
	if err := h.hooks.Before(&req); err != nil {
		return 0, err
	}
	origLen := len(p)
	n, err := h.inner.Write(req.Data)
	if err == nil && n == len(req.Data) {
		n = origLen
	}
	result := HookResult{Err: err, Bytes: n}
	if info, statErr := h.inner.Stat(); statErr == nil {
		result.Meta = fileMetaFromInfo(info)
	}
	h.hooks.After(req, result)
	return n, err
}

func (h *interceptHandle) WriteAt(p []byte, off int64) (int, error) {
	req := HookRequest{Op: HookOpWrite, Path: h.path, Offset: off, Data: append([]byte(nil), p...), UID: h.uid, GID: h.gid}
	h.populateWriteMetadata(&req)
	if err := h.hooks.Before(&req); err != nil {
		return 0, err
	}
	origLen := len(p)
	n, err := h.inner.WriteAt(req.Data, off)
	if err == nil && n == len(req.Data) {
		n = origLen
	}
	result := HookResult{Err: err, Bytes: n}
	if info, statErr := h.inner.Stat(); statErr == nil {
		result.Meta = fileMetaFromInfo(info)
	}
	h.hooks.After(req, result)
	return n, err
}

func (h *interceptHandle) Seek(off int64, whence int) (int64, error) {
	return h.inner.Seek(off, whence)
}

func (h *interceptHandle) Close() error {
	req := HookRequest{Op: HookOpClose, Path: h.path, UID: h.uid, GID: h.gid}
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
	req := HookRequest{Op: HookOpSync, Path: h.path, UID: h.uid, GID: h.gid}
	if err := h.hooks.Before(&req); err != nil {
		return err
	}
	err := h.inner.Sync()
	h.hooks.After(req, HookResult{Err: err})
	return err
}

func (h *interceptHandle) Truncate(size int64) error {
	req := HookRequest{Op: HookOpTruncate, Path: h.path, Offset: size, UID: h.uid, GID: h.gid}
	if err := h.hooks.Before(&req); err != nil {
		return err
	}
	err := h.inner.Truncate(size)
	h.hooks.After(req, HookResult{Err: err})
	return err
}

func (h *interceptHandle) populateWriteMetadata(req *HookRequest) {
	if req == nil {
		return
	}

	info, err := h.inner.Stat()
	if err != nil {
		return
	}
	req.Mode = info.Mode()
	if uid, gid, ok := ownerFromFileInfo(info); ok {
		req.UID = uid
		req.GID = gid
	}
}

func fileMetaFromInfo(info FileInfo) *HookFileMeta {
	meta := &HookFileMeta{
		Size: info.Size(),
		Mode: info.Mode(),
	}
	if uid, gid, ok := ownerFromFileInfo(info); ok {
		meta.UID = uid
		meta.GID = gid
		meta.HasOwnership = true
	}
	return meta
}

func ownerFromFileInfo(info FileInfo) (int, int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}
