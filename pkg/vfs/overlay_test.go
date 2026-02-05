package vfs

import (
	"os"
	"testing"
)

func TestOverlayProvider_ReadFromLower(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	h, _ := lower.Create("/file.txt", 0644)
	h.Write([]byte("lower content"))
	h.Close()

	overlay := NewOverlayProvider(upper, lower)

	h, err := overlay.Open("/file.txt", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer h.Close()

	buf := make([]byte, 100)
	n, _ := h.Read(buf)
	if string(buf[:n]) != "lower content" {
		t.Errorf("Expected 'lower content', got %q", string(buf[:n]))
	}
}

func TestOverlayProvider_WriteToUpper(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	overlay := NewOverlayProvider(upper, lower)

	h, err := overlay.Create("/new.txt", 0644)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	h.Write([]byte("upper content"))
	h.Close()

	_, err = upper.Stat("/new.txt")
	if err != nil {
		t.Error("File should exist in upper layer")
	}

	_, err = lower.Stat("/new.txt")
	if err == nil {
		t.Error("File should not exist in lower layer")
	}
}

func TestOverlayProvider_UpperShadowsLower(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	h1, _ := lower.Create("/shadow.txt", 0644)
	h1.Write([]byte("lower"))
	h1.Close()

	h2, _ := upper.Create("/shadow.txt", 0644)
	h2.Write([]byte("upper"))
	h2.Close()

	overlay := NewOverlayProvider(upper, lower)

	h, _ := overlay.Open("/shadow.txt", os.O_RDONLY, 0)
	defer h.Close()

	buf := make([]byte, 100)
	n, _ := h.Read(buf)
	if string(buf[:n]) != "upper" {
		t.Errorf("Expected 'upper', got %q", string(buf[:n]))
	}
}

func TestOverlayProvider_ReadDirMerged(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	lower.Mkdir("/dir", 0755)
	h1, _ := lower.Create("/dir/lower.txt", 0644)
	h1.Close()

	upper.Mkdir("/dir", 0755)
	h2, _ := upper.Create("/dir/upper.txt", 0644)
	h2.Close()

	overlay := NewOverlayProvider(upper, lower)

	entries, err := overlay.ReadDir("/dir")
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	if !names["lower.txt"] || !names["upper.txt"] {
		t.Error("Should see files from both layers")
	}
}

func TestOverlayProvider_Mkdir(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()

	overlay := NewOverlayProvider(upper, lower)

	if err := overlay.Mkdir("/newdir", 0755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	_, err := upper.Stat("/newdir")
	if err != nil {
		t.Error("Directory should exist in upper layer")
	}
}

func TestOverlayProvider_NotReadonly(t *testing.T) {
	lower := NewMemoryProvider()
	upper := NewMemoryProvider()
	overlay := NewOverlayProvider(upper, lower)

	if overlay.Readonly() {
		t.Error("OverlayProvider should not be readonly")
	}
}
