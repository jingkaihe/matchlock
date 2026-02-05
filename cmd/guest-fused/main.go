// Guest FUSE daemon connects to the host VFS server over vsock
// and mounts a FUSE filesystem at /workspace
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/fxamacker/cbor/v2"
)

const (
	AF_VSOCK        = 40
	VMADDR_CID_HOST = 2
	VsockPortVFS    = 5001
)

type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Zero      [4]byte
}

// VFS protocol operations (must match pkg/vfs/server.go)
type OpCode uint8

const (
	OpLookup OpCode = iota
	OpGetattr
	OpSetattr
	OpRead
	OpWrite
	OpCreate
	OpMkdir
	OpUnlink
	OpRmdir
	OpRename
	OpOpen
	OpRelease
	OpReaddir
	OpFsync
	OpMkdirAll
)

type VFSRequest struct {
	Op      OpCode `cbor:"op"`
	Path    string `cbor:"path,omitempty"`
	NewPath string `cbor:"new_path,omitempty"`
	Handle  uint64 `cbor:"fh,omitempty"`
	Offset  int64  `cbor:"off,omitempty"`
	Size    uint32 `cbor:"sz,omitempty"`
	Data    []byte `cbor:"data,omitempty"`
	Flags   uint32 `cbor:"flags,omitempty"`
	Mode    uint32 `cbor:"mode,omitempty"`
}

type VFSResponse struct {
	Err     int32         `cbor:"err"`
	Stat    *VFSStat      `cbor:"stat,omitempty"`
	Data    []byte        `cbor:"data,omitempty"`
	Written uint32        `cbor:"written,omitempty"`
	Handle  uint64        `cbor:"fh,omitempty"`
	Entries []VFSDirEntry `cbor:"entries,omitempty"`
}

type VFSStat struct {
	Size    int64  `cbor:"size"`
	Mode    uint32 `cbor:"mode"`
	ModTime int64  `cbor:"mtime"`
	IsDir   bool   `cbor:"is_dir"`
}

type VFSDirEntry struct {
	Name  string `cbor:"name"`
	IsDir bool   `cbor:"is_dir"`
	Mode  uint32 `cbor:"mode"`
	Size  int64  `cbor:"size"`
}

// FUSE constants
const (
	FUSE_ROOT_ID = 1

	FUSE_LOOKUP     = 1
	FUSE_GETATTR    = 3
	FUSE_SETATTR    = 4
	FUSE_READLINK   = 5
	FUSE_OPEN       = 14
	FUSE_READ       = 15
	FUSE_WRITE      = 16
	FUSE_RELEASE    = 18
	FUSE_FSYNC      = 20
	FUSE_OPENDIR    = 27
	FUSE_READDIR    = 28
	FUSE_RELEASEDIR = 29
	FUSE_INIT       = 26
	FUSE_MKDIR      = 9
	FUSE_UNLINK     = 10
	FUSE_RMDIR      = 11
	FUSE_RENAME     = 12
	FUSE_CREATE     = 35
	FUSE_DESTROY    = 38

	FATTR_MODE = 1 << 0
	FATTR_SIZE = 1 << 3

	S_IFDIR = 0o40000
	S_IFREG = 0o100000

	FUSE_KERNEL_VERSION       = 7
	FUSE_KERNEL_MINOR_VERSION = 38
)

type fuseInHeader struct {
	Len     uint32
	Opcode  uint32
	Unique  uint64
	Nodeid  uint64
	Uid     uint32
	Gid     uint32
	Pid     uint32
	Padding uint32
}

type fuseOutHeader struct {
	Len    uint32
	Error  int32
	Unique uint64
}

type fuseInitIn struct {
	Major        uint32
	Minor        uint32
	MaxReadahead uint32
	Flags        uint32
}

type fuseInitOut struct {
	Major               uint32
	Minor               uint32
	MaxReadahead        uint32
	Flags               uint32
	MaxBackground       uint16
	CongestionThreshold uint16
	MaxWrite            uint32
	TimeGran            uint32
	MaxPages            uint16
	MapAlignment        uint16
	Flags2              uint32
	Unused              [7]uint32
}

type fuseAttrOut struct {
	AttrValid     uint64
	AttrValidNsec uint32
	Dummy         uint32
	Attr          fuseAttr
}

