// Guest FUSE daemon using go-fuse library
// Connects to host VFS server over vsock and mounts at configurable workspace
package guestfused

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	// VMADDR_PORT_ANY requests that the kernel pick any port. It is invalid as a
	// dial target, so a matchlock.vfs_port=VMADDR_PORT_ANY argument is rejected.
	VMADDR_PORT_ANY = 0xffffffff
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
	OpFsyncPath
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
	UID     uint32 `cbor:"uid,omitempty"`
	GID     uint32 `cbor:"gid,omitempty"`
}

type VFSDirEntry struct {
	Name  string `cbor:"name"`
	IsDir bool   `cbor:"is_dir"`
	Mode  uint32 `cbor:"mode"`
	Size  int64  `cbor:"size"`
	Ino   uint64 `cbor:"ino,omitempty"`
}

// VFSClient communicates with host VFS server over vsock
type VFSClient struct {
	fd int
	mu sync.Mutex

	// hardLinkOwners mirrors, for the guest path bookkeeping, go-fuse's
	// bridge-wide stableAttrs coalescing: every hard-link alias that shares a
	// host inode number is coalesced by go-fuse onto the FIRST node created for
	// that inode, and kernel operations for every alias dispatch to that node's
	// ops object (fs/bridge.go addNewChild stableAttrs map). The owner map
	// remembers that first node per host inode so a Lookup of an alias that was
	// NOT created through guest Link (a hard link that pre-existed on the host,
	// or was created by another view of the same tree) can register the looked
	// up name on the surviving node. Without that, unlinking the cached alias
	// leaves the surviving alias resolving to a removed path, and a
	// rename-overwrite can leave it silently resolving to the wrong file.
	//
	// Concurrency: the registry must stay coherent when several aliases of a
	// never-before-seen host inode are looked up in parallel (go-fuse serves
	// FUSE requests from several goroutines, so two first Lookups of the same
	// host inode can overlap). go-fuse itself elects the coalescing winner only
	// later, inside addNewChild (fs/bridge.go), after our NodeLookuper returns;
	// naively registering the node we happen to create can therefore disagree
	// with the node the bridge actually keeps, and alias paths registered on a
	// discarded node are lost. We therefore elect exactly ONE node per host
	// inode up front: hardLinkOwners maps each inode to an ownerSlot whose
	// mutex serializes creation, go-fuse initialization publication, alias-path
	// registration and forget-clearing for that inode. Every concurrent Lookup
	// returns the same fully initialized go-fuse inode (see lookupOrReuse), so
	// the bridge's later coalescing cannot pick a different node.
	//
	// Lock order (source reasoning against go-fuse v2.9.0):
	//   ownerMu -> slot.mu -> (node) pathMu, and slot.mu -> bridge b.mu while a
	//   winner runs NewInode under the slot lock.
	// The reverse order b.mu -> slot.mu never happens: go-fuse calls into our
	// Node* methods without holding b.mu (rawBridge.Lookup/Mkdir/Create/...
	// release b.mu before invoking ops; addNewChild takes b.mu only after our
	// method returned), and NodeOnForgetter.OnForget is invoked from
	// Inode.removeRef AFTER removeRefInner released b.mu and n.mu (fs/inode.go
	// removeRef: OnForget is called outside removeRefInner's locks). So holding
	// slot.mu across NewInode (which takes b.mu) cannot deadlock against a
	// FORGET that later takes slot.mu to clear the registry.
	//
	// The slot mutex is per inode, never a global lock, and no slot lock is
	// held while waiting on the vsock host (Lookup performs its network request
	// before entering the registry).
	ownerMu        sync.Mutex
	hardLinkOwners map[uint64]*ownerSlot
}

// ownerSlot is the election/publication state for ONE host inode number.
//
// node is the canonical VFSNode every hard-link alias of the inode shares in
// this mount session. It is nil only while the first creation for the inode is
// in flight or after the kernel forgot the node (VFSNode.OnForget); the next
// Lookup then elects a fresh node. mu serializes the whole lifecycle so:
//   - only the elected goroutine runs NewInode (go-fuse's initInode under
//     bridge b.mu, fs/bridge.go newInodeUnlocked), and it publishes node only
//     AFTER NewInode returned, so a loser blocked on mu can never observe or
//     return an embed whose go-fuse initialization did not happen yet;
//   - alias-path registration and forget-clearing cannot interleave a removal
//     of the slot's node between another goroutine's read of node and its
//     registration/return.
type ownerSlot struct {
	mu   sync.Mutex
	node *VFSNode
}

// ownerSlotFor returns the owner slot for a host inode number, creating it on
// first sight. Slots are deliberately never deleted: a goroutine elected to
// create the node may still hold the slot pointer while an OnForget clears it,
// and deleting the slot out from under that goroutine would let a second
// election publish into a fresh slot and orphan the first node.
func (c *VFSClient) ownerSlotFor(ino uint64) *ownerSlot {
	if c == nil || ino == 0 {
		return nil
	}
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	if c.hardLinkOwners == nil {
		c.hardLinkOwners = make(map[uint64]*ownerSlot)
	}
	s := c.hardLinkOwners[ino]
	if s == nil {
		s = &ownerSlot{}
		c.hardLinkOwners[ino] = s
	}
	return s
}

