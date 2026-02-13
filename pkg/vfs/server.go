package vfs

import (
	"encoding/binary"
	"io"
	"net"
	"os"
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
}

type VFSDirEntry struct {
	Name  string `cbor:"name"`
	IsDir bool   `cbor:"is_dir"`
	Mode  uint32 `cbor:"mode"`
	Size  int64  `cbor:"size"`
}

type VFSServer struct {
	provider Provider
	handles  sync.Map
	nextFH   uint64
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
		return &VFSResponse{Stat: statFromInfo(info)}

	case OpSetattr:
		if err := provider.Chmod(req.Path, os.FileMode(req.Mode)); err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		info, err := provider.Stat(req.Path)
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{Stat: statFromInfo(info)}

	case OpOpen:
		h, err := provider.Open(req.Path, int(req.Flags), os.FileMode(req.Mode))
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		fh := atomic.AddUint64(&s.nextFH, 1)
		s.handles.Store(fh, h)
		return &VFSResponse{Handle: fh}

	case OpCreate:
		h, err := provider.Create(req.Path, os.FileMode(req.Mode))
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		fh := atomic.AddUint64(&s.nextFH, 1)
		s.handles.Store(fh, h)
		return &VFSResponse{Handle: fh}

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
		n, err := h.WriteAt(req.Data, req.Offset)
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{Written: uint32(n)}

	case OpRelease:
		if hi, ok := s.handles.LoadAndDelete(req.Handle); ok {
			hi.(Handle).Close()
		}
		return &VFSResponse{}

	case OpReaddir:
		entries, err := provider.ReadDir(req.Path)
		if err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{Entries: direntsFromEntries(entries)}

	case OpMkdir:
		if err := provider.Mkdir(req.Path, os.FileMode(req.Mode)); err != nil {
			return &VFSResponse{Err: errnoFromError(err)}
		}
		return &VFSResponse{}

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

func statFromInfo(info FileInfo) *VFSStat {
	return &VFSStat{
		Size:    info.Size(),
		Mode:    uint32(info.Mode()),
		ModTime: info.ModTime().Unix(),
		IsDir:   info.IsDir(),
	}
}

func direntsFromEntries(entries []DirEntry) []VFSDirEntry {
	result := make([]VFSDirEntry, len(entries))
	for i, e := range entries {
		info, _ := e.Info()
		result[i] = VFSDirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Mode:  uint32(e.Type()),
			Size:  info.Size(),
		}
	}
	return result
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