type fuseAttr struct {
	Ino       uint64
	Size      uint64
	Blocks    uint64
	Atime     uint64
	Mtime     uint64
	Ctime     uint64
	AtimeNsec uint32
	MtimeNsec uint32
	CtimeNsec uint32
	Mode      uint32
	Nlink     uint32
	Uid       uint32
	Gid       uint32
	Rdev      uint32
	Blksize   uint32
	Flags     uint32
}

type fuseEntryOut struct {
	Nodeid         uint64
	Generation     uint64
	EntryValid     uint64
	AttrValid      uint64
	EntryValidNsec uint32
	AttrValidNsec  uint32
	Attr           fuseAttr
}

type fuseOpenIn struct {
	Flags  uint32
	Unused uint32
}

type fuseOpenOut struct {
	Fh        uint64
	OpenFlags uint32
	Padding   uint32
}

type fuseReadIn struct {
	Fh        uint64
	Offset    uint64
	Size      uint32
	ReadFlags uint32
	LockOwner uint64
	Flags     uint32
	Padding   uint32
}

type fuseWriteIn struct {
	Fh         uint64
	Offset     uint64
	Size       uint32
	WriteFlags uint32
	LockOwner  uint64
	Flags      uint32
	Padding    uint32
}

type fuseWriteOut struct {
	Size    uint32
	Padding uint32
}

type fuseCreateIn struct {
	Flags   uint32
	Mode    uint32
	Umask   uint32
	Padding uint32
}

type fuseMkdirIn struct {
	Mode  uint32
	Umask uint32
}

type fuseRenameIn struct {
	Newdir uint64
}

type fuseSetAttrIn struct {
	Valid     uint32
	Padding   uint32
	Fh        uint64
	Size      uint64
	LockOwner uint64
	Atime     uint64
	Mtime     uint64
	Ctime     uint64
	AtimeNsec uint32
	MtimeNsec uint32
	CtimeNsec uint32
	Mode      uint32
	Unused4   uint32
	Uid       uint32
	Gid       uint32
	Unused5   uint32
}

type fuseDirent struct {
	Ino     uint64
	Off     uint64
	Namelen uint32
	Type    uint32
}

type VFSClient struct {
	fd int
	mu sync.Mutex
}

func NewVFSClient() (*VFSClient, error) {
	fd, err := dialVsock(VMADDR_CID_HOST, VsockPortVFS)
	if err != nil {
		return nil, err
	}
	return &VFSClient{fd: fd}, nil
}

func (c *VFSClient) Close() error {
	return syscall.Close(c.fd)
}

