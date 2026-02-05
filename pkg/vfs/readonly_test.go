package vfs

import (
	"os"
	"testing"
)

func TestReadonlyProvider_Read(t *testing.T) {
	base := NewMemoryProvider()
	h, _ := base.Create("/file.txt", 0644)
	h.Write([]byte("content"))
	h.Close()

	ro := NewReadonlyProvider(base)

	rh, err := ro.Open("/file.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer rh.Close()

	buf := make([]byte, 100)
	n, _ := rh.Read(buf)
	if string(buf[:n]) != "content" {
		t.Errorf("Expected 'content', got %q", string(buf[:n]))
	}
}

func TestReadonlyProvider_WriteBlocked(t *testing.T) {
	base := NewMemoryProvider()
	ro := NewReadonlyProvider(base)

	_, err := ro.Create("/newfile.txt", 0644)
	if err == nil {
		t.Error("Create should fail on readonly provider")
	}

	err = ro.Mkdir("/newdir", 0755)
	if err == nil {
		t.Error("Mkdir should fail on readonly provider")
	}

	err = ro.Remove("/anything")
	if err == nil {
		t.Error("Remove should fail on readonly provider")
	}

	err = ro.Rename("/old", "/new")
	if err == nil {
		t.Error("Rename should fail on readonly provider")
	}
}

func TestReadonlyProvider_Readonly(t *testing.T) {
	base := NewMemoryProvider()
	ro := NewReadonlyProvider(base)

	if !ro.Readonly() {
		t.Error("ReadonlyProvider should be readonly")
	}
}

func TestReadonlyProvider_Stat(t *testing.T) {
	base := NewMemoryProvider()
	base.Mkdir("/dir", 0755)

	ro := NewReadonlyProvider(base)

	info, err := ro.Stat("/dir")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if !info.IsDir() {
		t.Error("Expected directory")
	}
}

func TestReadonlyProvider_ReadDir(t *testing.T) {
	base := NewMemoryProvider()
	base.Mkdir("/dir", 0755)
	h, _ := base.Create("/dir/a.txt", 0644)
	h.Close()

	ro := NewReadonlyProvider(base)

	entries, err := ro.ReadDir("/dir")
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("Expected 1 entry, got %d", len(entries))
	}
}
