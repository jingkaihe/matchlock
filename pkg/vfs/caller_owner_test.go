package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callerAware is the per-request ownership hook the VFS server invokes in
// dispatch; declared here so the tests can drive it explicitly without
// re-declaring the anonymous interface at every call site.
type callerAware interface {
	withCaller(uid, gid int) Provider
}

var _ callerAware = (*RealFSProvider)(nil)
var _ callerAware = (*MountRouter)(nil)
var _ callerAware = (*ReadonlyProvider)(nil)
var _ callerAware = (*interceptProvider)(nil)

func newCallerFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0755))
	return dir, filepath.Join(dir, "file.txt")
}

// TestServerReportsCallerOwnerForHostFSStat is the always-runnable regression
// for the FUSE dubious-ownership failure: dispatching a lookup/getattr with an
// explicit non-zero caller must return that caller's uid/gid for a host_fs
// stat with no owner override. Before the fix the provider reported 0/0.
func TestServerReportsCallerOwnerForHostFSStat(t *testing.T) {
	dir, _ := newCallerFixture(t)
	s := NewVFSServer(NewRealFSProvider(dir))

	getattr := s.dispatch(&VFSRequest{Op: OpGetattr, Path: "file.txt", UID: 1234, GID: 5678})
	require.Equal(t, int32(0), getattr.Err)
	require.NotNil(t, getattr.Stat)
	assert.Equal(t, uint32(1234), getattr.Stat.UID)
	assert.Equal(t, uint32(5678), getattr.Stat.GID)

	lookup := s.dispatch(&VFSRequest{Op: OpLookup, Path: "sub", UID: 1234, GID: 5678})
	require.Equal(t, int32(0), lookup.Err)
	require.NotNil(t, lookup.Stat)
	assert.True(t, lookup.Stat.IsDir)
	assert.Equal(t, uint32(1234), lookup.Stat.UID)
	assert.Equal(t, uint32(5678), lookup.Stat.GID)
}

// TestServerReportsCallerOwnerThroughRouter mirrors the real wiring used by the
// real-FUSE git test and the sandbox: the server's provider is a MountRouter,
// so the caller identity must be propagated into the resolved mount provider.
func TestServerReportsCallerOwnerThroughRouter(t *testing.T) {
	dir, _ := newCallerFixture(t)
	router := NewMountRouter(map[string]Provider{
		"/guest/repo": NewRealFSProvider(dir),
	})
	s := NewVFSServer(router)

	resp := s.dispatch(&VFSRequest{Op: OpGetattr, Path: "/guest/repo/file.txt", UID: 4242, GID: 2424})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(4242), resp.Stat.UID)
	assert.Equal(t, uint32(2424), resp.Stat.GID)
}

// TestServerCallerZeroOwnerStaysZero locks the guest behavior: guest-init runs
// as root, so a 0/0 caller must still see 0/0 (unchanged from upstream).
func TestServerCallerZeroOwnerStaysZero(t *testing.T) {
	dir, _ := newCallerFixture(t)
	s := NewVFSServer(NewRealFSProvider(dir))

	resp := s.dispatch(&VFSRequest{Op: OpGetattr, Path: "file.txt", UID: 0, GID: 0})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(0), resp.Stat.UID)
	assert.Equal(t, uint32(0), resp.Stat.GID)
}

// TestServerWithOwnerOverrideBeatsCaller proves an explicit owner override
// keeps precedence over the per-request caller identity.
func TestServerWithOwnerOverrideBeatsCaller(t *testing.T) {
	dir, _ := newCallerFixture(t)
	p := NewRealFSProvider(dir).WithOwner(1000, 2000)
	s := NewVFSServer(p)

	resp := s.dispatch(&VFSRequest{Op: OpGetattr, Path: "file.txt", UID: 4242, GID: 2424})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(1000), resp.Stat.UID)
	assert.Equal(t, uint32(2000), resp.Stat.GID)
}

// TestServerWithOwnerZeroOverrideBeatsCaller proves the provider can
// deliberately pin 0/0 (the degenerate override) and the caller substitution
// must not second-guess it. A server-level "provider reported 0/0" heuristic
// could not express this, which is why the override lives on the provider.
func TestServerWithOwnerZeroOverrideBeatsCaller(t *testing.T) {
	dir, _ := newCallerFixture(t)
	p := NewRealFSProvider(dir).WithOwner(0, 0)
	s := NewVFSServer(p)

	resp := s.dispatch(&VFSRequest{Op: OpGetattr, Path: "file.txt", UID: 4242, GID: 2424})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(0), resp.Stat.UID)
	assert.Equal(t, uint32(0), resp.Stat.GID)
}

