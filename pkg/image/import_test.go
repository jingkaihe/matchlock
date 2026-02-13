package image

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTestTarball creates a Docker-save-format tarball containing a single-layer
// image with the given files. Each file is a path→content pair.
func buildTestTarball(t *testing.T, files map[string]string) string {
	t.Helper()

	layerBuf := new(bytes.Buffer)
	tw := tar.NewWriter(layerBuf)
	for path, content := range files {
		dir := parentDir(path)
		if dir != "" && dir != "." {
			for _, d := range splitDirs(dir) {
				tw.WriteHeader(&tar.Header{
					Typeflag: tar.TypeDir,
					Name:     d + "/",
					Mode:     0755,
				})
			}
		}
		tw.WriteHeader(&tar.Header{
			Name: path,
			Mode: 0644,
			Size: int64(len(content)),
		})
		tw.Write([]byte(content))
	}
	tw.Close()

	layerData := layerBuf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerData)), nil
	})
	require.NoError(t, err, "tarball.LayerFromOpener")

	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err, "mutate.AppendLayers")

	tmpTar, err := os.CreateTemp("", "test-image-*.tar")
	require.NoError(t, err)
	t.Cleanup(func() { os.Remove(tmpTar.Name()) })

	tag, _ := name.NewTag("test/image:latest")
	err = tarball.Write(tag, img, tmpTar)
	tmpTar.Close()
	require.NoError(t, err, "tarball.Write")
	return tmpTar.Name()
}

func parentDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return ""
	}
	return path[:idx]
}

func splitDirs(dir string) []string {
	parts := strings.Split(dir, "/")
	var result []string
	for i := range parts {
		result = append(result, strings.Join(parts[:i+1], "/"))
	}
	return result
}

func TestImportRoundTrip(t *testing.T) {
	tarPath := buildTestTarball(t, map[string]string{
		"hello.txt": "hello from import test",
	})

	storeDir := t.TempDir()
	builder := NewBuilder(&BuildOptions{
		CacheDir: t.TempDir(),
	})
	builder.store = NewStore(storeDir)

	f, err := os.Open(tarPath)
	require.NoError(t, err)
	defer f.Close()

	result, err := builder.Import(context.Background(), f, "myapp:v1")
	require.NoError(t, err, "Import")

	require.NotEmpty(t, result.RootfsPath)
	assert.True(t, result.Cached, "expected Cached=true from store")
	assert.NotEmpty(t, result.Digest)
	assert.Greater(t, result.Size, int64(0))

	_, err = os.Stat(result.RootfsPath)
	require.NoError(t, err, "rootfs not found")
}

func TestImportStoresMetadata(t *testing.T) {
	tarPath := buildTestTarball(t, map[string]string{
		"data.txt": "some data",
	})

	storeDir := t.TempDir()
	builder := NewBuilder(&BuildOptions{
		CacheDir: t.TempDir(),
	})
	builder.store = NewStore(storeDir)

	f, _ := os.Open(tarPath)
	defer f.Close()

	_, err := builder.Import(context.Background(), f, "imported:v2")
	require.NoError(t, err, "Import")

	storeResult, err := builder.store.Get("imported:v2")
	require.NoError(t, err, "store.Get")
	assert.NotEmpty(t, storeResult.Digest)

	images, err := builder.store.List()
	require.NoError(t, err, "store.List")
	require.Len(t, images, 1)
	assert.Equal(t, "import", images[0].Meta.Source)
}

func TestImportOverwritesExisting(t *testing.T) {
	storeDir := t.TempDir()
	builder := NewBuilder(&BuildOptions{
		CacheDir: t.TempDir(),
	})
	builder.store = NewStore(storeDir)

	tarPath1 := buildTestTarball(t, map[string]string{"v1.txt": "version1"})
	f1, _ := os.Open(tarPath1)
	result1, err := builder.Import(context.Background(), f1, "app:latest")
	f1.Close()
	require.NoError(t, err, "Import v1")

	tarPath2 := buildTestTarball(t, map[string]string{"v2.txt": "version2"})
	f2, _ := os.Open(tarPath2)
	result2, err := builder.Import(context.Background(), f2, "app:latest")
	f2.Close()
	require.NoError(t, err, "Import v2")

	assert.NotEqual(t, result1.Digest, result2.Digest, "expected different digests for different images")

	images, _ := builder.store.List()
	assert.Len(t, images, 1, "store should have 1 image (overwritten)")
}

func TestImportInvalidTarball(t *testing.T) {
	builder := NewBuilder(&BuildOptions{
		CacheDir: t.TempDir(),
	})
	builder.store = NewStore(t.TempDir())

	_, err := builder.Import(context.Background(), strings.NewReader("not a tarball"), "bad:image")
	require.Error(t, err, "expected error for invalid tarball")
}

func TestImportEmptyReader(t *testing.T) {
	builder := NewBuilder(&BuildOptions{
		CacheDir: t.TempDir(),
	})
	builder.store = NewStore(t.TempDir())

	_, err := builder.Import(context.Background(), strings.NewReader(""), "empty:image")
	require.Error(t, err, "expected error for empty reader")
}
