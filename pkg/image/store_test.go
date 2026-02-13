package image

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStoreRoundTrip(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	rootfsFile := filepath.Join(t.TempDir(), "test.ext4")
	require.NoError(t, os.WriteFile(rootfsFile, []byte("fake-rootfs-content"), 0644))

	meta := ImageMeta{
		Digest:    "sha256:abc123",
		Source:    "test",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	require.NoError(t, store.Save("myapp:latest", rootfsFile, meta), "Save")

	result, err := store.Get("myapp:latest")
	require.NoError(t, err, "Get")
	assert.Equal(t, "sha256:abc123", result.Digest)
	assert.True(t, result.Cached, "expected Cached=true")

	content, err := os.ReadFile(result.RootfsPath)
	require.NoError(t, err, "read rootfs")
	assert.Equal(t, "fake-rootfs-content", string(content))
}

func TestStoreList(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	rootfsFile := filepath.Join(t.TempDir(), "test.ext4")
	require.NoError(t, os.WriteFile(rootfsFile, []byte("data"), 0644))

	store.Save("app1:v1", rootfsFile, ImageMeta{
		Digest:    "sha256:aaa",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	store.Save("app2:v2", rootfsFile, ImageMeta{
		Digest:    "sha256:bbb",
		CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})

	images, err := store.List()
	require.NoError(t, err, "List")
	require.Len(t, images, 2)

	assert.Equal(t, "app2:v2", images[0].Tag, "sorted by creation time desc")
	assert.Equal(t, "app1:v1", images[1].Tag)
}

func TestStoreRemove(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	rootfsFile := filepath.Join(t.TempDir(), "test.ext4")
	require.NoError(t, os.WriteFile(rootfsFile, []byte("data"), 0644))

	store.Save("myapp:latest", rootfsFile, ImageMeta{Digest: "sha256:abc"})

	require.NoError(t, store.Remove("myapp:latest"), "Remove")

	_, err := store.Get("myapp:latest")
	require.Error(t, err, "expected error after Remove")
}

func TestStoreRemoveNotFound(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	require.Error(t, store.Remove("nonexistent:tag"), "expected error for nonexistent tag")
}

func TestStoreGetNotFound(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	_, err := store.Get("nonexistent:tag")
	require.Error(t, err, "expected error for nonexistent tag")
}

func TestStoreListEmpty(t *testing.T) {
	storeDir := t.TempDir()
	store := NewStore(storeDir)

	images, err := store.List()
	require.NoError(t, err, "List")
	assert.Empty(t, images)
}

func TestStoreListNonexistentDir(t *testing.T) {
	store := NewStore("/nonexistent/path")
	images, err := store.List()
	require.NoError(t, err, "List")
	assert.Nil(t, images)
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
	require.NoError(t, err, "Get")
	assert.Equal(t, "sha256:v2", result.Digest)

	content, _ := os.ReadFile(result.RootfsPath)
	assert.Equal(t, "version2", string(content))
}

func TestRemoveRegistryCache(t *testing.T) {
	cacheDir := t.TempDir()

	imgDir := filepath.Join(cacheDir, "ubuntu_24.04")
	os.MkdirAll(imgDir, 0755)
	os.WriteFile(filepath.Join(imgDir, "abc123.ext4"), []byte("rootfs"), 0644)
	os.WriteFile(filepath.Join(imgDir, "metadata.json"), []byte(`{"tag":"ubuntu:24.04"}`), 0644)

	require.NoError(t, RemoveRegistryCache("ubuntu:24.04", cacheDir), "RemoveRegistryCache")

	_, err := os.Stat(imgDir)
	assert.True(t, os.IsNotExist(err), "expected directory to be removed")
}

func TestRemoveRegistryCacheNotFound(t *testing.T) {
	cacheDir := t.TempDir()
	require.Error(t, RemoveRegistryCache("nonexistent:tag", cacheDir), "expected error for nonexistent tag")
}

func TestListRegistryCacheEmpty(t *testing.T) {
	images, err := ListRegistryCache(t.TempDir())
	require.NoError(t, err, "ListRegistryCache")
	assert.Empty(t, images)
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
	require.NoError(t, err, "ListRegistryCache")
	require.Len(t, images, 1)
	assert.Equal(t, "alpine:latest", images[0].Tag)
	assert.Equal(t, "registry", images[0].Meta.Source)
	assert.Equal(t, "sha256:abc123def456", images[0].Meta.Digest)
}

func TestListRegistryCacheFallbackNoMetadata(t *testing.T) {
	cacheDir := t.TempDir()

	imgDir := filepath.Join(cacheDir, "alpine_latest")
	os.MkdirAll(imgDir, 0755)
	os.WriteFile(filepath.Join(imgDir, "abc123def456.ext4"), []byte("rootfs"), 0644)

	images, err := ListRegistryCache(cacheDir)
	require.NoError(t, err, "ListRegistryCache")
	require.Len(t, images, 1)
	assert.Equal(t, "alpine_latest", images[0].Tag, "raw dir name")
}