func (c *VFSClient) Request(req *VFSRequest) (*VFSResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := cbor.Marshal(req)
	if err != nil {
		return nil, err
	}

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	if _, err := writeFull(c.fd, lenBuf[:]); err != nil {
		return nil, err
	}
	if _, err := writeFull(c.fd, data); err != nil {
		return nil, err
	}

	if _, err := readFull(c.fd, lenBuf[:]); err != nil {
		return nil, err
	}
	respLen := binary.BigEndian.Uint32(lenBuf[:])

	respData := make([]byte, respLen)
	if _, err := readFull(c.fd, respData); err != nil {
		return nil, err
	}

	var resp VFSResponse
	if err := cbor.Unmarshal(respData, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type FUSEServer struct {
	fuseFD  int
	client  *VFSClient
	inodes  sync.Map // inode -> path
	nextIno uint64
	inoMu   sync.Mutex
	pathIno sync.Map // path -> inode
}

func NewFUSEServer(mountpoint string, client *VFSClient) (*FUSEServer, error) {
	fuseFD, err := syscall.Open("/dev/fuse", syscall.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open /dev/fuse: %w", err)
	}

	mountOpts := fmt.Sprintf("fd=%d,rootmode=40000,user_id=%d,group_id=%d",
		fuseFD, os.Getuid(), os.Getgid())

	if err := syscall.Mount("fuse", mountpoint, "fuse", 0, mountOpts); err != nil {
		syscall.Close(fuseFD)
		return nil, fmt.Errorf("failed to mount FUSE: %w", err)
	}

	fs := &FUSEServer{
		fuseFD:  fuseFD,
		client:  client,
		nextIno: 2,
	}

	// Root inode maps to /workspace to match the host VFS mount point
	fs.inodes.Store(uint64(FUSE_ROOT_ID), "/workspace")
	fs.pathIno.Store("/workspace", uint64(FUSE_ROOT_ID))

	return fs, nil
}

func (fs *FUSEServer) getPath(ino uint64) string {
	if p, ok := fs.inodes.Load(ino); ok {
		return p.(string)
	}
	return "/workspace"
}

func (fs *FUSEServer) getOrCreateInode(path string) uint64 {
	if ino, ok := fs.pathIno.Load(path); ok {
		return ino.(uint64)
	}

	fs.inoMu.Lock()
	ino := fs.nextIno
	fs.nextIno++
	fs.inoMu.Unlock()

	fs.inodes.Store(ino, path)
	fs.pathIno.Store(path, ino)
	return ino
}

func (fs *FUSEServer) Serve() error {
	buf := make([]byte, 65536+128)

	for {
		n, err := syscall.Read(fs.fuseFD, buf)
		if err != nil {
			if err == syscall.ENOENT {
				continue
			}
			if err == syscall.EINTR {
				continue
			}
			return err
		}

		if n < int(unsafe.Sizeof(fuseInHeader{})) {
			continue
		}

		hdr := (*fuseInHeader)(unsafe.Pointer(&buf[0]))
		data := buf[unsafe.Sizeof(fuseInHeader{}):n]

		fs.handleRequest(hdr, data)
	}
}

func (fs *FUSEServer) handleRequest(hdr *fuseInHeader, data []byte) {
	switch hdr.Opcode {
	case FUSE_INIT:
		fs.handleInit(hdr, data)
	case FUSE_GETATTR:
		fs.handleGetattr(hdr)
	case FUSE_LOOKUP:
		fs.handleLookup(hdr, data)
	case FUSE_OPEN:
		fs.handleOpen(hdr, data)
	case FUSE_OPENDIR:
		fs.handleOpendir(hdr)
	case FUSE_READ:
		fs.handleRead(hdr, data)
	case FUSE_READDIR:
		fs.handleReaddir(hdr, data)
	case FUSE_RELEASE, FUSE_RELEASEDIR:
		fs.handleRelease(hdr, data)
	case FUSE_WRITE:
		fs.handleWrite(hdr, data)
	case FUSE_CREATE:
		fs.handleCreate(hdr, data)
	case FUSE_MKDIR:
		fs.handleMkdir(hdr, data)
	case FUSE_UNLINK:
		fs.handleUnlink(hdr, data)
	case FUSE_RMDIR:
		fs.handleRmdir(hdr, data)
	case FUSE_RENAME:
		fs.handleRename(hdr, data)
	case FUSE_SETATTR:
		fs.handleSetattr(hdr, data)
	case FUSE_FSYNC:
		fs.handleFsync(hdr, data)
	case FUSE_DESTROY:
		fs.sendError(hdr.Unique, 0)
	default:
		fs.sendError(hdr.Unique, -int32(syscall.ENOSYS))
	}
}

func (fs *FUSEServer) handleInit(hdr *fuseInHeader, data []byte) {
	out := fuseInitOut{
		Major:               FUSE_KERNEL_VERSION,
		Minor:               FUSE_KERNEL_MINOR_VERSION,
		MaxReadahead:        65536,
		MaxWrite:            65536,
		MaxBackground:       16,
		CongestionThreshold: 12,
		TimeGran:            1,
	}

	fs.sendReply(hdr.Unique, unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)))
}

func (fs *FUSEServer) handleGetattr(hdr *fuseInHeader) {
	path := fs.getPath(hdr.Nodeid)

	resp, err := fs.client.Request(&VFSRequest{Op: OpGetattr, Path: path})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	out := fuseAttrOut{
		AttrValid: 1,
		Attr:      fs.statToAttr(hdr.Nodeid, resp.Stat),
	}

	fs.sendReply(hdr.Unique, unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)))
}

