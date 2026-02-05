package kernel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentArch(t *testing.T) {
	arch := CurrentArch()
	if arch != ArchX86_64 && arch != ArchARM64 {
		t.Errorf("unexpected architecture: %s", arch)
	}
}

func TestKernelFilename(t *testing.T) {
	tests := []struct {
		arch     Architecture
		expected string
	}{
		{ArchX86_64, "kernel"},
		{ArchARM64, "kernel-arm64"},
	}
	for _, tt := range tests {
		if got := tt.arch.KernelFilename(); got != tt.expected {
			t.Errorf("KernelFilename(%s) = %s, want %s", tt.arch, got, tt.expected)
		}
	}
}

func TestOCIPlatform(t *testing.T) {
	tests := []struct {
		arch     Architecture
		expected string
	}{
		{ArchX86_64, "linux/amd64"},
		{ArchARM64, "linux/arm64"},
	}
	for _, tt := range tests {
		if got := tt.arch.OCIPlatform(); got != tt.expected {
			t.Errorf("OCIPlatform(%s) = %s, want %s", tt.arch, got, tt.expected)
		}
	}
}

func TestManagerKernelPath(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(WithCacheDir(tmpDir))

	path := mgr.KernelPath(ArchX86_64, "6.1.137")
	expected := filepath.Join(tmpDir, "kernels", "6.1.137", "kernel")
	if path != expected {
		t.Errorf("KernelPath() = %s, want %s", path, expected)
	}

	path = mgr.KernelPath(ArchARM64, "")
	expected = filepath.Join(tmpDir, "kernels", Version, "kernel-arm64")
	if path != expected {
		t.Errorf("KernelPath() = %s, want %s", path, expected)
	}
}

func TestListCachedVersions(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(WithCacheDir(tmpDir))

	versions, err := mgr.ListCachedVersions()
	if err != nil {
		t.Fatalf("ListCachedVersions() error: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("expected empty list, got %v", versions)
	}

	os.MkdirAll(filepath.Join(tmpDir, "kernels", "6.1.137"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "kernels", "6.1.140"), 0755)

	versions, err = mgr.ListCachedVersions()
	if err != nil {
		t.Fatalf("ListCachedVersions() error: %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("expected 2 versions, got %d", len(versions))
	}
}

func TestImageReference(t *testing.T) {
	ref := ImageReference("")
	expected := DefaultRegistry + "/kernel:" + Version
	if ref != expected {
		t.Errorf("ImageReference() = %s, want %s", ref, expected)
	}

	ref = ImageReference("6.1.140")
	expected = DefaultRegistry + "/kernel:6.1.140"
	if ref != expected {
		t.Errorf("ImageReference(6.1.140) = %s, want %s", ref, expected)
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		ref      string
		expected string
	}{
		{"ghcr.io/jingkaihe/matchlock/kernel:6.1.137", "6.1.137"},
		{"kernel:6.1.140", "6.1.140"},
		{"kernel", Version},
	}
	for _, tt := range tests {
		if got := ParseVersion(tt.ref); got != tt.expected {
			t.Errorf("ParseVersion(%s) = %s, want %s", tt.ref, got, tt.expected)
		}
	}
}
