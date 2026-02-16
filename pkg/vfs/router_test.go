package vfs

import (
	"os"
	"path/filepath"
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