func (fs *FUSEServer) handleLookup(hdr *fuseInHeader, data []byte) {
	name := string(data[:len(data)-1])
	parentPath := fs.getPath(hdr.Nodeid)
	childPath := filepath.Join(parentPath, name)

	resp, err := fs.client.Request(&VFSRequest{Op: OpLookup, Path: childPath})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.ENOENT)
		if resp != nil && resp.Err != 0 {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	ino := fs.getOrCreateInode(childPath)

	out := fuseEntryOut{
		Nodeid:     ino,
		Generation: 1,
		EntryValid: 1,
		AttrValid:  1,
		Attr:       fs.statToAttr(ino, resp.Stat),
	}

	fs.sendReply(hdr.Unique, unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)))
}

func (fs *FUSEServer) handleOpen(hdr *fuseInHeader, data []byte) {
	in := (*fuseOpenIn)(unsafe.Pointer(&data[0]))
	path := fs.getPath(hdr.Nodeid)

	resp, err := fs.client.Request(&VFSRequest{Op: OpOpen, Path: path, Flags: in.Flags})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	out := fuseOpenOut{Fh: resp.Handle}
	fs.sendReply(hdr.Unique, unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)))
}

func (fs *FUSEServer) handleOpendir(hdr *fuseInHeader) {
	out := fuseOpenOut{Fh: hdr.Nodeid}
	fs.sendReply(hdr.Unique, unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)))
}

func (fs *FUSEServer) handleRead(hdr *fuseInHeader, data []byte) {
	in := (*fuseReadIn)(unsafe.Pointer(&data[0]))

	resp, err := fs.client.Request(&VFSRequest{
		Op:     OpRead,
		Handle: in.Fh,
		Offset: int64(in.Offset),
		Size:   in.Size,
	})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	fs.sendReply(hdr.Unique, resp.Data)
}

func (fs *FUSEServer) handleReaddir(hdr *fuseInHeader, data []byte) {
	in := (*fuseReadIn)(unsafe.Pointer(&data[0]))
	path := fs.getPath(hdr.Nodeid)

	resp, err := fs.client.Request(&VFSRequest{Op: OpReaddir, Path: path})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	buf := make([]byte, 0, in.Size)
	offset := uint64(0)

	for i, entry := range resp.Entries {
		entryType := uint32(8) // DT_REG
		if entry.IsDir {
			entryType = 4 // DT_DIR
		}

		childPath := filepath.Join(path, entry.Name)
		ino := fs.getOrCreateInode(childPath)

		namebytes := []byte(entry.Name)
		reclen := (24 + len(namebytes) + 7) &^ 7

		if uint64(i) < in.Offset {
			offset++
			continue
		}

		if len(buf)+reclen > int(in.Size) {
			break
		}

		dirent := fuseDirent{
			Ino:     ino,
			Off:     offset + 1,
			Namelen: uint32(len(namebytes)),
			Type:    entryType,
		}

		dirBytes := unsafe.Slice((*byte)(unsafe.Pointer(&dirent)), 24)
		buf = append(buf, dirBytes...)
		buf = append(buf, namebytes...)

		padding := reclen - 24 - len(namebytes)
		for j := 0; j < padding; j++ {
			buf = append(buf, 0)
		}

		offset++
	}

	fs.sendReply(hdr.Unique, buf)
}

func (fs *FUSEServer) handleRelease(hdr *fuseInHeader, data []byte) {
	in := (*fuseOpenIn)(unsafe.Pointer(&data[0]))

	fs.client.Request(&VFSRequest{Op: OpRelease, Handle: uint64(in.Flags)})
	fs.sendError(hdr.Unique, 0)
}

func (fs *FUSEServer) handleWrite(hdr *fuseInHeader, data []byte) {
	in := (*fuseWriteIn)(unsafe.Pointer(&data[0]))
	writeData := data[unsafe.Sizeof(fuseWriteIn{}):]

	resp, err := fs.client.Request(&VFSRequest{
		Op:     OpWrite,
		Handle: in.Fh,
		Offset: int64(in.Offset),
		Data:   writeData,
	})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	out := fuseWriteOut{Size: resp.Written}
	fs.sendReply(hdr.Unique, unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)))
}

