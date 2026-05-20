package vfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMountRouter_BasicRouting(t *testing.T) {
	workspace := NewMemoryProvider()
	data := NewMemoryProvider()

	router := NewMountRouter(map[string]Provider{
		"/workspace": workspace,
		"/data":      data,
	})

	h1, err := router.Create("/workspace/file.txt", 0644)
	require.NoError(t, err)
	h1.Write([]byte("workspace content"))
	h1.Close()

	h2, err := router.Create("/data/file.txt", 0644)
	require.NoError(t, err)
	h2.Write([]byte("data content"))
	h2.Close()

	content1, _ := workspace.ReadFile("/file.txt")
	assert.Equal(t, "workspace content", string(content1))

	content2, _ := data.ReadFile("/file.txt")
	assert.Equal(t, "data content", string(content2))
}

func TestMountRouter_UnmountedPath(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/workspace": NewMemoryProvider(),
	})

	_, err := router.Create("/other/file.txt", 0644)
	require.Error(t, err)
}

func TestMountRouter_NestedMounts(t *testing.T) {
	parent := NewMemoryProvider()
	child := NewMemoryProvider()

	router := NewMountRouter(map[string]Provider{
		"/a":   parent,
		"/a/b": child,
	})

	h1, _ := router.Create("/a/parent.txt", 0644)
	h1.Close()

	h2, _ := router.Create("/a/b/child.txt", 0644)
	h2.Close()

	_, err := parent.Stat("/parent.txt")
	require.NoError(t, err, "parent.txt should be in parent provider")

	_, err = child.Stat("/child.txt")
	require.NoError(t, err, "child.txt should be in child provider")
}

func TestMountRouter_Stat(t *testing.T) {
	mp := NewMemoryProvider()
	mp.Mkdir("/dir", 0755)
	h, _ := mp.Create("/dir/file.txt", 0644)
	h.Write([]byte("content"))
	h.Close()

	router := NewMountRouter(map[string]Provider{
		"/mount": mp,
	})

	info, err := router.Stat("/mount/dir/file.txt")
	require.NoError(t, err)
	assert.Equal(t, int64(7), info.Size())
}

func TestMountRouter_ReadDir(t *testing.T) {
	mp := NewMemoryProvider()
	mp.Mkdir("/subdir", 0755)
	h1, _ := mp.Create("/a.txt", 0644)
	h1.Close()
	h2, _ := mp.Create("/b.txt", 0644)
	h2.Close()

	router := NewMountRouter(map[string]Provider{
		"/mount": mp,
	})

	entries, err := router.ReadDir("/mount")
	require.NoError(t, err)
	assert.Len(t, entries, 3)
}

func TestMountRouter_Open(t *testing.T) {
	mp := NewMemoryProvider()
	h, _ := mp.Create("/file.txt", 0644)
	h.Write([]byte("test data"))
	h.Close()

	router := NewMountRouter(map[string]Provider{
		"/mount": mp,
	})

	rh, err := router.Open("/mount/file.txt", os.O_RDONLY, 0)
	require.NoError(t, err)
	defer rh.Close()

	buf := make([]byte, 100)
	n, _ := rh.Read(buf)
	assert.Equal(t, "test data", string(buf[:n]))
}

func TestMountRouter_Mkdir(t *testing.T) {
	mp := NewMemoryProvider()

	router := NewMountRouter(map[string]Provider{
		"/mount": mp,
	})

	require.NoError(t, router.Mkdir("/mount/newdir", 0755))

	info, err := mp.Stat("/newdir")
	require.NoError(t, err, "Directory should exist")
	assert.True(t, info.IsDir())
}

func TestMountRouter_Remove(t *testing.T) {
	mp := NewMemoryProvider()
	h, _ := mp.Create("/file.txt", 0644)
	h.Close()

	router := NewMountRouter(map[string]Provider{
		"/mount": mp,
	})

	require.NoError(t, router.Remove("/mount/file.txt"))

	_, err := mp.Stat("/file.txt")
	require.Error(t, err, "File should not exist after remove")
}

