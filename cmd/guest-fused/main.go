// Guest FUSE daemon using go-fuse library
// Connects to host VFS server over vsock and mounts at configurable workspace
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/fxamacker/cbor/v2"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	AF_VSOCK        = 40
	VMADDR_CID_HOST = 2
	VsockPortVFS    = 5001
)

// VFS protocol (must match pkg/vfs/server.go)
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
	OpTruncate
	OpSymlink
	OpReadlink
	OpLink
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
	UID     uint32 `cbor:"uid,omitempty"`
	GID     uint32 `cbor:"gid,omitempty"`
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

// VFSClient communicates with host VFS server over vsock
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

func (c *VFSClient) RequestCtx(ctx context.Context, req *VFSRequest) (*VFSResponse, error) {
	if req != nil {
		if caller, ok := fuse.FromContext(ctx); ok {
			req.UID = caller.Uid
			req.GID = caller.Gid
		}
	}
	return c.Request(req)
}

// VFSRoot is the root node of the FUSE filesystem
type VFSRoot struct {
	fs.Inode
	client   *VFSClient
	basePath string
}

var _ = (fs.NodeGetattrer)((*VFSRoot)(nil))
var _ = (fs.NodeLookuper)((*VFSRoot)(nil))
var _ = (fs.NodeReaddirer)((*VFSRoot)(nil))
var _ = (fs.NodeMkdirer)((*VFSRoot)(nil))
var _ = (fs.NodeCreater)((*VFSRoot)(nil))
var _ = (fs.NodeUnlinker)((*VFSRoot)(nil))
var _ = (fs.NodeRmdirer)((*VFSRoot)(nil))
var _ = (fs.NodeRenamer)((*VFSRoot)(nil))

// VFSNode represents a file or directory in the VFS
type VFSNode struct {
	fs.Inode
	client *VFSClient
	path   string
	isDir  bool
}

var _ = (fs.NodeGetattrer)((*VFSNode)(nil))
var _ = (fs.NodeLookuper)((*VFSNode)(nil))
var _ = (fs.NodeReaddirer)((*VFSNode)(nil))
var _ = (fs.NodeOpener)((*VFSNode)(nil))
var _ = (fs.NodeMkdirer)((*VFSNode)(nil))
var _ = (fs.NodeCreater)((*VFSNode)(nil))
var _ = (fs.NodeUnlinker)((*VFSNode)(nil))
var _ = (fs.NodeRmdirer)((*VFSNode)(nil))
var _ = (fs.NodeRenamer)((*VFSNode)(nil))
var _ = (fs.NodeSetattrer)((*VFSNode)(nil))

func (r *VFSRoot) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpGetattr, Path: r.basePath})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	fillAttr(&out.Attr, resp.Stat)
	return 0
}

func (r *VFSRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := filepath.Join(r.basePath, name)
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpLookup, Path: path})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}

	fillAttr(&out.Attr, resp.Stat)
	node := &VFSNode{client: r.client, path: path, isDir: resp.Stat.IsDir}
	stable := fs.StableAttr{Mode: out.Attr.Mode}
	child := r.NewInode(ctx, node, stable)
	return child, 0
}

func (r *VFSRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpReaddir, Path: r.basePath})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}

	entries := make([]fuse.DirEntry, len(resp.Entries))
	for i, e := range resp.Entries {
		mode := uint32(syscall.S_IFREG)
		if e.IsDir {
			mode = syscall.S_IFDIR
		}
		entries[i] = fuse.DirEntry{Name: e.Name, Mode: mode}
	}
	return fs.NewListDirStream(entries), 0
}

func (r *VFSRoot) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := filepath.Join(r.basePath, name)
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpMkdir, Path: path, Mode: mode})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}

	out.Attr.Mode = syscall.S_IFDIR | mode
	node := &VFSNode{client: r.client, path: path, isDir: true}
	stable := fs.StableAttr{Mode: out.Attr.Mode}
	child := r.NewInode(ctx, node, stable)
	return child, 0
}

func (r *VFSRoot) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	path := filepath.Join(r.basePath, name)
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpCreate, Path: path, Mode: mode})
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, nil, 0, syscall.Errno(-resp.Err)
	}

	out.Attr.Mode = syscall.S_IFREG | mode
	node := &VFSNode{client: r.client, path: path, isDir: false}
	stable := fs.StableAttr{Mode: out.Attr.Mode}
	child := r.NewInode(ctx, node, stable)
	handle := &VFSFileHandle{client: r.client, handle: resp.Handle, path: path}
	return child, handle, 0, 0
}