// TestServerReportsCallerOwnerForCreatedFile drives the OpCreate stat path,
// which reports ownership from the opened handle rather than a fresh Stat.
func TestServerReportsCallerOwnerForCreatedFile(t *testing.T) {
	dir, _ := newCallerFixture(t)
	s := NewVFSServer(NewRealFSProvider(dir))

	resp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "new.txt", Mode: 0644, UID: 1234, GID: 5678})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(1234), resp.Stat.UID)
	assert.Equal(t, uint32(5678), resp.Stat.GID)
}

// TestServerReportsCallerOwnerThroughReadonly proves the read-only wrapper
// forwards caller identity to the host_fs provider beneath it.
func TestServerReportsCallerOwnerThroughReadonly(t *testing.T) {
	dir, _ := newCallerFixture(t)
	s := NewVFSServer(NewReadonlyProvider(NewRealFSProvider(dir)))

	resp := s.dispatch(&VFSRequest{Op: OpGetattr, Path: "file.txt", UID: 1234, GID: 5678})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(1234), resp.Stat.UID)
	assert.Equal(t, uint32(5678), resp.Stat.GID)
}

// TestServerReportsCallerOwnerThroughInterceptor proves the hook wrapper (the
// production sandbox wiring whenever VFS hooks are configured) forwards caller
// identity to the router and host_fs provider beneath it, so the fix also works
// with interception enabled.
func TestServerReportsCallerOwnerThroughInterceptor(t *testing.T) {
	dir, _ := newCallerFixture(t)
	router := NewMountRouter(map[string]Provider{
		"/guest/repo": NewRealFSProvider(dir),
	})
	s := NewVFSServer(NewInterceptProvider(router, NewHookEngine(nil)))

	resp := s.dispatch(&VFSRequest{Op: OpGetattr, Path: "/guest/repo/file.txt", UID: 4242, GID: 2424})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	assert.Equal(t, uint32(4242), resp.Stat.UID)
	assert.Equal(t, uint32(2424), resp.Stat.GID)
}

// TestRealFSProviderWithCallerDirectly locks the provider-level behavior: an
// explicit owner wins over the caller, and a caller wins over the 0/0 default,
// without requiring a VFS server.
func TestRealFSProviderWithCallerDirectly(t *testing.T) {
	dir, _ := newCallerFixture(t)
	base := NewRealFSProvider(dir)

	caller := base.withCaller(1234, 5678).(*RealFSProvider)
	info, err := caller.Stat("file.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(1234), info.UID())
	assert.Equal(t, uint32(5678), info.GID())

	// The shared base provider is not mutated by withCaller.
	info, err = base.Stat("file.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(0), info.UID())
	assert.Equal(t, uint32(0), info.GID())

	// An explicit owner override wins over the caller identity.
	owned := NewRealFSProvider(dir).WithOwner(1000, 2000).withCaller(1234, 5678).(*RealFSProvider)
	info, err = owned.Stat("file.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(1000), info.UID())
	assert.Equal(t, uint32(2000), info.GID())
}

// TestReadonlyProviderWithCallerPropagates locks the wrapper contract directly.
func TestReadonlyProviderWithCallerPropagates(t *testing.T) {
	dir, _ := newCallerFixture(t)
	inner := NewRealFSProvider(dir)
	ro := NewReadonlyProvider(inner)

	clone := ro.withCaller(4321, 8765).(*ReadonlyProvider)
	info, err := clone.Stat("file.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(4321), info.UID())
	assert.Equal(t, uint32(8765), info.GID())

	// The original wrapper is untouched.
	info, err = ro.Stat("file.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(0), info.UID())
}

// TestMountRouterWithCallerDoesNotMutateOriginal guards against the router
// sharing caller state across requests: each dispatch must produce an isolated
// caller-aware view.
func TestMountRouterWithCallerDoesNotMutateOriginal(t *testing.T) {
	dir, _ := newCallerFixture(t)
	base := NewRealFSProvider(dir)
	router := NewMountRouter(map[string]Provider{"/guest/repo": base})

	first := router.withCaller(1111, 2222).(*MountRouter)
	info, err := first.Stat("/guest/repo/file.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(1111), info.UID())
	assert.Equal(t, uint32(2222), info.GID())

	second := router.withCaller(3333, 4444).(*MountRouter)
	info, err = second.Stat("/guest/repo/file.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(3333), info.UID())
	assert.Equal(t, uint32(4444), info.GID())

	// The base router is untouched by either clone.
	info, err = router.Stat("/guest/repo/file.txt")
	require.NoError(t, err)
	assert.Equal(t, uint32(0), info.UID())
	assert.Equal(t, uint32(0), info.GID())
}