func (fs *FUSEServer) handleCreate(hdr *fuseInHeader, data []byte) {
	in := (*fuseCreateIn)(unsafe.Pointer(&data[0]))
	name := string(data[unsafe.Sizeof(fuseCreateIn{}):])
	name = name[:len(name)-1]

	parentPath := fs.getPath(hdr.Nodeid)
	childPath := filepath.Join(parentPath, name)

	resp, err := fs.client.Request(&VFSRequest{
		Op:   OpCreate,
		Path: childPath,
		Mode: in.Mode,
	})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	ino := fs.getOrCreateInode(childPath)

	out := make([]byte, unsafe.Sizeof(fuseEntryOut{})+unsafe.Sizeof(fuseOpenOut{}))
	entry := (*fuseEntryOut)(unsafe.Pointer(&out[0]))
	open := (*fuseOpenOut)(unsafe.Pointer(&out[unsafe.Sizeof(fuseEntryOut{})]))

	entry.Nodeid = ino
	entry.Generation = 1
	entry.EntryValid = 1
	entry.AttrValid = 1
	entry.Attr = fuseAttr{
		Ino:   ino,
		Mode:  S_IFREG | (in.Mode & 0o777),
		Nlink: 1,
		Uid:   uint32(os.Getuid()),
		Gid:   uint32(os.Getgid()),
	}

	open.Fh = resp.Handle

	fs.sendReply(hdr.Unique, out)
}

func (fs *FUSEServer) handleMkdir(hdr *fuseInHeader, data []byte) {
	in := (*fuseMkdirIn)(unsafe.Pointer(&data[0]))
	name := string(data[unsafe.Sizeof(fuseMkdirIn{}):])
	name = name[:len(name)-1]

	parentPath := fs.getPath(hdr.Nodeid)
	childPath := filepath.Join(parentPath, name)

	resp, err := fs.client.Request(&VFSRequest{
		Op:   OpMkdir,
		Path: childPath,
		Mode: in.Mode,
	})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	ino := fs.getOrCreateInode(childPath)

	out := fuseEntryOut{
		Nodeid:     ino,
		Generation: 1,
		EntryValid: 1,
		AttrValid:  1,
		Attr: fuseAttr{
			Ino:   ino,
			Mode:  S_IFDIR | (in.Mode & 0o777),
			Nlink: 2,
			Uid:   uint32(os.Getuid()),
			Gid:   uint32(os.Getgid()),
		},
	}

	fs.sendReply(hdr.Unique, unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)))
}

func (fs *FUSEServer) handleUnlink(hdr *fuseInHeader, data []byte) {
	name := string(data[:len(data)-1])
	parentPath := fs.getPath(hdr.Nodeid)
	childPath := filepath.Join(parentPath, name)

	resp, err := fs.client.Request(&VFSRequest{Op: OpUnlink, Path: childPath})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	fs.sendError(hdr.Unique, 0)
}

func (fs *FUSEServer) handleRmdir(hdr *fuseInHeader, data []byte) {
	name := string(data[:len(data)-1])
	parentPath := fs.getPath(hdr.Nodeid)
	childPath := filepath.Join(parentPath, name)

	resp, err := fs.client.Request(&VFSRequest{Op: OpRmdir, Path: childPath})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	fs.sendError(hdr.Unique, 0)
}

func (fs *FUSEServer) handleRename(hdr *fuseInHeader, data []byte) {
	in := (*fuseRenameIn)(unsafe.Pointer(&data[0]))
	names := data[unsafe.Sizeof(fuseRenameIn{}):]

	oldName := ""
	newName := ""
	for i, b := range names {
		if b == 0 {
			oldName = string(names[:i])
			newName = string(names[i+1 : len(names)-1])
			break
		}
	}

	oldParentPath := fs.getPath(hdr.Nodeid)
	newParentPath := fs.getPath(in.Newdir)
	oldPath := filepath.Join(oldParentPath, oldName)
	newPath := filepath.Join(newParentPath, newName)

	resp, err := fs.client.Request(&VFSRequest{
		Op:      OpRename,
		Path:    oldPath,
		NewPath: newPath,
	})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	fs.sendError(hdr.Unique, 0)
}