// setHardLinkOwner records the first-created node for a host inode number. An
// earlier owner is never displaced: go-fuse keeps coalescing onto it, so the
// registry must keep pointing at it until the node is forgotten (OnForget).
func (c *VFSClient) setHardLinkOwner(ino uint64, n *VFSNode) {
	if c == nil || n == nil || ino == 0 {
		return
	}
	s := c.ownerSlotFor(ino)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.node == nil {
		s.node = n
	}
}

// clearHardLinkOwner clears the registry slot for a host inode number when its
// node is forgotten by the kernel (VFSNode.OnForget). It is a no-op unless the
// slot still points at n, so a racing newer owner is never cleared. The slot
// itself is retained (see ownerSlotFor) and re-used by the next election.
//
// Serialization note (why the identity check alone is race-free for the
// covered orderings): a forget is fully processed in go-fuse only when the
// node's kernel lookup count reached zero (fs/inode.go removeRefInner), and
// OnForget is invoked after removeRefInner released every bridge/node lock.
// Both this clear and every lookupOrReuse registration take the same per-inode
// slot mutex, so a clear can never interleave between a lookup's read of the
// canonical node and its registration on that node: the two are atomic with
// respect to each other. The slot therefore cannot be cleared out from under a
// lookup that is still deciding which node to return.
//
// Residual (documented limitation, requires bridge-level knowledge this
// package does not have): go-fuse processes requests on several goroutines
// (fuse/server.go loop(); fs/bridge.go addNewChild is the only b.mu-protected
// step of a lookup). In the narrow window where the kernel fully forgets a node
// while a Lookup reply for one of its aliases is still being computed, go-fuse
// may call OnForget for a node that a concurrent addNewChild is about to
// re-insert ("resurrect"); if that clear lands after this slot was emptied, the
// registry can be momentarily empty while the bridge keeps the resurrected
// node, and a Lookup in that window would elect a fresh node that addNewChild
// then discards. go-fuse re-forgets such a resurrected node when its kernel
// references drop again, which re-fires OnForget and re-synchronizes the slot;
// the practical exposure is limited to alias registration for lookups issued in
// that window, and it requires the kernel to evict an inode while an alias
// lookup for the same inode is mid-flight. It is not reachable by any of the
// deterministic interleavings below (publication is always completed and
// observed under the slot lock) and no reproduction is retained.
func (c *VFSClient) clearHardLinkOwner(ino uint64, n *VFSNode) {
	if c == nil || n == nil || ino == 0 {
		return
	}
	s := c.ownerSlotFor(ino)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.node == n {
		s.node = nil
	}
}

// lookupOrReuse resolves a freshly looked-up guest name to the canonical node
// for its host inode and returns that node's go-fuse inode. This is the
// election/publication primitive shared by VFSRoot.Lookup and VFSNode.Lookup.
//
// When another hard-link alias of the same host inode was seen earlier in this
// mount session (or is being created concurrently), the looked-up name is
// registered as an alternate on the surviving canonical node — so the alias
// keeps resolving after the cached alias is unlinked or overwritten — and the
// canonical node's go-fuse inode is returned. The SAME fully initialized inode
// object is handed to every concurrent alias, which is what makes go-fuse's
// later stableAttrs coalescing (addNewChild) deterministic: the bridge can only
// ever keep the canonical node for the inode, never a discarded duplicate.
//
// When no canonical node exists yet, exactly one concurrent caller is elected
// (under the per-inode slot lock) to run create, which must construct the new
// VFSNode and initialize its go-fuse inode via parent.NewInode. The winner
// publishes the node only after NewInode returned, and only then releases the
// slot lock, so every caller blocked on the slot observes a fully initialized
// embed (never a half-built one). A caller that loses the election (or arrives
// after a forget cleared the slot) registers under the same lock it read the
// node with, so it can never observe a slot state that a concurrent OnForget
// changed mid-flight.
//
// It returns nil only for a degenerate lookup: a nil client, an effective
// inode number of 0, or a create callback that produced no node/embed. The
// zero-inode case is deliberately fail-closed: the Lookup methods substitute
// inodeForPath when the host reports ino 0, and inodeForPath can never return
// 0, so a nil here is mapped to EIO rather than silently creating a node for a
// nonexistent inode number (the pre-fix code created the node in that case;
// see the EIO guards in both Lookup methods).
func (c *VFSClient) lookupOrReuse(ino uint64, path string, create func() (*VFSNode, *fs.Inode)) *fs.Inode {
	if c == nil || ino == 0 {
		return nil
	}
	s := c.ownerSlotFor(ino)
	s.mu.Lock()
	if s.node == nil {
		// Elected: initialize and publish while holding the slot lock. The
		// node is handed to go-fuse only via the returned embed after this
		// publish, so no concurrent OnForget can fire for it yet (a kernel
		// FORGET for a node go-fuse has never seen is impossible), and no
		// losing Lookup can observe or return a half-initialized embed.
		node, embed := create()
		if node == nil || embed == nil {
			s.mu.Unlock()
			return nil
		}
		s.node = node
		s.mu.Unlock()
		node.registerLinkPath(path)
		return embed
	}
	// A canonical node already exists (or a concurrent winner just published
	// it). Register the looked-up name under the slot lock so a concurrent
	// OnForget cannot clear the slot between our read of node and the
	// registration, then return the shared, already-initialized embed: every
	// alias of the host inode is handed the SAME go-fuse inode, so go-fuse's
	// later stableAttrs coalescing (addNewChild) deterministically keeps this
	// node and can never split aliases across a discarded duplicate.
	node := s.node
	node.registerLinkPath(path)
	embed := node.EmbeddedInode()
	s.mu.Unlock()
	return embed
}