func TestMountRouter_Rename(t *testing.T) {
	mp := NewMemoryProvider()
	mp.Mkdir("/dir", 0755)
	h, _ := mp.Create("/old.txt", 0644)
	h.Write([]byte("content"))
	h.Close()

	router := NewMountRouter(map[string]Provider{
		"/mount": mp,
	})

	require.NoError(t, router.Rename("/mount/old.txt", "/mount/new.txt"))

	_, err := mp.Stat("/old.txt")
	require.Error(t, err, "Old file should not exist")

	_, err = mp.Stat("/new.txt")
	require.NoError(t, err, "New file should exist")
}

func TestMountRouter_NotReadonly(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/mount": NewMemoryProvider(),
	})

	assert.False(t, router.Readonly())
}

func TestMountRouter_ReadDirIncludesNestedFileMount(t *testing.T) {
	hostDir := t.TempDir()
	hostFile := filepath.Join(hostDir, "hello.txt")
	require.NoError(t, os.WriteFile(hostFile, []byte("hello"), 0644), "write file")

	router := NewMountRouter(map[string]Provider{
		"/workspace":           NewMemoryProvider(),
		"/workspace/hello.txt": NewRealFSProvider(hostFile),
	})

	entries, err := router.ReadDir("/workspace")
	require.NoError(t, err, "ReadDir failed")

	var found bool
	for _, e := range entries {
		if e.Name() == "hello.txt" {
			found = true
			require.False(t, e.IsDir(), "hello.txt should be a file mount entry")
		}
	}
	require.True(t, found, "expected hello.txt in /workspace listing, got %v entries", len(entries))
}

func TestMountRouter_ReadDirIncludesNestedDirMount(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/workspace":        NewMemoryProvider(),
		"/workspace/nested": NewMemoryProvider(),
	})

	entries, err := router.ReadDir("/workspace")
	require.NoError(t, err, "ReadDir failed")

	var found bool
	for _, e := range entries {
		if e.Name() == "nested" {
			found = true
			require.True(t, e.IsDir(), "nested should be a directory mount entry")
		}
	}
	require.True(t, found, "expected nested in /workspace listing, got %v entries", len(entries))
}

func TestMountRouter_ReadDirIncludesIntermediateMountDirs(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/workspace":                 NewMemoryProvider(),
		"/workspace/.host/example":   NewMemoryProvider(),
		"/workspace/.cache/nested/x": NewMemoryProvider(),
	})

	entries, err := router.ReadDir("/workspace")
	require.NoError(t, err, "ReadDir /workspace failed")

	found := map[string]bool{}
	for _, e := range entries {
		if e.Name() == ".host" || e.Name() == ".cache" {
			found[e.Name()] = true
			require.True(t, e.IsDir(), "intermediate mount dir %q should be a directory", e.Name())
		}
	}
	require.True(t, found[".host"], "expected .host in /workspace listing")
	require.True(t, found[".cache"], "expected .cache in /workspace listing")

	hostEntries, err := router.ReadDir("/workspace/.host")
	require.NoError(t, err, "ReadDir /workspace/.host failed")
	var hostExampleFound bool
	for _, e := range hostEntries {
		if e.Name() == "example" {
			hostExampleFound = true
			require.True(t, e.IsDir(), "example should be a directory mount entry")
		}
	}
	require.True(t, hostExampleFound, "expected example in /workspace/.host listing")

	cacheEntries, err := router.ReadDir("/workspace/.cache")
	require.NoError(t, err, "ReadDir /workspace/.cache failed")
	var nestedFound bool
	for _, e := range cacheEntries {
		if e.Name() == "nested" {
			nestedFound = true
			require.True(t, e.IsDir(), "nested should be a directory mount entry")
		}
	}
	require.True(t, nestedFound, "expected nested in /workspace/.cache listing")
}

func TestMountRouter_StatIntermediateMountDir(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/workspace":               NewMemoryProvider(),
		"/workspace/.host/example": NewMemoryProvider(),
	})

	info, err := router.Stat("/workspace/.host")
	require.NoError(t, err, "Stat /workspace/.host failed")
	assert.True(t, info.IsDir(), "intermediate mount dir should be a directory")
}

func TestMountRouter_StatAncestorWithoutBaseMount(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/workspace/data": NewMemoryProvider(),
	})

	info, err := router.Stat("/workspace")
	require.NoError(t, err, "Stat /workspace failed")
	assert.True(t, info.IsDir(), "ancestor path should be synthetic directory")
}

