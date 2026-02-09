package image

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	rootfsFile := filepath.Join(t.TempDir(), "test.ext4")
	if err := os.WriteFile(rootfsFile, []byte("fake-rootfs-content"), 0644); err != nil {
		t.Fatal(err)
	}

	meta := ImageMeta{
		Digest:    "sha256:abc123",
		Source:    "test",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	if err := store.Save("myapp:latest", rootfsFile, meta); err != nil {
		t.Fatalf("Save: %v", err)
	}

	result, err := store.Get("myapp:latest")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if result.Digest != "sha256:abc123" {
		t.Errorf("Digest = %q, want %q", result.Digest, "sha256:abc123")
	}
	if !result.Cached {
		t.Error("expected Cached=true")
	}

	content, err := os.ReadFile(result.RootfsPath)
	if err != nil {
		t.Fatalf("read rootfs: %v", err)
	}
	if string(content) != "fake-rootfs-content" {
		t.Errorf("rootfs content = %q, want %q", string(content), "fake-rootfs-content")
	}
}

func TestStoreList(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	rootfsFile := filepath.Join(t.TempDir(), "test.ext4")
	if err := os.WriteFile(rootfsFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	store.Save("app1:v1", rootfsFile, ImageMeta{
		Digest:    "sha256:aaa",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	store.Save("app2:v2", rootfsFile, ImageMeta{
		Digest:    "sha256:bbb",
		CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})

	images, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("len = %d, want 2", len(images))
	}

	if images[0].Tag != "app2:v2" {
		t.Errorf("first image = %q, want %q (sorted by creation time desc)", images[0].Tag, "app2:v2")
	}
	if images[1].Tag != "app1:v1" {
		t.Errorf("second image = %q, want %q", images[1].Tag, "app1:v1")
	}
}

func TestStoreRemove(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	rootfsFile := filepath.Join(t.TempDir(), "test.ext4")
	if err := os.WriteFile(rootfsFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	store.Save("myapp:latest", rootfsFile, ImageMeta{Digest: "sha256:abc"})

	if err := store.Remove("myapp:latest"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, err := store.Get("myapp:latest"); err == nil {
		t.Error("expected error after Remove, got nil")
	}
}

func TestStoreRemoveNotFound(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	if err := store.Remove("nonexistent:tag"); err == nil {
		t.Error("expected error for nonexistent tag")
	}
}

func TestStoreGetNotFound(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	if _, err := store.Get("nonexistent:tag"); err == nil {
		t.Error("expected error for nonexistent tag")
	}
}

func TestStoreListEmpty(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	images, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("len = %d, want 0", len(images))
	}
}

func TestStoreListNonexistentDir(t *testing.T) {
	store := NewStore("/nonexistent/path")
	images, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if images != nil {
		t.Errorf("expected nil, got %v", images)
	}
}

func TestStoreOverwrite(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	rootfsFile1 := filepath.Join(t.TempDir(), "test1.ext4")
	os.WriteFile(rootfsFile1, []byte("version1"), 0644)

	rootfsFile2 := filepath.Join(t.TempDir(), "test2.ext4")
	os.WriteFile(rootfsFile2, []byte("version2"), 0644)

	store.Save("myapp:latest", rootfsFile1, ImageMeta{Digest: "sha256:v1"})
	store.Save("myapp:latest", rootfsFile2, ImageMeta{Digest: "sha256:v2"})

	result, err := store.Get("myapp:latest")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if result.Digest != "sha256:v2" {
		t.Errorf("Digest = %q, want %q", result.Digest, "sha256:v2")
	}

	content, _ := os.ReadFile(result.RootfsPath)
	if string(content) != "version2" {
		t.Errorf("content = %q, want %q", string(content), "version2")
	}
}

func TestRemoveRegistryCache(t *testing.T) {
	cacheDir := t.TempDir()

	imgDir := filepath.Join(cacheDir, "ubuntu_24.04")
	os.MkdirAll(imgDir, 0755)
	os.WriteFile(filepath.Join(imgDir, "abc123.ext4"), []byte("rootfs"), 0644)
	os.WriteFile(filepath.Join(imgDir, "metadata.json"), []byte(`{"tag":"ubuntu:24.04"}`), 0644)

	if err := RemoveRegistryCache("ubuntu:24.04", cacheDir); err != nil {
		t.Fatalf("RemoveRegistryCache: %v", err)
	}

	if _, err := os.Stat(imgDir); !os.IsNotExist(err) {
		t.Error("expected directory to be removed")
	}
}

func TestRemoveRegistryCacheNotFound(t *testing.T) {
	cacheDir := t.TempDir()
	if err := RemoveRegistryCache("nonexistent:tag", cacheDir); err == nil {
		t.Error("expected error for nonexistent tag")
	}
}

func TestListRegistryCacheEmpty(t *testing.T) {
	images, err := ListRegistryCache(t.TempDir())
	if err != nil {
		t.Fatalf("ListRegistryCache: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("len = %d, want 0", len(images))
	}
}

func TestListRegistryCacheWithMetadata(t *testing.T) {
	cacheDir := t.TempDir()

	imgDir := filepath.Join(cacheDir, "alpine_latest")
	os.MkdirAll(imgDir, 0755)
	os.WriteFile(filepath.Join(imgDir, "abc123def456.ext4"), []byte("rootfs"), 0644)
	meta := `{"tag":"alpine:latest","digest":"sha256:abc123def456","size":6,"created_at":"2026-01-01T00:00:00Z","source":"registry"}`
	os.WriteFile(filepath.Join(imgDir, "metadata.json"), []byte(meta), 0644)

	localDir := filepath.Join(cacheDir, "local")
	os.MkdirAll(localDir, 0755)

	images, err := ListRegistryCache(cacheDir)
	if err != nil {
		t.Fatalf("ListRegistryCache: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("len = %d, want 1", len(images))
	}
	if images[0].Tag != "alpine:latest" {
		t.Errorf("Tag = %q, want %q", images[0].Tag, "alpine:latest")
	}
	if images[0].Meta.Source != "registry" {
		t.Errorf("Source = %q, want %q", images[0].Meta.Source, "registry")
	}
	if images[0].Meta.Digest != "sha256:abc123def456" {
		t.Errorf("Digest = %q, want %q", images[0].Meta.Digest, "sha256:abc123def456")
	}
}

func TestListRegistryCacheFallbackNoMetadata(t *testing.T) {
	cacheDir := t.TempDir()

	imgDir := filepath.Join(cacheDir, "alpine_latest")
	os.MkdirAll(imgDir, 0755)
	os.WriteFile(filepath.Join(imgDir, "abc123def456.ext4"), []byte("rootfs"), 0644)

	images, err := ListRegistryCache(cacheDir)
	if err != nil {
		t.Fatalf("ListRegistryCache: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("len = %d, want 1", len(images))
	}
	if images[0].Tag != "alpine_latest" {
		t.Errorf("Tag = %q, want %q (raw dir name)", images[0].Tag, "alpine_latest")
	}
}