func NewVFSClient() (*VFSClient, error) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, fmt.Errorf("read /proc/cmdline: %w", err)
	}
	port, err := parseVFSPortFromCmdline(string(data))
	if err != nil {
		return nil, err
	}
	fd, err := dialVsock(VMADDR_CID_HOST, port)
	if err != nil {
		return nil, err
	}
	return &VFSClient{fd: fd}, nil
}

// VsockPortVFSDefault is the well-known VFS vsock port used by Firecracker and
// Darwin. The QEMU backend assigns a distinct port per sandbox and passes it via
// matchlock.vfs_port=; when the argument is absent, this default is dialed.
const VsockPortVFSDefault = uint32(VsockPortVFS)

// parseVFSPortFromCmdline resolves the VFS vsock port to dial from the kernel
// command line. It is the single source of truth for port resolution:
//
//   - Argument absent: return the default VFS port (Firecracker/Darwin).
//   - Explicit valid decimal port: return it, preserving all 32 bits.
//   - Empty, malformed, negative, zero, overflow, or VMADDR_PORT_ANY: return an
//     error (fail-closed — never silently fall back to a wrong port).
//   - Duplicate explicit arguments: reject (ambiguous), even when the first is
//     empty.
//
// AF_VSOCK ports are 32-bit, so a kernel-allocated port may exceed 65535 and
// must not be clamped to 16 bits.
func parseVFSPortFromCmdline(cmdline string) (uint32, error) {
	var (
		found string
		seen  bool
	)
	for _, part := range strings.Fields(cmdline) {
		if !strings.HasPrefix(part, "matchlock.vfs_port=") {
			continue
		}
		if seen {
			return 0, fmt.Errorf("matchlock.vfs_port specified more than once")
		}
		seen = true
		found = strings.TrimPrefix(part, "matchlock.vfs_port=")
	}
	if !seen {
		return VsockPortVFSDefault, nil
	}
	if found == "" {
		return 0, fmt.Errorf("matchlock.vfs_port is empty")
	}
	p, err := strconv.ParseUint(found, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("matchlock.vfs_port=%q is not a valid 32-bit port: %w", found, err)
	}
	port := uint32(p)
	if port == 0 {
		return 0, fmt.Errorf("matchlock.vfs_port=0 is invalid")
	}
	if port == VMADDR_PORT_ANY {
		return 0, fmt.Errorf("matchlock.vfs_port=%d is reserved (VMADDR_PORT_ANY)", port)
	}
	return port, nil
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
var _ = (fs.NodeSymlinker)((*VFSRoot)(nil))
var _ = (fs.NodeLinker)((*VFSRoot)(nil))
var _ = (fs.NodeFsyncer)((*VFSRoot)(nil))

// VFSNode represents a file or directory in the VFS.
//
// A VFSNode is the go-fuse ops object for one host inode. Because hard links
// share a host inode number (and therefore a go-fuse stable attr), go-fuse
// coalesces every name that hard-links to the same file onto a single
// fs.Inode and therefore a single VFSNode. A directory entry removed by unlink
// or renamed does not create a new node; the surviving names continue to use
// this node's cached path. A single mutable cached path that is blindly
// repointed to the newest link name breaks the reverse unlink direction
// (link old->new; unlink new while old survives): the surviving old name would
// keep resolving to the removed new path. This node therefore remembers every
// guest name currently believed to map to the inode (pathMu-guarded) and
// repoints the cached path only when the current name disappears but another
// registered link name survives.
type VFSNode struct {
	fs.Inode
	client *VFSClient

	// pathMu guards path, altPaths and dropped. path is the guest path used to
	// address this inode on the host; altPaths holds other guest names known to
	// hard-link to the same host inode. Both are guest absolute paths under the
	// mount root. Accessors must be used after the node is published to the
	// go-fuse tree because kernel operations on a hard-linked inode can run
	// concurrently.
	pathMu   sync.RWMutex
	path     string
	altPaths []string

	// dropped records that the cached path was removed on the host while no
	// alternate name was known yet (unlink of the last tracked alias before a
	// second alias of the same host inode is looked up). When such a name is
	// discovered later it becomes the new cached path, because the old one no
	// longer exists.
	dropped bool

	isDir bool
}

// currentPath returns the guest path used to address this inode on the host.
func (n *VFSNode) currentPath() string {
	if n == nil {
		return ""
	}
	n.pathMu.RLock()
	defer n.pathMu.RUnlock()
	return n.path
}

// registerLinkPath records that another guest name now hard-links to this same
// host inode (called after the host-side link succeeded, or when a Lookup
// discovers an alias that go-fuse coalesces onto this node). The current cached
// path is left unchanged while it still exists, so both directions of a
// link-then-unlink sequence keep a surviving name. When the cached path was
// already removed (dropped) and this is the first newly discovered name, the
// new name is promoted to cached path instead: the old one can no longer
// resolve on the host.
func (n *VFSNode) registerLinkPath(p string) {
	n.pathMu.Lock()
	defer n.pathMu.Unlock()
	if p == "" {
		return
	}
	if n.dropped && p != n.path {
		n.dropped = false
		for i, a := range n.altPaths {
			if a == p {
				n.altPaths = append(n.altPaths[:i], n.altPaths[i+1:]...)
				break
			}
		}
		n.path = p
		return
	}
	if p == n.path {
		return
	}
	for _, a := range n.altPaths {
		if a == p {
			return
		}
	}
	n.altPaths = append(n.altPaths, p)
}

// dropLinkPath forgets a guest name that was just removed on the host (called
// after the host-side unlink succeeded). If the removed name was the cached
// path and another registered name survives, the cached path is promoted to it
// so subsequent requests keep resolving to an existing name. When no other
// name is known the node is marked dropped: the cached path no longer exists
// on the host, and the next alias discovered for this inode (Lookup of a
// pre-existing hard-link name) is promoted to cached path. Open file handles
// keep working through the host fd regardless.
func (n *VFSNode) dropLinkPath(p string) {
	n.pathMu.Lock()
	defer n.pathMu.Unlock()
	if p == n.path {
		if len(n.altPaths) > 0 {
			n.path = n.altPaths[0]
			n.altPaths = n.altPaths[1:]
		} else {
			n.dropped = true
		}
		return
	}
	for i, a := range n.altPaths {
		if a == p {
			n.altPaths = append(n.altPaths[:i], n.altPaths[i+1:]...)
			return
		}
	}
}

// rebasePaths applies a rename rebase to the cached path and to every known
// hard-link name of this inode. updateCachedPathsAfterRename rebases the whole
// moved subtree through this method so that a hard-linked file reached through
// a renamed directory keeps a resolvable path.
func (n *VFSNode) rebasePaths(oldPath, newPath string) {
	n.pathMu.Lock()
	defer n.pathMu.Unlock()
	n.path = rebasePathForRename(n.path, oldPath, newPath)
	for i, a := range n.altPaths {
		n.altPaths[i] = rebasePathForRename(a, oldPath, newPath)
	}
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
var _ = (fs.NodeSymlinker)((*VFSNode)(nil))
var _ = (fs.NodeReadlinker)((*VFSNode)(nil))
var _ = (fs.NodeLinker)((*VFSNode)(nil))
var _ = (fs.NodeFsyncer)((*VFSNode)(nil))

var _ = (fs.NodeOnForgetter)((*VFSNode)(nil))

// OnForget implements fs.NodeOnForgetter. When go-fuse drops this node from the
// bridge tree (the kernel forgot it and no directory entry or open handle keeps
// it alive), the hard-link owner registry entry for its host inode is cleared:
// go-fuse then no longer coalesces new lookups onto this node, so the next
// Lookup of that host inode must create (and register) a fresh node. The node
// is only cleared when the registry still points at it.
func (n *VFSNode) OnForget() {
	if n == nil || n.client == nil {
		return
	}
	n.client.clearHardLinkOwner(n.EmbeddedInode().StableAttr().Ino, n)
}

func fsyncPath(ctx context.Context, client *VFSClient, path string) syscall.Errno {
	resp, err := client.RequestCtx(ctx, &VFSRequest{Op: OpFsyncPath, Path: path})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	return 0
}

func (r *VFSRoot) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	if fh, ok := f.(*VFSFileHandle); ok {
		return fh.Fsync(ctx, flags)
	}
	return fsyncPath(ctx, r.client, r.basePath)
}

func (n *VFSNode) Fsync(ctx context.Context, f fs.FileHandle, flags uint32) syscall.Errno {
	if fh, ok := f.(*VFSFileHandle); ok {
		return fh.Fsync(ctx, flags)
	}
	return fsyncPath(ctx, n.client, n.currentPath())
}

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
	if out.Attr.Ino == 0 {
		isDir := resp.Stat != nil && resp.Stat.IsDir
		out.Attr.Ino = inodeForPath(path, isDir)
	}
	out.Ino = out.Attr.Ino
	// If another name for this host inode was seen earlier (a hard-link alias
	// that pre-existed on the host, discovered by Lookup rather than created
	// through guest Link), or is being looked up concurrently, go-fuse will
	// coalesce this Lookup onto the canonical node for the host inode (see
	// lookupOrReuse). Register the looked-up name there so the alias keeps
	// resolving when the cached alias is later unlinked or overwritten, and
	// hand back that node's (shared, fully initialized) go-fuse inode.
	child := r.client.lookupOrReuse(out.Attr.Ino, path, func() (*VFSNode, *fs.Inode) {
		node := &VFSNode{client: r.client, path: path, isDir: resp.Stat.IsDir}
		stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: out.Attr.Ino}
		return node, r.NewInode(ctx, node, stable)
	})
	// child == nil only for the degenerate cases documented on lookupOrReuse
	// (nil client, or an effective ino of 0 — unreachable after inodeForPath,
	// which never returns 0). EIO here is an intentional fail-closed guard,
	// not a regression: pre-fix code would have created a node for ino 0.
	if child == nil {
		return nil, syscall.EIO
	}
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
		ino := e.Ino
		if ino == 0 {
			ino = inodeForPath(filepath.Join(r.basePath, e.Name), e.IsDir)
		}
		entries[i] = fuse.DirEntry{Name: e.Name, Mode: mode, Ino: ino}
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

	fillEntryAttr(out, resp.Stat, entryAttrDefaults{
		mode:  syscall.S_IFDIR | mode,
		ino:   inodeForPath(path, true),
		isDir: true,
	})
	node := &VFSNode{client: r.client, path: path, isDir: true}
	stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: out.Attr.Ino}
	child := r.NewInode(ctx, node, stable)
	r.client.setHardLinkOwner(out.Attr.Ino, node)
	return child, 0
}