func (r *VFSRoot) Unlink(ctx context.Context, name string) syscall.Errno {
	path := filepath.Join(r.basePath, name)
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpUnlink, Path: path})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	return 0
}

func (r *VFSRoot) Rmdir(ctx context.Context, name string) syscall.Errno {
	path := filepath.Join(r.basePath, name)
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpRmdir, Path: path})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	return 0
}

func (r *VFSRoot) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	oldPath := filepath.Join(r.basePath, name)
	var newPath string
	switch p := newParent.(type) {
	case *VFSRoot:
		newPath = filepath.Join(p.basePath, newName)
	case *VFSNode:
		newPath = filepath.Join(p.path, newName)
	default:
		return syscall.EINVAL
	}

	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpRename, Path: oldPath, NewPath: newPath})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	return 0
}

// VFSNode implementations

func (n *VFSNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpGetattr, Path: n.path})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	fillAttr(&out.Attr, resp.Stat)
	return 0
}

func (n *VFSNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	// Handle chmod
	if mode, ok := in.GetMode(); ok {
		resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpSetattr, Path: n.path, Mode: mode})
		if err != nil {
			return syscall.EIO
		}
		if resp.Err != 0 {
			return syscall.Errno(-resp.Err)
		}
	}

	// Handle truncate
	if sz, ok := in.GetSize(); ok {
		resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpOpen, Path: n.path, Flags: uint32(os.O_RDWR)})
		if err != nil {
			return syscall.EIO
		}
		if resp.Err != 0 {
			return syscall.Errno(-resp.Err)
		}
		handle := resp.Handle

		if sz == 0 {
			n.client.RequestCtx(ctx, &VFSRequest{Op: OpRelease, Handle: handle})
			resp, err = n.client.RequestCtx(ctx, &VFSRequest{Op: OpCreate, Path: n.path, Mode: 0644})
			if err != nil {
				return syscall.EIO
			}
			if resp.Err != 0 {
				return syscall.Errno(-resp.Err)
			}
			n.client.RequestCtx(ctx, &VFSRequest{Op: OpRelease, Handle: resp.Handle})
		} else {
			n.client.RequestCtx(ctx, &VFSRequest{Op: OpRelease, Handle: handle})
		}
	}

	return n.Getattr(ctx, fh, out)
}

func (n *VFSNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := filepath.Join(n.path, name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpLookup, Path: path})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}

	fillAttr(&out.Attr, resp.Stat)
	node := &VFSNode{client: n.client, path: path, isDir: resp.Stat.IsDir}
	stable := fs.StableAttr{Mode: out.Attr.Mode}
	child := n.NewInode(ctx, node, stable)
	return child, 0
}

func (n *VFSNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpReaddir, Path: n.path})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}

	entries := make([]fuse.DirEntry, len(resp.Entries))
	for i, e := range resp.Entries {
		mode := uint32(syscall.S_IFREG)
		if e.IsDir {
			mode = syscall.S_IFDIR
		}
		entries[i] = fuse.DirEntry{Name: e.Name, Mode: mode}
	}
	return fs.NewListDirStream(entries), 0
}

func (n *VFSNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpOpen, Path: n.path, Flags: flags})
	if err != nil {
		return nil, 0, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, 0, syscall.Errno(-resp.Err)
	}
	return &VFSFileHandle{client: n.client, handle: resp.Handle, path: n.path}, 0, 0
}

func (n *VFSNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := filepath.Join(n.path, name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpMkdir, Path: path, Mode: mode})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}

	out.Attr.Mode = syscall.S_IFDIR | mode
	node := &VFSNode{client: n.client, path: path, isDir: true}
	stable := fs.StableAttr{Mode: out.Attr.Mode}
	child := n.NewInode(ctx, node, stable)
	return child, 0
}

func (n *VFSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	path := filepath.Join(n.path, name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpCreate, Path: path, Mode: mode})
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, nil, 0, syscall.Errno(-resp.Err)
	}

	out.Attr.Mode = syscall.S_IFREG | mode
	node := &VFSNode{client: n.client, path: path, isDir: false}
	stable := fs.StableAttr{Mode: out.Attr.Mode}
	child := n.NewInode(ctx, node, stable)
	handle := &VFSFileHandle{client: n.client, handle: resp.Handle, path: path}
	return child, handle, 0, 0
}

func (n *VFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	path := filepath.Join(n.path, name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpUnlink, Path: path})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	return 0
}

func (n *VFSNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	path := filepath.Join(n.path, name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpRmdir, Path: path})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	return 0
}

