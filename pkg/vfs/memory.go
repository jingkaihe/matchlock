package vfs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type MemoryProvider struct {
	mu        sync.RWMutex
	totalSize atomic.Int64
	sizeLimit int64
	files     map[string]*memFile
	dirs      map[string]bool
	dirModes  map[string]os.FileMode
}

type memFile struct {
	mu      sync.RWMutex
	p       *MemoryProvider
	data    []byte
	mode    os.FileMode
	modTime time.Time
}

const DefaultMemoryProviderSizeLimit = 32_000_000

type MemoryProviderOption func(*MemoryProvider)

// WithSizeLimit sets a memory size limit in byte for this memory provider.
// Operations exceeding the size limit will abort and return ENOSPC.
func WithSizeLimit(sizeLimit int64) MemoryProviderOption {
	return func(p *MemoryProvider) {
		p.sizeLimit = sizeLimit
	}
}

func NewMemoryProvider(opts ...MemoryProviderOption) *MemoryProvider {
	provider := &MemoryProvider{
		sizeLimit: DefaultMemoryProviderSizeLimit,
		files:     make(map[string]*memFile),
		dirs:      map[string]bool{"/": true},
		dirModes:  map[string]os.FileMode{"/": 0o755},
	}
	for _, opt := range opts {
		opt(provider)
	}
	return provider
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

	if p.dirs[path] {
		mode := p.dirModes[path]
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

	if !p.dirs[path] {
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
			if !p.dirs[dir] {
				p.mu.Unlock()
				return nil, syscall.ENOENT
			}

			bytesGrowth := p.fileEntryMemorySize(path)
			err := p.ensureGrowthBelowSizeLimit(bytesGrowth)
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			p.files[path] = &memFile{
				p:       p,
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
	if !p.dirs[parent] {
		return syscall.ENOENT
	}

	if p.dirs[path] {
		return syscall.EEXIST
	}

	err := p.ensureGrowthBelowSizeLimit(p.dirEntryMemorySize(path))
	if err != nil {
		return err
	}

	p.dirs[path] = true
	p.dirModes[path] = mode
	return nil
}

func (p *MemoryProvider) Chmod(path string, mode os.FileMode) error {
	path = p.normPath(path)
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.dirs[path] {
		p.dirModes[path] = mode
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

	if p.dirs[path] {
		for k := range p.files {
			if strings.HasPrefix(k, path+"/") {
				return syscall.ENOTEMPTY
			}
		}
		p.totalSize.Add(-p.dirEntryMemorySize(path))
		delete(p.dirs, path)
		delete(p.dirModes, path)
		return nil
	}

	if _, ok := p.files[path]; !ok {
		return syscall.ENOENT
	}
	p.totalSize.Add(-p.fileTotalMemorySize(path))
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
			p.totalSize.Add(-p.fileTotalMemorySize(path))
			delete(p.files, k)
		}
	}

	for k := range p.dirs {
		if k == path || strings.HasPrefix(k, prefix) {
			p.totalSize.Add(-p.dirEntryMemorySize(path))
			delete(p.dirs, k)
			delete(p.dirModes, k)
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
		if !p.dirs[oldPath] {
			return syscall.ENOENT
		}
		mode := p.dirModes[oldPath]
		delete(p.dirs, oldPath)
		delete(p.dirModes, oldPath)
		p.dirs[newPath] = true
		if mode != 0 {
			p.dirModes[newPath] = mode
		}
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
		bytesGrowth := end - int64(len(h.file.data))
		err := h.file.p.ensureGrowthBelowSizeLimit(bytesGrowth)
		if err != nil {
			return 0, err
		}

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

	bytesGrowth := size - int64(len(h.file.data))
	if bytesGrowth < 0 {
		h.file.p.totalSize.Add(bytesGrowth)

		h.file.data = h.file.data[:size]
	} else if bytesGrowth > 0 {
		err := h.file.p.ensureGrowthBelowSizeLimit(bytesGrowth)
		if err != nil {
			return err
		}

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
	if !p.dirs[dir] {
		return syscall.ENOENT
	}

	bytesGrowth := p.fileEntryMemorySize(path) + int64(len(data))
	err := p.ensureGrowthBelowSizeLimit(bytesGrowth)
	if err != nil {
		return err
	}
	p.files[path] = &memFile{
		p:       p,
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
		if !p.dirs[current] {
			err := p.ensureGrowthBelowSizeLimit(p.dirEntryMemorySize(current))
			if err != nil {
				return err
			}

			p.dirs[current] = true
		}
	}
	return nil
}

func (p *MemoryProvider) fileEntryMemorySize(path string) int64 {
	// add overhead to account for the memFile struct (sizeof) and map overhead
	return int64(len(path)) + 24
}

func (p *MemoryProvider) dirEntryMemorySize(path string) int64 {
	// len(path)*2 as we currently use two maps (dirs and dirModes)
	return int64(len(path))*2 + 2
}

func (p *MemoryProvider) fileTotalMemorySize(path string) int64 {
	file, ok := p.files[path]
	if !ok {
		return p.fileEntryMemorySize(path)
	}
	return p.fileEntryMemorySize(path) + int64(len(file.data))
}

func (p *MemoryProvider) ensureGrowthBelowSizeLimit(bytesGrowth int64) error {
	currentSize := p.totalSize.Load()
	if currentSize+bytesGrowth > p.sizeLimit {
		return syscall.ENOSPC
	}

	p.totalSize.Add(bytesGrowth)
	return nil
}
