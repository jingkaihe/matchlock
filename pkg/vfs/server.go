package vfs

import (
	"encoding/binary"
	"hash/fnv"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/fxamacker/cbor/v2"
)

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
	Ino     uint64 `cbor:"ino,omitempty"`
}

type VFSDirEntry struct {
	Name  string `cbor:"name"`
	IsDir bool   `cbor:"is_dir"`
	Mode  uint32 `cbor:"mode"`
	Size  int64  `cbor:"size"`
	Ino   uint64 `cbor:"ino,omitempty"`
}

type VFSServer struct {
	provider   Provider
	handles    sync.Map
	appendMode sync.Map // fh -> bool; tracks handles opened with O_APPEND
	nextFH     uint64
}

func NewVFSServer(provider Provider) *VFSServer {
	return &VFSServer{provider: provider}
}

func (s *VFSServer) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go s.HandleConnection(conn)
	}
}

// HandleConnection handles a single VFS connection. Exported for use by platform-specific backends.
func (s *VFSServer) HandleConnection(conn net.Conn) {
	defer conn.Close()

	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		msgLen := binary.BigEndian.Uint32(lenBuf[:])

		msgBuf := make([]byte, msgLen)
		if _, err := io.ReadFull(conn, msgBuf); err != nil {
			return
		}

		var req VFSRequest
		if err := cbor.Unmarshal(msgBuf, &req); err != nil {
			return
		}

		resp := s.dispatch(&req)

		respBuf, err := cbor.Marshal(resp)
		if err != nil {
			return
		}

		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(respBuf)))
		if _, err := conn.Write(lenBuf[:]); err != nil {
			return
		}
		if _, err := conn.Write(respBuf); err != nil {
			return
		}
	}
}

func (s *VFSServer) dispatch(req *VFSRequest) *VFSResponse {
	provider := s.provider
	if callerAware, ok := provider.(interface {
		withCaller(uid, gid int) Provider
	}); ok {
		provider = callerAware.withCaller(int(req.UID), int(req.GID))
	}

	switch req.Op {
	case OpLookup, OpGetattr:
		info, err := provider.Stat(req.Path)
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{Stat: statFromInfo(req.Path, info)}

	case OpSetattr:
		if err := provider.Chmod(req.Path, os.FileMode(req.Mode)); err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		info, err := provider.Stat(req.Path)
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{Stat: statFromInfo(req.Path, info)}

	case OpOpen:
		h, err := provider.Open(req.Path, int(req.Flags), os.FileMode(req.Mode))
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		fh := atomic.AddUint64(&s.nextFH, 1)
		s.handles.Store(fh, h)
		if req.Flags&uint32(syscall.O_APPEND) != 0 {
			s.appendMode.Store(fh, true)
		}
		return &VFSResponse{Handle: fh}

	case OpCreate:
		h, err := provider.Create(req.Path, os.FileMode(req.Mode))
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		info, err := h.Stat()
		if err != nil {
			_ = h.Close()
			return &VFSResponse{Err: errnoFromError(err)}
		}
		fh := atomic.AddUint64(&s.nextFH, 1)
		s.handles.Store(fh, h)
		return &VFSResponse{Handle: fh, Stat: statFromInfo(req.Path, info)}

	case OpRead:
		hi, ok := s.handles.Load(req.Handle)
		if !ok {
			return &VFSResponse{Err: -int32(syscall.EBADF)}
		}
		h := hi.(Handle)
		buf := make([]byte, req.Size)
		n, err := h.ReadAt(buf, req.Offset)
		if err != nil && err != io.EOF {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{Data: buf[:n]}

	case OpWrite:
		hi, ok := s.handles.Load(req.Handle)
		if !ok {
			return &VFSResponse{Err: -int32(syscall.EBADF)}
		}
		h := hi.(Handle)
		// Go's os.File.WriteAt rejects files opened with O_APPEND.
		// For append-mode handles, use Write() which lets the kernel
		// atomically seek to EOF and write.
		var n int
		var err error
		if _, isAppend := s.appendMode.Load(req.Handle); isAppend {
			n, err = h.Write(req.Data)
		} else {
			n, err = h.WriteAt(req.Data, req.Offset)
		}
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{Written: uint32(n)}

	case OpRelease:
		if hi, ok := s.handles.LoadAndDelete(req.Handle); ok {
			hi.(Handle).Close()
		}
		s.appendMode.Delete(req.Handle)
		return &VFSResponse{}

	case OpReaddir:
		entries, err := provider.ReadDir(req.Path)
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{Entries: direntsFromEntries(req.Path, entries)}

	case OpMkdir:
		if err := provider.Mkdir(req.Path, os.FileMode(req.Mode)); err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		info, err := provider.Stat(req.Path)
		if err != nil {
			return &VFSResponse{}
		}
		return &VFSResponse{Stat: statFromInfo(req.Path, info)}

	case OpMkdirAll:
		mp, ok := provider.(*MemoryProvider)
		if ok {
			if err := mp.MkdirAll(req.Path, os.FileMode(req.Mode)); err != nil {
				return &VFSResponse{Err: errnoFromError(err)}
			}
			return &VFSResponse{}
		}
		return &VFSResponse{Err: -int32(syscall.ENOSYS)}

	case OpUnlink:
		if err := provider.Remove(req.Path); err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{}

	case OpRmdir:
		if err := provider.Remove(req.Path); err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{}

	case OpRename:
		if err := provider.Rename(req.Path, req.NewPath); err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{}

	case OpFsync:
		if hi, ok := s.handles.Load(req.Handle); ok {
			hi.(Handle).Sync()
		}
		return &VFSResponse{}

	default:
		return &VFSResponse{Err: -int32(syscall.ENOSYS)}
	}
}

func errnoFromError(err error) int32 {
	if err == nil {
		return 0
	}
	if errno, ok := err.(syscall.Errno); ok {
		return -int32(errno)
	}
	if os.IsNotExist(err) {
		return -int32(syscall.ENOENT)
	}
	if os.IsPermission(err) {
		return -int32(syscall.EACCES)
	}
	if os.IsExist(err) {
		return -int32(syscall.EEXIST)
	}
	return -int32(syscall.EIO)
}

func statFromInfo(path string, info FileInfo) *VFSStat {
	return &VFSStat{
		Size:    info.Size(),
		Mode:    uint32(info.Mode()),
		ModTime: info.ModTime().Unix(),
		IsDir:   info.IsDir(),
		Ino:     inodeFromFileInfo(path, info, info.IsDir()),
	}
}

func direntsFromEntries(parentPath string, entries []DirEntry) []VFSDirEntry {
	result := make([]VFSDirEntry, len(entries))
	for i, e := range entries {
		childPath := filepath.Join(parentPath, e.Name())
		info, infoErr := e.Info()
		var size int64
		ino := syntheticInode(childPath, e.IsDir())
		if infoErr == nil {
			size = info.Size()
			ino = inodeFromFileInfo(childPath, info, e.IsDir())
		}
		result[i] = VFSDirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Mode:  uint32(e.Type()),
			Size:  size,
			Ino:   ino,
		}
	}
	return result
}