func (r *VFSRoot) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	path := filepath.Join(r.basePath, name)
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpCreate, Path: path, Flags: flags, Mode: mode})
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, nil, 0, syscall.Errno(-resp.Err)
	}

	fillEntryAttr(out, resp.Stat, entryAttrDefaults{
		mode:  syscall.S_IFREG | mode,
		ino:   inodeForPath(path, false),
		isDir: false,
	})
	node := &VFSNode{client: r.client, path: path, isDir: false}
	stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: out.Attr.Ino}
	child := r.NewInode(ctx, node, stable)
	r.client.setHardLinkOwner(out.Attr.Ino, node)
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
	// Keep a surviving hard-link name resolvable when the removed name was one
	// of several links to the same inode (see VFSNode.Unlink). Direct children
	// of the mount root are VFSNode objects; a missing child means the name was
	// never looked up, so no node caches it.
	if child := r.GetChild(name); child != nil {
		if vn, ok := child.Operations().(*VFSNode); ok {
			vn.dropLinkPath(path)
		}
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
		newPath = filepath.Join(p.currentPath(), newName)
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

	// Update cached child/subtree paths so subsequent Open/Read use the new path.
	reconcilePathsAfterRename(r.EmbeddedInode(), name, oldPath, newParent.EmbeddedInode(), newName, newPath)

	return 0
}

