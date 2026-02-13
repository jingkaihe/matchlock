package vfs

import (
	"io"
	"io/fs"
	"os"
	"time"
)

type Provider interface {
	Readonly() bool
	Stat(path string) (FileInfo, error)
	ReadDir(path string) ([]DirEntry, error)
	Open(path string, flags int, mode os.FileMode) (Handle, error)
	Create(path string, mode os.FileMode) (Handle, error)
	Mkdir(path string, mode os.FileMode) error
	Chmod(path string, mode os.FileMode) error
	Remove(path string) error
	RemoveAll(path string) error
	Rename(oldPath, newPath string) error
	Symlink(target, link string) error
	Readlink(path string) (string, error)
}

type Handle interface {
	io.Reader
	io.Writer
	io.Seeker
	io.Closer
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Stat() (FileInfo, error)
	Sync() error
	Truncate(size int64) error
}

type FileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
	sys     any
}

func (fi FileInfo) Name() string       { return fi.name }
func (fi FileInfo) Size() int64        { return fi.size }
func (fi FileInfo) Mode() os.FileMode  { return fi.mode }
func (fi FileInfo) ModTime() time.Time { return fi.modTime }
func (fi FileInfo) IsDir() bool        { return fi.isDir }
func (fi FileInfo) Sys() any           { return fi.sys }

func NewFileInfo(name string, size int64, mode os.FileMode, modTime time.Time, isDir bool) FileInfo {
	return NewFileInfoWithSys(name, size, mode, modTime, isDir, nil)
}

func NewFileInfoWithSys(name string, size int64, mode os.FileMode, modTime time.Time, isDir bool, sys any) FileInfo {
	return FileInfo{
		name:    name,
		size:    size,
		mode:    mode,
		modTime: modTime,
		isDir:   isDir,
		sys:     sys,
	}
}

type DirEntry struct {
	name  string
	isDir bool
	mode  os.FileMode
	info  FileInfo
}

func (de DirEntry) Name() string               { return de.name }
func (de DirEntry) IsDir() bool                { return de.isDir }
func (de DirEntry) Type() fs.FileMode          { return de.mode.Type() }
func (de DirEntry) Info() (fs.FileInfo, error) { return de.info, nil }

func NewDirEntry(name string, isDir bool, mode os.FileMode, info FileInfo) DirEntry {
	return DirEntry{
		name:  name,
		isDir: isDir,
		mode:  mode,
		info:  info,
	}
}
