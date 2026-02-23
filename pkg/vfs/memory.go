package vfs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type MemoryProvider struct {
	mu    sync.RWMutex
	files map[string]*memFile
	dirs  map[string]os.FileMode
}

type memFile struct {
	mu      sync.RWMutex
	data    []byte
	mode    os.FileMode
	modTime time.Time
}

func NewMemoryProvider() *MemoryProvider {
	return &MemoryProvider{
		files: make(map[string]*memFile),
		dirs:  map[string]os.FileMode{"/": 0755},
	}
}

func (p *MemoryProvider) Readonly() bool { return false }

func (p *MemoryProvider) normPath(path string) string {
	path = filepath.Clean(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func (p *MemoryProvider) Stat(path string) (FileInfo, error) {
	path = p.normPath(path)
	p.mu.RLock()
	defer p.mu.RUnlock()

	if mode, ok := p.dirs[path]; ok {
		if mode == 0 {
			mode = 0755
		}
		return NewFileInfo(filepath.Base(path), 0, os.ModeDir|mode, time.Now(), true), nil
	}

	f, ok := p.files[path]
	if !ok {
		return FileInfo{}, syscall.ENOENT
	}

	f.mu.RLock()
	defer f.mu.RUnlock()
	return NewFileInfo(filepath.Base(path), int64(len(f.data)), f.mode, f.modTime, false), nil
}

func (p *MemoryProvider) ReadDir(path string) ([]DirEntry, error) {
	path = p.normPath(path)
	p.mu.RLock()
	defer p.mu.RUnlock()

	if _, ok := p.dirs[path]; !ok {
		return nil, syscall.ENOTDIR
	}

	prefix := path
	if prefix != "/" {
		prefix += "/"
	}

	seen := make(map[string]bool)
	var entries []DirEntry

	for filePath, f := range p.files {
		if !strings.HasPrefix(filePath, prefix) {
			continue
		}
		rel := strings.TrimPrefix(filePath, prefix)
		parts := strings.SplitN(rel, "/", 2)
		name := parts[0]
		if seen[name] {
			continue
		}
		seen[name] = true

		if len(parts) > 1 {
			entries = append(entries, NewDirEntry(name, true, os.ModeDir|0755, NewFileInfo(name, 0, os.ModeDir|0755, time.Now(), true)))
		} else {
			f.mu.RLock()
			info := NewFileInfo(name, int64(len(f.data)), f.mode, f.modTime, false)
			f.mu.RUnlock()
			entries = append(entries, NewDirEntry(name, false, f.mode, info))
		}
	}

	for dirPath := range p.dirs {
		if !strings.HasPrefix(dirPath, prefix) || dirPath == path {
			continue
		}
		rel := strings.TrimPrefix(dirPath, prefix)
		parts := strings.SplitN(rel, "/", 2)
		name := parts[0]
		if seen[name] {
			continue
		}
		seen[name] = true
		entries = append(entries, NewDirEntry(name, true, os.ModeDir|0755, NewFileInfo(name, 0, os.ModeDir|0755, time.Now(), true)))
	}

	return entries, nil
}

func (p *MemoryProvider) Open(path string, flags int, mode os.FileMode) (Handle, error) {
	path = p.normPath(path)

	if flags&os.O_CREATE != 0 {
		p.mu.Lock()
		if _, exists := p.files[path]; !exists {
			dir := filepath.Dir(path)
			if _, ok := p.dirs[dir]; !ok {
				p.mu.Unlock()
				return nil, syscall.ENOENT
			}
			p.files[path] = &memFile{
				data:    []byte{},
				mode:    mode,
				modTime: time.Now(),
			}
		}
		p.mu.Unlock()
	}

	p.mu.RLock()
	f, ok := p.files[path]
	p.mu.RUnlock()
	if !ok {
		return nil, syscall.ENOENT
	}

	return &memHandle{
		file:   f,
		flags:  flags,
		offset: 0,
	}, nil
}

func (p *MemoryProvider) Create(path string, mode os.FileMode) (Handle, error) {
	return p.Open(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
}

func (p *MemoryProvider) Mkdir(path string, mode os.FileMode) error {
	path = p.normPath(path)
	p.mu.Lock()
	defer p.mu.Unlock()

	parent := filepath.Dir(path)
	if _, ok := p.dirs[parent]; !ok {
		return syscall.ENOENT
	}

	if _, ok := p.dirs[path]; ok {
		return syscall.EEXIST
	}

	p.dirs[path] = mode
	return nil
}

func (p *MemoryProvider) Chmod(path string, mode os.FileMode) error {
	path = p.normPath(path)
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.dirs[path]; ok {
		p.dirs[path] = mode
		return nil
	}

	f, ok := p.files[path]
	if !ok {
		return syscall.ENOENT
	}

	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
	return nil
}

func (p *MemoryProvider) Remove(path string) error {
	path = p.normPath(path)
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.dirs[path]; ok {
		for k := range p.files {
			if strings.HasPrefix(k, path+"/") {
				return syscall.ENOTEMPTY
			}
		}
		delete(p.dirs, path)
		return nil
	}

	if _, ok := p.files[path]; !ok {
		return syscall.ENOENT
	}
	delete(p.files, path)
	return nil
}

func (p *MemoryProvider) RemoveAll(path string) error {
	path = p.normPath(path)
	p.mu.Lock()
	defer p.mu.Unlock()

	prefix := path
	if prefix != "/" {
		prefix += "/"
	}

	for k := range p.files {
		if k == path || strings.HasPrefix(k, prefix) {
			delete(p.files, k)
		}
	}

	for k := range p.dirs {
		if k == path || strings.HasPrefix(k, prefix) {
			delete(p.dirs, k)
		}
	}

	return nil
}

func (p *MemoryProvider) Rename(oldPath, newPath string) error {
	oldPath = p.normPath(oldPath)
	newPath = p.normPath(newPath)
	p.mu.Lock()
	defer p.mu.Unlock()

	f, ok := p.files[oldPath]
	if !ok {
		var mode os.FileMode
		if mode, ok = p.dirs[oldPath]; !ok {
			return syscall.ENOENT
		}
		delete(p.dirs, oldPath)
		p.dirs[newPath] = mode
		return nil
	}

	delete(p.files, oldPath)
	p.files[newPath] = f
	return nil
}

func (p *MemoryProvider) Symlink(target, link string) error {
	return syscall.ENOSYS
}

func (p *MemoryProvider) Readlink(path string) (string, error) {
	return "", syscall.ENOSYS
}

type memHandle struct {
	file   *memFile
	flags  int
	offset int64
}

func (h *memHandle) Read(p []byte) (int, error) {
	n, err := h.ReadAt(p, h.offset)
	h.offset += int64(n)
	return n, err
}

func (h *memHandle) ReadAt(p []byte, off int64) (int, error) {
	h.file.mu.RLock()
	defer h.file.mu.RUnlock()

	if off >= int64(len(h.file.data)) {
		return 0, io.EOF
	}

	n := copy(p, h.file.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *memHandle) Write(p []byte) (int, error) {
	n, err := h.WriteAt(p, h.offset)
	h.offset += int64(n)
	return n, err
}

func (h *memHandle) WriteAt(p []byte, off int64) (int, error) {
	h.file.mu.Lock()
	defer h.file.mu.Unlock()

	end := off + int64(len(p))
	if end > int64(len(h.file.data)) {
		newData := make([]byte, end)
		copy(newData, h.file.data)
		h.file.data = newData
	}

	n := copy(h.file.data[off:], p)
	h.file.modTime = time.Now()
	return n, nil
}

func (h *memHandle) Seek(offset int64, whence int) (int64, error) {
	h.file.mu.RLock()
	size := int64(len(h.file.data))
	h.file.mu.RUnlock()

	switch whence {
	case io.SeekStart:
		h.offset = offset
	case io.SeekCurrent:
		h.offset += offset
	case io.SeekEnd:
		h.offset = size + offset
	}

	if h.offset < 0 {
		h.offset = 0
	}
	return h.offset, nil
}

func (h *memHandle) Stat() (FileInfo, error) {
	h.file.mu.RLock()
	defer h.file.mu.RUnlock()
	return NewFileInfo("", int64(len(h.file.data)), h.file.mode, h.file.modTime, false), nil
}

func (h *memHandle) Sync() error {
	return nil
}

func (h *memHandle) Truncate(size int64) error {
	h.file.mu.Lock()
	defer h.file.mu.Unlock()

	if size < int64(len(h.file.data)) {
		h.file.data = h.file.data[:size]
	} else if size > int64(len(h.file.data)) {
		newData := make([]byte, size)
		copy(newData, h.file.data)
		h.file.data = newData
	}
	h.file.modTime = time.Now()
	return nil
}

func (h *memHandle) Close() error {
	return nil
}

func (p *MemoryProvider) WriteFile(path string, data []byte, mode os.FileMode) error {
	path = p.normPath(path)
	p.mu.Lock()
	defer p.mu.Unlock()

	dir := filepath.Dir(path)
	if _, ok := p.dirs[dir]; !ok {
		return syscall.ENOENT
	}

	p.files[path] = &memFile{
		data:    bytes.Clone(data),
		mode:    mode,
		modTime: time.Now(),
	}
	return nil
}

func (p *MemoryProvider) ReadFile(path string) ([]byte, error) {
	path = p.normPath(path)
	p.mu.RLock()
	defer p.mu.RUnlock()

	f, ok := p.files[path]
	if !ok {
		return nil, syscall.ENOENT
	}

	f.mu.RLock()
	defer f.mu.RUnlock()
	return bytes.Clone(f.data), nil
}

func (p *MemoryProvider) MkdirAll(path string, mode os.FileMode) error {
	path = p.normPath(path)
	p.mu.Lock()
	defer p.mu.Unlock()

	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	current := ""
	for _, part := range parts {
		if part == "" {
			continue
		}
		current += "/" + part
		if _, ok := p.dirs[current]; !ok {
			p.dirs[current] = mode
		}
	}
	return nil
}