// reconcilePathsAfterRename updates the guest path bookkeeping after a
// successful host rename, covering the cases a naive "rebase the moved node"
// misses:
//
//   - rename A->B where A and B are already hard links to the same host inode:
//     POSIX rename is a no-op that keeps BOTH names on the host (go-fuse still
//     moves its tree entry onto the destination name, but host state is
//     unchanged). Both names are kept registered so whichever alias the kernel
//     still holds keeps resolving after the other is unlinked.
//   - rename-overwrite (rename A->B where B existed and was a different
//     inode): the destination name B no longer refers to the destination inode,
//     so B must be dropped from the destination node's registry — otherwise a
//     surviving alias of the destination inode resolves to B, which now holds
//     the MOVED file (silent wrong-content reads).
//   - a plain move rebases the moved node's cached path/alternates and every
//     cached name inside a moved subtree (updateCachedPathsAfterRename).
//
// oldParentInode/newParentInode are inspected BEFORE go-fuse's rawBridge.Rename
// runs MvChild (that happens after this helper returns), so both the source and
// destination child entries are still present in the tree.
func reconcilePathsAfterRename(oldParentInode *fs.Inode, oldName string, oldPath string, newParentInode *fs.Inode, newName string, newPath string) {
	if oldParentInode == nil || newParentInode == nil {
		return
	}
	oldChild := oldParentInode.GetChild(oldName)
	if oldChild == nil {
		// The source name was never looked up in this mount session; go-fuse
		// moves its (absent) tree entry and no node caches the old path.
		return
	}
	destChild := newParentInode.GetChild(newName)
	oldNode, oldIsNode := oldChild.Operations().(*VFSNode)
	var destNode *VFSNode
	destIsNode := false
	if destChild != nil {
		destNode, destIsNode = destChild.Operations().(*VFSNode)
	}
	if oldIsNode && oldNode != nil && destIsNode && destNode != nil && oldChild == destChild {
		// Same-inode rename (A and B are aliases of one host inode): the host
		// kept both names, so keep both registered on the shared node.
		oldNode.registerLinkPath(oldPath)
		oldNode.registerLinkPath(newPath)
		return
	}
	if destIsNode && destNode != nil {
		// The destination entry was overwritten by the rename and no longer
		// refers to the destination inode; drop it so a surviving alias of the
		// destination inode (if any) becomes/keeps the cached path.
		destNode.dropLinkPath(newPath)
	}
	// Rebase the moved node and every cached name in a moved subtree.
	updateCachedPathsAfterRename(oldChild, oldPath, newPath)
}