func (fs *FUSEServer) handleSetattr(hdr *fuseInHeader, data []byte) {
	path := fs.getPath(hdr.Nodeid)

	resp, err := fs.client.Request(&VFSRequest{Op: OpGetattr, Path: path})
	if err != nil || resp.Err != 0 {
		errno := int32(syscall.EIO)
		if resp != nil {
			errno = resp.Err
		}
		fs.sendError(hdr.Unique, errno)
		return
	}

	out := fuseAttrOut{
		AttrValid: 1,
		Attr:      fs.statToAttr(hdr.Nodeid, resp.Stat),
	}

	fs.sendReply(hdr.Unique, unsafe.Slice((*byte)(unsafe.Pointer(&out)), unsafe.Sizeof(out)))
}

func (fs *FUSEServer) handleFsync(hdr *fuseInHeader, data []byte) {
	in := (*fuseOpenIn)(unsafe.Pointer(&data[0]))

	fs.client.Request(&VFSRequest{Op: OpFsync, Handle: uint64(in.Flags)})
	fs.sendError(hdr.Unique, 0)
}

func (fs *FUSEServer) statToAttr(ino uint64, stat *VFSStat) fuseAttr {
	mode := stat.Mode
	if stat.IsDir {
		mode = S_IFDIR | (mode & 0o777)
	} else {
		mode = S_IFREG | (mode & 0o777)
	}

	nlink := uint32(1)
	if stat.IsDir {
		nlink = 2
	}

	return fuseAttr{
		Ino:     ino,
		Size:    uint64(stat.Size),
		Mode:    mode,
		Nlink:   nlink,
		Uid:     uint32(os.Getuid()),
		Gid:     uint32(os.Getgid()),
		Mtime:   uint64(stat.ModTime),
		Ctime:   uint64(stat.ModTime),
		Atime:   uint64(stat.ModTime),
		Blksize: 4096,
	}
}

func (fs *FUSEServer) sendReply(unique uint64, data []byte) {
	hdr := fuseOutHeader{
		Len:    uint32(unsafe.Sizeof(fuseOutHeader{})) + uint32(len(data)),
		Unique: unique,
	}

	buf := make([]byte, hdr.Len)
	copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(&hdr)), unsafe.Sizeof(hdr)))
	copy(buf[unsafe.Sizeof(hdr):], data)

	syscall.Write(fs.fuseFD, buf)
}

func (fs *FUSEServer) sendError(unique uint64, errno int32) {
	hdr := fuseOutHeader{
		Len:    uint32(unsafe.Sizeof(fuseOutHeader{})),
		Error:  errno,
		Unique: unique,
	}

	buf := unsafe.Slice((*byte)(unsafe.Pointer(&hdr)), unsafe.Sizeof(hdr))
	syscall.Write(fs.fuseFD, buf)
}

func (fs *FUSEServer) Close() error {
	return syscall.Close(fs.fuseFD)
}

func dialVsock(cid, port uint32) (int, error) {
	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}

	addr := sockaddrVM{
		Family: AF_VSOCK,
		CID:    cid,
		Port:   port,
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_CONNECT,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		syscall.Close(fd)
		return -1, fmt.Errorf("connect: %w", errno)
	}

	return fd, nil
}

func readFull(fd int, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := syscall.Read(fd, buf[total:])
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, fmt.Errorf("EOF")
		}
		total += n
	}
	return total, nil
}

func writeFull(fd int, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := syscall.Write(fd, buf[total:])
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func main() {
	mountpoint := "/workspace"
	if len(os.Args) > 1 {
		mountpoint = os.Args[1]
	}

	fmt.Printf("Guest FUSE daemon starting, mounting at %s...\n", mountpoint)

	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create mountpoint: %v\n", err)
		os.Exit(1)
	}

	var client *VFSClient
	var err error

	for i := 0; i < 30; i++ {
		client, err = NewVFSClient()
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to VFS server: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	fmt.Println("Connected to VFS server")

	fs, err := NewFUSEServer(mountpoint, client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create FUSE server: %v\n", err)
		os.Exit(1)
	}
	defer fs.Close()

	fmt.Printf("FUSE filesystem mounted at %s\n", mountpoint)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- fs.Serve()
	}()

	select {
	case <-sigCh:
		fmt.Println("Shutting down...")
		syscall.Unmount(mountpoint, 0)
	case err := <-errCh:
		fmt.Fprintf(os.Stderr, "FUSE server error: %v\n", err)
		os.Exit(1)
	}
}
