package vfs

import (
	"io"
	"os"
	"testing"
)

func TestMemoryProvider_Basic(t *testing.T) {
	mp := NewMemoryProvider()

	if err := mp.Mkdir("/test", 0755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	info, err := mp.Stat("/test")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if !info.IsDir() {
		t.Error("Expected directory")
	}

	h, err := mp.Create("/test/file.txt", 0644)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	content := []byte("hello world")
	n, err := h.Write(content)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(content) {
		t.Errorf("Expected %d bytes written, got %d", len(content), n)
	}
	h.Close()

	h, err = mp.Open("/test/file.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	buf := make([]byte, 100)
	n, err = h.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("Read failed: %v", err)
	}
	if string(buf[:n]) != "hello world" {
		t.Errorf("Expected 'hello world', got %q", string(buf[:n]))
	}
}

func TestMemoryProvider_ReadDir(t *testing.T) {
	mp := NewMemoryProvider()

	mp.Mkdir("/dir", 0755)
	h1, _ := mp.Create("/dir/a.txt", 0644)
	h1.Close()
	h2, _ := mp.Create("/dir/b.txt", 0644)
	h2.Close()
	mp.Mkdir("/dir/subdir", 0755)

	entries, err := mp.ReadDir("/dir")
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	if len(entries) != 3 {
		t.Errorf("Expected 3 entries, got %d", len(entries))
	}

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	if !names["a.txt"] || !names["b.txt"] || !names["subdir"] {
		t.Error("Missing expected entries")
	}
}

func TestMemoryProvider_Remove(t *testing.T) {
	mp := NewMemoryProvider()

	h, _ := mp.Create("/file.txt", 0644)
	h.Write([]byte("test"))
	h.Close()

	if err := mp.Remove("/file.txt"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	_, err := mp.Stat("/file.txt")
	if err == nil {
		t.Error("Expected error for removed file")
	}
}

func TestMemoryProvider_Rename(t *testing.T) {
	mp := NewMemoryProvider()

	mp.Mkdir("/src", 0755)
	h, _ := mp.Create("/src/file.txt", 0644)
	h.Write([]byte("content"))
	h.Close()

	mp.Mkdir("/dst", 0755)

	if err := mp.Rename("/src/file.txt", "/dst/moved.txt"); err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	_, err := mp.Stat("/src/file.txt")
	if err == nil {
		t.Error("Old path should not exist")
	}

	info, err := mp.Stat("/dst/moved.txt")
	if err != nil {
		t.Fatalf("New path should exist: %v", err)
	}
	if info.Size() != 7 {
		t.Errorf("Expected size 7, got %d", info.Size())
	}
}

func TestMemoryProvider_Readonly(t *testing.T) {
	mp := NewMemoryProvider()
	if mp.Readonly() {
		t.Error("MemoryProvider should not be readonly")
	}
}

func TestMemoryProvider_WriteFile_ReadFile(t *testing.T) {
	mp := NewMemoryProvider()

	content := []byte("file content")
	if err := mp.WriteFile("/test.txt", content, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	read, err := mp.ReadFile("/test.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	if string(read) != string(content) {
		t.Errorf("Content mismatch: expected %q, got %q", content, read)
	}
}

func TestMemoryProvider_MkdirAll(t *testing.T) {
	mp := NewMemoryProvider()

	if err := mp.MkdirAll("/a/b/c/d", 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	for _, path := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d"} {
		info, err := mp.Stat(path)
		if err != nil {
			t.Errorf("Path %s should exist: %v", path, err)
		}
		if !info.IsDir() {
			t.Errorf("Path %s should be directory", path)
		}
	}
}

func TestMemoryProvider_Seek(t *testing.T) {
	mp := NewMemoryProvider()

	h, _ := mp.Create("/seek.txt", 0644)
	h.Write([]byte("0123456789"))
	h.Close()

	h, _ = mp.Open("/seek.txt", os.O_RDONLY, 0)
	defer h.Close()

	pos, err := h.Seek(5, io.SeekStart)
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	if pos != 5 {
		t.Errorf("Expected position 5, got %d", pos)
	}

	buf := make([]byte, 3)
	n, _ := h.Read(buf)
	if string(buf[:n]) != "567" {
		t.Errorf("Expected '567', got %q", string(buf[:n]))
	}
}

func TestMemoryProvider_Truncate(t *testing.T) {
	mp := NewMemoryProvider()

	h, _ := mp.Create("/trunc.txt", 0644)
	h.Write([]byte("0123456789"))

	if err := h.Truncate(5); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	info, _ := h.Stat()
	if info.Size() != 5 {
		t.Errorf("Expected size 5, got %d", info.Size())
	}
	h.Close()

	content, _ := mp.ReadFile("/trunc.txt")
	if string(content) != "01234" {
		t.Errorf("Expected '01234', got %q", string(content))
	}
}