func updateCachedPathsAfterRename(inode *fs.Inode, oldPath string, newPath string) {
	if inode == nil {
		return
	}

	if node, ok := inode.Operations().(*VFSNode); ok {
		node.rebasePaths(oldPath, newPath)
	}

	for _, child := range inode.Children() {
		updateCachedPathsAfterRename(child, oldPath, newPath)
	}
}

func rebasePathForRename(path string, oldPath string, newPath string) string {
	if path == oldPath {
		return newPath
	}
	if strings.HasPrefix(path, oldPath+"/") {
		return newPath + strings.TrimPrefix(path, oldPath)
	}
	return path
}

// nodePath extracts the absolute guest path from a go-fuse inode embedder so a
// hard-link target can be turned into a VFS request path. It supports the two
// node types this package creates; an unknown embedder returns "" (the caller
// fails closed).
func nodePath(ie fs.InodeEmbedder) string {
	switch n := ie.(type) {
	case *VFSRoot:
		return n.basePath
	case *VFSNode:
		return n.currentPath()
	default:
		return ""
	}
}

// linkPath registers another guest path that hard-links to the same host inode
// as the node identified by a go-fuse inode embedder. go-fuse coalesces every
// hard link to the same host inode onto a single fs.Inode, so each new link
// name shares this node's ops object. The cached path is NOT blindly switched
// to the newest link name: either link may be removed next (git removes the
// temp name after linking the object; a user may equally remove the new name),
// so the new name is recorded as an alternate and the current cached path is
// kept while it exists. unlinkedPath promotes an alternate only when the name
// actually disappears.
func linkPath(ie fs.InodeEmbedder, newPath string) {
	if vn, ok := ie.(*VFSNode); ok {
		vn.registerLinkPath(newPath)
		return
	}
	// A VFSRoot (the mount root) cannot be the target of a hard link: roots are
	// directories and directories cannot be hard-linked. Nothing to register.
}

// VFSRoot / VFSNode symlink, readlink and hard-link operations.

func (r *VFSRoot) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := filepath.Join(r.basePath, name)
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpSymlink, Path: path, Data: []byte(target)})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	fillEntryAttr(out, resp.Stat, entryAttrDefaults{
		mode:  syscall.S_IFLNK | 0777,
		ino:   inodeForPath(path, false),
		isDir: false,
	})
	node := &VFSNode{client: r.client, path: path, isDir: false}
	stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: out.Attr.Ino}
	child := r.NewInode(ctx, node, stable)
	r.client.setHardLinkOwner(out.Attr.Ino, node)
	return child, 0
}

func (n *VFSNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := filepath.Join(n.currentPath(), name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpSymlink, Path: path, Data: []byte(target)})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	fillEntryAttr(out, resp.Stat, entryAttrDefaults{
		mode:  syscall.S_IFLNK | 0777,
		ino:   inodeForPath(path, false),
		isDir: false,
	})
	node := &VFSNode{client: n.client, path: path, isDir: false}
	stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: out.Attr.Ino}
	child := n.NewInode(ctx, node, stable)
	n.client.setHardLinkOwner(out.Attr.Ino, node)
	return child, 0
}

func (n *VFSNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpReadlink, Path: n.currentPath()})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	return resp.Data, 0
}

func (r *VFSRoot) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	targetPath := nodePath(target)
	if targetPath == "" {
		return nil, syscall.EINVAL
	}
	newPath := filepath.Join(r.basePath, name)
	resp, err := r.client.RequestCtx(ctx, &VFSRequest{Op: OpLink, Path: targetPath, NewPath: newPath})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	targetInode := target.EmbeddedInode()
	// Fill the LINK entry with the real stat (host returns it after a successful
	// link). Hard links share the target inode, so a zero-size default entry
	// would corrupt the kernel's cached attrs of the shared inode and turn later
	// reads into EOF.
	fillEntryAttr(out, resp.Stat, entryAttrDefaults{
		mode:  targetInode.StableAttr().Mode,
		ino:   targetInode.StableAttr().Ino,
		isDir: false,
	})
	if out.Attr.Ino == 0 {
		out.Attr.Ino = targetInode.StableAttr().Ino
		out.Ino = out.Attr.Ino
	}
	child := r.NewInode(ctx, &VFSNode{client: r.client, path: newPath, isDir: false}, fs.StableAttr{
		Mode: out.Attr.Mode,
		Ino:  out.Attr.Ino,
	})
	// A hard link shares the target inode (same stable inode number), so go-fuse
	// coalesces this new link name onto the target's existing node. Register the
	// new name as an alternate of that shared node instead of blindly repointing
	// the single cached path to the newest link: either direction of a
	// link-then-unlink sequence must keep a surviving name (git removes the temp
	// name after publishing an object; a user may equally remove the new name).
	linkPath(target, newPath)
	return child, 0
}