func inodeFromFileInfo(path string, info interface {
	Sys() any
}, isDir bool) uint64 {
	if info != nil {
		if ino := inodeFromSys(info.Sys()); ino != 0 {
			return ino
		}
	}
	return syntheticInode(path, isDir)
}

func inodeFromSys(sys any) uint64 {
	switch st := sys.(type) {
	case *syscall.Stat_t:
		if st == nil || st.Ino == 0 {
			return 0
		}
		return namespacedInode(uint64(st.Dev), uint64(st.Ino))
	default:
		return 0
	}
}

func namespacedInode(dev, ino uint64) uint64 {
	var pair [16]byte
	binary.BigEndian.PutUint64(pair[0:8], dev)
	binary.BigEndian.PutUint64(pair[8:16], ino)

	h := fnv.New64a()
	_, _ = h.Write(pair[:])
	out := h.Sum64()
	if out == 0 || out == 1 {
		out += 2
	}
	return out
}

func syntheticInode(path string, isDir bool) uint64 {
	clean := filepath.Clean(path)
	if clean == "/" {
		return 1
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(clean))
	if isDir {
		_, _ = h.Write([]byte{'d'})
	} else {
		_, _ = h.Write([]byte{'f'})
	}
	ino := h.Sum64()
	if ino == 0 || ino == 1 {
		ino += 2
	}
	return ino
}

// ServeUDS starts the VFS server on a Unix domain socket
// This is used by Firecracker vsock which exposes guest vsock ports as UDS
func (s *VFSServer) ServeUDS(socketPath string) error {
	// Remove existing socket if present
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	return s.Serve(listener)
}

// ServeUDSBackground starts the VFS server on a Unix domain socket in a goroutine
// Returns a function to stop the server
func (s *VFSServer) ServeUDSBackground(socketPath string) (stop func(), err error) {
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	go s.Serve(listener)

	return func() {
		listener.Close()
	}, nil
}
