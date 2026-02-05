package vfs

import (
	"os"
	"testing"
)

func TestMountRouter_BasicRouting(t *testing.T) {
	workspace := NewMemoryProvider()
	data := NewMemoryProvider()

	router := NewMountRouter(map[string]Provider{
		"/workspace": workspace,
		"/data":      data,
	})

	h1, err := router.Create("/workspace/file.txt", 0644)
	if err != nil {
		t.Fatalf("Create in workspace failed: %v", err)
	}
	h1.Write([]byte("workspace content"))
	h1.Close()

	h2, err := router.Create("/data/file.txt", 0644)
	if err != nil {
		t.Fatalf("Create in data failed: %v", err)
	}
	h2.Write([]byte("data content"))
	h2.Close()

	content1, _ := workspace.ReadFile("/file.txt")
	if string(content1) != "workspace content" {
		t.Errorf("Expected 'workspace content' in workspace provider")
	}

	content2, _ := data.ReadFile("/file.txt")
	if string(content2) != "data content" {
		t.Errorf("Expected 'data content' in data provider")
	}
}

func TestMountRouter_UnmountedPath(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/workspace": NewMemoryProvider(),
	})

	_, err := router.Create("/other/file.txt", 0644)
	if err == nil {
		t.Error("Create on unmounted path should fail")
	}
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
	if err != nil {
		t.Error("parent.txt should be in parent provider")
	}

	_, err = child.Stat("/child.txt")
	if err != nil {
		t.Error("child.txt should be in child provider")
	}
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
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != 7 {
		t.Errorf("Expected size 7, got %d", info.Size())
	}
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
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("Expected 3 entries, got %d", len(entries))
	}
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
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer rh.Close()

	buf := make([]byte, 100)
	n, _ := rh.Read(buf)
	if string(buf[:n]) != "test data" {
		t.Errorf("Expected 'test data', got %q", string(buf[:n]))
	}
}

func TestMountRouter_Mkdir(t *testing.T) {
	mp := NewMemoryProvider()

	router := NewMountRouter(map[string]Provider{
		"/mount": mp,
	})

	if err := router.Mkdir("/mount/newdir", 0755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	info, err := mp.Stat("/newdir")
	if err != nil {
		t.Fatalf("Directory should exist: %v", err)
	}
	if !info.IsDir() {
		t.Error("Should be a directory")
	}
}

func TestMountRouter_Remove(t *testing.T) {
	mp := NewMemoryProvider()
	h, _ := mp.Create("/file.txt", 0644)
	h.Close()

	router := NewMountRouter(map[string]Provider{
		"/mount": mp,
	})

	if err := router.Remove("/mount/file.txt"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	_, err := mp.Stat("/file.txt")
	if err == nil {
		t.Error("File should not exist after remove")
	}
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

	if err := router.Rename("/mount/old.txt", "/mount/new.txt"); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	_, err := mp.Stat("/old.txt")
	if err == nil {
		t.Error("Old file should not exist")
	}

	_, err = mp.Stat("/new.txt")
	if err != nil {
		t.Error("New file should exist")
	}
}

func TestMountRouter_NotReadonly(t *testing.T) {
	router := NewMountRouter(map[string]Provider{
		"/mount": NewMemoryProvider(),
	})

	if router.Readonly() {
		t.Error("MountRouter should not be readonly")
	}
}