func (n *VFSNode) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	targetPath := nodePath(target)
	if targetPath == "" {
		return nil, syscall.EINVAL
	}
	newPath := filepath.Join(n.currentPath(), name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpLink, Path: targetPath, NewPath: newPath})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	targetInode := target.EmbeddedInode()
	// See the VFSRoot.Link comment: fill the entry with the real stat so the
	// shared inode's kernel-side attributes (notably size) are not zeroed by a
	// default entry, and register the new name as an alternate so both
	// directions of a link-then-unlink sequence keep a surviving, resolvable
	// cached path.
	fillEntryAttr(out, resp.Stat, entryAttrDefaults{
		mode:  targetInode.StableAttr().Mode,
		ino:   targetInode.StableAttr().Ino,
		isDir: false,
	})
	if out.Attr.Ino == 0 {
		out.Attr.Ino = targetInode.StableAttr().Ino
		out.Ino = out.Attr.Ino
	}
	child := n.NewInode(ctx, &VFSNode{client: n.client, path: newPath, isDir: false}, fs.StableAttr{
		Mode: out.Attr.Mode,
		Ino:  out.Attr.Ino,
	})
	// See the VFSRoot.Link comment: register the new name as an alternate of the
	// shared (coalesced) target node so both directions of a link-then-unlink
	// sequence keep a surviving, resolvable cached path.
	linkPath(target, newPath)
	return child, 0
}

// VFSNode implementations

func (n *VFSNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpGetattr, Path: n.currentPath()})
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
	cur := n.currentPath()
	// Handle chmod
	if mode, ok := in.GetMode(); ok {
		resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpSetattr, Path: cur, Mode: mode})
		if err != nil {
			return syscall.EIO
		}
		if resp.Err != 0 {
			return syscall.Errno(-resp.Err)
		}
	}

	// Handle truncate
	if sz, ok := in.GetSize(); ok {
		resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpOpen, Path: cur, Flags: uint32(os.O_RDWR)})
		if err != nil {
			return syscall.EIO
		}
		if resp.Err != 0 {
			return syscall.Errno(-resp.Err)
		}
		handle := resp.Handle

		if sz == 0 {
			n.client.RequestCtx(ctx, &VFSRequest{Op: OpRelease, Handle: handle})
			resp, err = n.client.RequestCtx(ctx, &VFSRequest{Op: OpCreate, Path: cur, Mode: 0644})
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
	path := filepath.Join(n.currentPath(), name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpLookup, Path: path})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}

	fillAttr(&out.Attr, resp.Stat)
	if out.Attr.Ino == 0 {
		isDir := resp.Stat != nil && resp.Stat.IsDir
		out.Attr.Ino = inodeForPath(path, isDir)
	}
	out.Ino = out.Attr.Ino
	// See VFSRoot.Lookup: a hard-link alias discovered by Lookup (not created
	// through guest Link) coalesces onto the canonical node for this host
	// inode; register the name on that surviving node (serialized with any
	// concurrent first Lookup and with OnForget, see lookupOrReuse) so the
	// alias keeps resolving when the cached alias is later unlinked or
	// overwritten.
	child := n.client.lookupOrReuse(out.Attr.Ino, path, func() (*VFSNode, *fs.Inode) {
		node := &VFSNode{client: n.client, path: path, isDir: resp.Stat.IsDir}
		stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: out.Attr.Ino}
		return node, n.NewInode(ctx, node, stable)
	})
	// See the identical guard in VFSRoot.Lookup: nil only for the degenerate
	// lookupOrReuse cases; EIO is an intentional fail-closed guard (the old
	// code created the node even for an effective ino of 0).
	if child == nil {
		return nil, syscall.EIO
	}
	return child, 0
}

func (n *VFSNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	cur := n.currentPath()
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpReaddir, Path: cur})
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
		ino := e.Ino
		if ino == 0 {
			ino = inodeForPath(filepath.Join(cur, e.Name), e.IsDir)
		}
		entries[i] = fuse.DirEntry{Name: e.Name, Mode: mode, Ino: ino}
	}
	return fs.NewListDirStream(entries), 0
}

func (n *VFSNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	cur := n.currentPath()
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpOpen, Path: cur, Flags: flags})
	if err != nil {
		return nil, 0, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, 0, syscall.Errno(-resp.Err)
	}
	return &VFSFileHandle{client: n.client, handle: resp.Handle, path: cur}, 0, 0
}

func (n *VFSNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := filepath.Join(n.currentPath(), name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpMkdir, Path: path, Mode: mode})
	if err != nil {
		return nil, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}

	fillEntryAttr(out, resp.Stat, entryAttrDefaults{
		mode:  syscall.S_IFDIR | mode,
		ino:   inodeForPath(path, true),
		isDir: true,
	})
	node := &VFSNode{client: n.client, path: path, isDir: true}
	stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: out.Attr.Ino}
	child := n.NewInode(ctx, node, stable)
	n.client.setHardLinkOwner(out.Attr.Ino, node)
	return child, 0
}

