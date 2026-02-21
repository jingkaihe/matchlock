package image

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	cacheRoot := t.TempDir()
	localDir := filepath.Join(cacheRoot, "local")
	store := NewStore(localDir)
	require.NoError(t, store.ready())
	return store, cacheRoot
}

func writeTestLayer(t *testing.T, cacheRoot, digest, content string) LayerRef {
	t.Helper()
	path := blobPathForDigest(cacheRoot, digest)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return LayerRef{Digest: digest, Path: path, Size: fi.Size()}
}

func TestStoreRoundTrip(t *testing.T) {
	store, cacheRoot := newTestStore(t)
	layer := writeTestLayer(t, cacheRoot, "sha256:abc123", "fake-rootfs-content")

	meta := ImageMeta{
		Digest:    "sha256:image",
		Source:    "test",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	require.NoError(t, store.Save("myapp:latest", []LayerRef{layer}, meta))

	result, err := store.Get("myapp:latest")
	require.NoError(t, err)
	assert.Equal(t, "sha256:image", result.Digest)
	assert.True(t, result.Cached)
	require.Len(t, result.LowerPaths, 1)
	assert.Equal(t, layer.Path, result.LowerPaths[0])
}

func TestStoreList(t *testing.T) {
	store, cacheRoot := newTestStore(t)
	layer := writeTestLayer(t, cacheRoot, "sha256:base", "data")

	require.NoError(t, store.Save("app1:v1", []LayerRef{layer}, ImageMeta{
		Digest:    "sha256:aaa",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}))
	require.NoError(t, store.Save("app2:v2", []LayerRef{layer}, ImageMeta{
		Digest:    "sha256:bbb",
		CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}))

	images, err := store.List()
	require.NoError(t, err)
	require.Len(t, images, 2)
	assert.Equal(t, "app2:v2", images[0].Tag)
	assert.Equal(t, "app1:v1", images[1].Tag)
}

func TestStoreRemove(t *testing.T) {
	store, cacheRoot := newTestStore(t)
	layer := writeTestLayer(t, cacheRoot, "sha256:abc", "data")
	require.NoError(t, store.Save("myapp:latest", []LayerRef{layer}, ImageMeta{Digest: "sha256:image"}))

	require.NoError(t, store.Remove("myapp:latest"))
	_, err := store.Get("myapp:latest")
	require.Error(t, err)
	_, err = os.Stat(layer.Path)
	assert.True(t, os.IsNotExist(err), "unreferenced blob should be pruned")
}

func TestStoreRemoveSharedLayer(t *testing.T) {
	store, cacheRoot := newTestStore(t)
	layer := writeTestLayer(t, cacheRoot, "sha256:shared", "data")
	require.NoError(t, store.Save("app1:latest", []LayerRef{layer}, ImageMeta{Digest: "sha256:image1"}))
	require.NoError(t, store.Save("app2:latest", []LayerRef{layer}, ImageMeta{Digest: "sha256:image2"}))

	require.NoError(t, store.Remove("app1:latest"))
	_, err := os.Stat(layer.Path)
	require.NoError(t, err, "shared blob must remain referenced")

	result, err := store.Get("app2:latest")
	require.NoError(t, err)
	require.Len(t, result.LowerPaths, 1)
	assert.Equal(t, layer.Path, result.LowerPaths[0])
}

func TestStoreRemoveNotFound(t *testing.T) {
	store, _ := newTestStore(t)
	require.Error(t, store.Remove("nonexistent:tag"))
}

func TestStoreGetNotFound(t *testing.T) {
	store, _ := newTestStore(t)
	_, err := store.Get("nonexistent:tag")
	require.Error(t, err)
}

func TestStoreListEmpty(t *testing.T) {
	store, _ := newTestStore(t)

	images, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, images)
}

func TestStoreListNonexistentDir(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "does-not-exist", "local")
	store := NewStore(storeDir)
	images, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, images)
}

func TestStoreOverwrite(t *testing.T) {
	store, cacheRoot := newTestStore(t)
	layer1 := writeTestLayer(t, cacheRoot, "sha256:v1", "version1")
	layer2 := writeTestLayer(t, cacheRoot, "sha256:v2", "version2")

	require.NoError(t, store.Save("myapp:latest", []LayerRef{layer1}, ImageMeta{Digest: "sha256:imagev1"}))
	require.NoError(t, store.Save("myapp:latest", []LayerRef{layer2}, ImageMeta{Digest: "sha256:imagev2"}))

	result, err := store.Get("myapp:latest")
	require.NoError(t, err)
	assert.Equal(t, "sha256:imagev2", result.Digest)
	require.Len(t, result.LowerPaths, 1)
	assert.Equal(t, layer2.Path, result.LowerPaths[0])
}

func TestRemoveRegistryCache(t *testing.T) {
	cacheDir := t.TempDir()
	layer := writeTestLayer(t, cacheDir, "sha256:abc123", "rootfs")
	require.NoError(t, SaveRegistryCache("ubuntu:24.04", cacheDir, []LayerRef{layer}, ImageMeta{
		Digest:    "sha256:image",
		Source:    "registry",
		CreatedAt: time.Now().UTC(),
	}))

	require.NoError(t, RemoveRegistryCache("ubuntu:24.04", cacheDir))
	_, err := os.Stat(layer.Path)
	assert.True(t, os.IsNotExist(err))
}

func TestRemoveRegistryCacheNotFound(t *testing.T) {
	cacheDir := t.TempDir()
	require.Error(t, RemoveRegistryCache("nonexistent:tag", cacheDir))
}

func TestListRegistryCacheEmpty(t *testing.T) {
	images, err := ListRegistryCache(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, images)
}

func TestListRegistryCacheWithMetadata(t *testing.T) {
	cacheDir := t.TempDir()
	layer := writeTestLayer(t, cacheDir, "sha256:abc123def456", "rootfs")
	require.NoError(t, SaveRegistryCache("alpine:latest", cacheDir, []LayerRef{layer}, ImageMeta{
		Digest:    "sha256:abc123def456",
		Source:    "registry",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}))

	images, err := ListRegistryCache(cacheDir)
	require.NoError(t, err)
	require.Len(t, images, 1)
	assert.Equal(t, "alpine:latest", images[0].Tag)
	assert.Equal(t, "registry", images[0].Meta.Source)
	assert.Equal(t, "sha256:abc123def456", images[0].Meta.Digest)
}

func TestGetRegistryCache(t *testing.T) {
	cacheDir := t.TempDir()
	layerA := writeTestLayer(t, cacheDir, "sha256:a", "a")
	layerB := writeTestLayer(t, cacheDir, "sha256:b", "b")
	require.NoError(t, SaveRegistryCache("python:3.12", cacheDir, []LayerRef{layerA, layerB}, ImageMeta{
		Digest:    "sha256:image",
		Source:    "registry",
		CreatedAt: time.Now().UTC(),
	}))

	result, err := GetRegistryCache("python:3.12", cacheDir)
	require.NoError(t, err)
	require.Len(t, result.LowerPaths, 2)
	assert.Equal(t, []string{layerA.Path, layerB.Path}, result.LowerPaths)
	assert.Equal(t, "sha256:image", result.Digest)
}