func TestMountRouter_ReadDirAncestorWithoutBaseMount(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/workspace/data": NewMemoryProvider(),
	})

	entries, err := router.ReadDir("/workspace")
	require.NoError(t, err, "ReadDir /workspace failed")
	require.Len(t, entries, 1)
	assert.Equal(t, "data", entries[0].Name())
	assert.True(t, entries[0].IsDir())
}

// Symlink target validation lives at the router level (so cross-mount targets
// are judged against the full guest namespace, not one provider's host root).

func TestMountRouter_Symlink_RejectsAbsoluteTarget(t *testing.T) {
	dir := t.TempDir()
	router := NewMountRouter(map[string]Provider{
		"/workspace": NewRealFSProvider(dir),
	})

	err := router.Symlink("/etc/passwd", "/workspace/link")
	assert.ErrorIs(t, err, syscall.EPERM)

	_, statErr := os.Lstat(filepath.Join(dir, "link"))
	assert.True(t, os.IsNotExist(statErr), "rejected symlink must not exist on disk")
}

func TestMountRouter_Symlink_RejectsTargetEscapingAllMounts(t *testing.T) {
	dir := t.TempDir()
	router := NewMountRouter(map[string]Provider{
		"/workspace": NewRealFSProvider(dir),
	})

	err := router.Symlink("../../escape", "/workspace/link")
	assert.ErrorIs(t, err, syscall.EPERM)
}

func TestMountRouter_Symlink_AllowsRelativeTargetInsideMount(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0755))
	router := NewMountRouter(map[string]Provider{
		"/workspace": NewRealFSProvider(dir),
	})

	// Link at /workspace/a/b/c/link, target ../../d resolves to /workspace/a/d.
	require.NoError(t, router.Symlink("../../d", "/workspace/a/b/c/link"))
}

// Cross-mount sibling: link sits in a nested mount, target reaches back into
// the parent mount. The per-provider host-root check would have rejected this;
// the router-level check allows it because the resolved guest path is still
// inside a registered mount. This is the pnpm node_modules layout.
func TestMountRouter_Symlink_AllowsCrossMountSibling(t *testing.T) {
	workspaceHost := t.TempDir()
	nmHost := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(nmHost, ".pnpm", "node_modules"), 0755))

	router := NewMountRouter(map[string]Provider{
		"/home/agent/workspace":              NewRealFSProvider(workspaceHost),
		"/home/agent/workspace/node_modules": NewRealFSProvider(nmHost),
	})

	// Link: /home/agent/workspace/node_modules/.pnpm/node_modules/bar
	// Target "../../../bar" resolves to /home/agent/workspace/bar — covered
	// by the workspace mount even though it escapes the node_modules backing dir.
	err := router.Symlink("../../../bar", "/home/agent/workspace/node_modules/.pnpm/node_modules/bar")
	require.NoError(t, err)

	got, err := os.Readlink(filepath.Join(nmHost, ".pnpm", "node_modules", "bar"))
	require.NoError(t, err)
	assert.Equal(t, "../../../bar", got, "link target must be stored verbatim — guest kernel resolves it")
}

// One-too-many: the resolved guest path lands one level above the workspace
// mount root, so it is not covered by any mount and must be rejected.
func TestMountRouter_Symlink_RejectsCrossMountTargetOutsideAllMounts(t *testing.T) {
	workspaceHost := t.TempDir()
	nmHost := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(nmHost, ".pnpm", "node_modules"), 0755))

	router := NewMountRouter(map[string]Provider{
		"/home/agent/workspace":              NewRealFSProvider(workspaceHost),
		"/home/agent/workspace/node_modules": NewRealFSProvider(nmHost),
	})

	// "../../../../bar" resolves to /home/agent/bar — outside every mount.
	err := router.Symlink("../../../../bar", "/home/agent/workspace/node_modules/.pnpm/node_modules/bar")
	assert.ErrorIs(t, err, syscall.EPERM)
}

// Target equals a mount root exactly — must be allowed (boundary case of HasPrefix).
func TestMountRouter_Symlink_AllowsTargetEqualToMountRoot(t *testing.T) {
	workspaceHost := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceHost, "sub"), 0755))
	router := NewMountRouter(map[string]Provider{
		"/home/agent/workspace": NewRealFSProvider(workspaceHost),
	})

	// Link at /home/agent/workspace/sub/link, target ".." resolves to the mount root.
	require.NoError(t, router.Symlink("..", "/home/agent/workspace/sub/link"))
}