func (n *VFSNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	oldPath := filepath.Join(n.path, name)
	var newPath string
	switch p := newParent.(type) {
	case *VFSRoot:
		newPath = filepath.Join(p.basePath, newName)
	case *VFSNode:
		newPath = filepath.Join(p.path, newName)
	default:
		return syscall.EINVAL
	}

	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpRename, Path: oldPath, NewPath: newPath})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	return 0
}

// VFSFileHandle handles read/write operations on open files
type VFSFileHandle struct {
	client *VFSClient
	handle uint64
	path   string
}

var _ = (fs.FileReader)((*VFSFileHandle)(nil))
var _ = (fs.FileWriter)((*VFSFileHandle)(nil))
var _ = (fs.FileFsyncer)((*VFSFileHandle)(nil))
var _ = (fs.FileReleaser)((*VFSFileHandle)(nil))
var _ = (fs.FileGetattrer)((*VFSFileHandle)(nil))

func (h *VFSFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	resp, err := h.client.RequestCtx(ctx, &VFSRequest{
		Op:     OpRead,
		Handle: h.handle,
		Offset: off,
		Size:   uint32(len(dest)),
	})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	return fuse.ReadResultData(resp.Data), 0
}

func (h *VFSFileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	resp, err := h.client.RequestCtx(ctx, &VFSRequest{
		Op:     OpWrite,
		Handle: h.handle,
		Offset: off,
		Data:   data,
	})
	if err != nil {
		return 0, syscall.EIO
	}
	if resp.Err != 0 {
		return 0, syscall.Errno(-resp.Err)
	}
	return resp.Written, 0
}

func (h *VFSFileHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	resp, err := h.client.RequestCtx(ctx, &VFSRequest{Op: OpFsync, Handle: h.handle})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	return 0
}

func (h *VFSFileHandle) Release(ctx context.Context) syscall.Errno {
	h.client.RequestCtx(ctx, &VFSRequest{Op: OpRelease, Handle: h.handle})
	return 0
}

func (h *VFSFileHandle) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	resp, err := h.client.RequestCtx(ctx, &VFSRequest{Op: OpGetattr, Path: h.path})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	fillAttr(&out.Attr, resp.Stat)
	return 0
}

func fillAttr(attr *fuse.Attr, stat *VFSStat) {
	attr.Size = uint64(stat.Size)
	attr.Mtime = uint64(stat.ModTime)
	attr.Ctime = uint64(stat.ModTime)
	attr.Atime = uint64(stat.ModTime)
	attr.Blksize = 4096
	attr.Blocks = (uint64(stat.Size) + 511) / 512
	if stat.IsDir {
		attr.Mode = syscall.S_IFDIR | (stat.Mode & 0777)
		attr.Nlink = 2
	} else {
		attr.Mode = syscall.S_IFREG | (stat.Mode & 0777)
		attr.Nlink = 1
	}
}

// Vsock helpers

type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Zero      [4]byte
}

func dialVsock(cid, port uint32) (int, error) {
	fd, err := syscall.Socket(AF_VSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, errx.Wrap(ErrSocket, err)
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
		return -1, errx.Wrap(ErrConnect, errno)
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
			return total, ErrEOF
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

func getWorkspaceFromCmdline() string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "/workspace"
	}
	for _, part := range strings.Fields(string(data)) {
		if strings.HasPrefix(part, "matchlock.workspace=") {
			return strings.TrimPrefix(part, "matchlock.workspace=")
		}
	}
	return "/workspace"
}

func main() {
	// Get workspace from kernel cmdline or use default
	mountpoint := getWorkspaceFromCmdline()
	if len(os.Args) > 1 {
		mountpoint = os.Args[1]
	}

	fmt.Printf("Guest FUSE daemon (go-fuse) starting, mounting at %s...\n", mountpoint)

	if err := os.MkdirAll(mountpoint, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create mountpoint: %v\n", err)
		os.Exit(1)
	}

	// Connect to host VFS server with retries
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

	// Create root node - basePath must match the VFS mount configuration on host
	root := &VFSRoot{client: client, basePath: mountpoint}

	// Mount with go-fuse using DirectMountStrict to avoid fusermount dependency
	opts := &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther:        true,
			FsName:            "matchlock",
			Name:              "fuse.matchlock",
			Debug:             false,
			DirectMountStrict: true,
		},
		AttrTimeout:  &[]time.Duration{time.Second}[0],
		EntryTimeout: &[]time.Duration{time.Second}[0],
	}

	server, err := fs.Mount(mountpoint, root, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to mount: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("FUSE filesystem mounted at %s\n", mountpoint)

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("Shutting down...")
		server.Unmount()
	}()

	// Serve until unmounted
	server.Wait()
}