func (n *VFSNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (inode *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	path := filepath.Join(n.currentPath(), name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpCreate, Path: path, Flags: flags, Mode: mode})
	if err != nil {
		return nil, nil, 0, syscall.EIO
	}
	if resp.Err != 0 {
		return nil, nil, 0, syscall.Errno(-resp.Err)
	}

	fillEntryAttr(out, resp.Stat, entryAttrDefaults{
		mode:  syscall.S_IFREG | mode,
		ino:   inodeForPath(path, false),
		isDir: false,
	})
	node := &VFSNode{client: n.client, path: path, isDir: false}
	stable := fs.StableAttr{Mode: out.Attr.Mode, Ino: out.Attr.Ino}
	child := n.NewInode(ctx, node, stable)
	n.client.setHardLinkOwner(out.Attr.Ino, node)
	handle := &VFSFileHandle{client: n.client, handle: resp.Handle, path: path}
	return child, handle, 0, 0
}

func (n *VFSNode) Unlink(ctx context.Context, name string) syscall.Errno {
	path := filepath.Join(n.currentPath(), name)
	resp, err := n.client.RequestCtx(ctx, &VFSRequest{Op: OpUnlink, Path: path})
	if err != nil {
		return syscall.EIO
	}
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	// The removed name may be one of several hard links to an inode that is
	// still alive under another name (git removes a temp object name after
	// publishing it; a user may equally remove the newest link name). Drop the
	// removed name from the child node's registered link paths so the surviving
	// name keeps resolving through the shared (coalesced) node. The child may be
	// absent when the name was never looked up in this mount session; then no
	// node caches that path.
	if child := n.GetChild(name); child != nil {
		if vn, ok := child.Operations().(*VFSNode); ok {
			vn.dropLinkPath(path)
		}
	}
	return 0
}

func (n *VFSNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	path := filepath.Join(n.currentPath(), name)
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
	oldPath := filepath.Join(n.currentPath(), name)
	var newPath string
	switch p := newParent.(type) {
	case *VFSRoot:
		newPath = filepath.Join(p.basePath, newName)
	case *VFSNode:
		newPath = filepath.Join(p.currentPath(), newName)
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

	// Update cached child/subtree paths so subsequent Open/Read use the new path.
	reconcilePathsAfterRename(n.EmbeddedInode(), name, oldPath, newParent.EmbeddedInode(), newName, newPath)

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
	if stat == nil {
		return
	}
	attr.Size = uint64(stat.Size)
	attr.Mtime = uint64(stat.ModTime)
	attr.Ctime = uint64(stat.ModTime)
	attr.Atime = uint64(stat.ModTime)
	attr.Blksize = 4096
	attr.Blocks = (uint64(stat.Size) + 511) / 512
	attr.Ino = stat.Ino
	attr.Uid = stat.UID
	attr.Gid = stat.GID
	if stat.Mode&uint32(os.ModeSymlink) != 0 {
		// A symbolic link must be advertised as such so the kernel treats the
		// inode as a symlink and issues a Readlink instead of a data read.
		attr.Mode = syscall.S_IFLNK | (stat.Mode & 0777)
		attr.Nlink = 1
	} else if stat.IsDir {
		attr.Mode = syscall.S_IFDIR | (stat.Mode & 0777)
		attr.Nlink = 2
	} else {
		attr.Mode = syscall.S_IFREG | (stat.Mode & 0777)
		attr.Nlink = 1
	}
}

type entryAttrDefaults struct {
	mode  uint32
	ino   uint64
	isDir bool
}

func fillEntryAttr(out *fuse.EntryOut, stat *VFSStat, defaults entryAttrDefaults) {
	if stat != nil {
		fillAttr(&out.Attr, stat)
		out.Ino = out.Attr.Ino
		return
	}
	out.Attr.Mode = defaults.mode
	out.Attr.Blksize = 4096
	out.Attr.Ino = defaults.ino
	out.Ino = defaults.ino
	if defaults.isDir {
		out.Attr.Nlink = 2
	} else {
		out.Attr.Nlink = 1
	}
}

func inodeForPath(path string, isDir bool) uint64 {
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
		return ""
	}
	for _, part := range strings.Fields(string(data)) {
		if strings.HasPrefix(part, "matchlock.workspace=") {
			return strings.TrimPrefix(part, "matchlock.workspace=")
		}
	}
	return ""
}

func Run() {
	// Get workspace from kernel cmdline.
	mountpoint := getWorkspaceFromCmdline()
	if len(os.Args) > 1 {
		mountpoint = os.Args[1]
	}
	if mountpoint == "" {
		fmt.Fprintln(os.Stderr, "Missing workspace mountpoint")
		os.Exit(1)
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
			Debug:             os.Getenv("MATCHLOCK_FUSE_DEBUG") != "",
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
