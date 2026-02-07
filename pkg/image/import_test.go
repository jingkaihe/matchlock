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
	if err != nil {
		t.Fatalf("tarball.LayerFromOpener: %v", err)
	}

	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("mutate.AppendLayers: %v", err)
	}

	tmpTar, err := os.CreateTemp("", "test-image-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(tmpTar.Name()) })

	tag, _ := name.NewTag("test/image:latest")
	if err := tarball.Write(tag, img, tmpTar); err != nil {
		tmpTar.Close()
		t.Fatalf("tarball.Write: %v", err)
	}
	tmpTar.Close()
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
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	result, err := builder.Import(context.Background(), f, "myapp:v1")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if result.RootfsPath == "" {
		t.Fatal("RootfsPath is empty")
	}
	if !result.Cached {
		t.Error("expected Cached=true from store")
	}
	if result.Digest == "" {
		t.Error("Digest is empty")
	}
	if result.Size <= 0 {
		t.Errorf("Size = %d, want > 0", result.Size)
	}

	if _, err := os.Stat(result.RootfsPath); err != nil {
		t.Fatalf("rootfs not found: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	storeResult, err := builder.store.Get("imported:v2")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if storeResult.Digest == "" {
		t.Error("stored Digest is empty")
	}

	images, err := builder.store.List()
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("store has %d images, want 1", len(images))
	}
	if images[0].Meta.Source != "import" {
		t.Errorf("Source = %q, want %q", images[0].Meta.Source, "import")
	}
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
	if err != nil {
		t.Fatalf("Import v1: %v", err)
	}

	tarPath2 := buildTestTarball(t, map[string]string{"v2.txt": "version2"})
	f2, _ := os.Open(tarPath2)
	result2, err := builder.Import(context.Background(), f2, "app:latest")
	f2.Close()
	if err != nil {
		t.Fatalf("Import v2: %v", err)
	}

	if result1.Digest == result2.Digest {
		t.Error("expected different digests for different images")
	}

	images, _ := builder.store.List()
	if len(images) != 1 {
		t.Errorf("store has %d images, want 1 (overwritten)", len(images))
	}
}

func TestImportInvalidTarball(t *testing.T) {
	builder := NewBuilder(&BuildOptions{
		CacheDir: t.TempDir(),
	})
	builder.store = NewStore(t.TempDir())

	_, err := builder.Import(context.Background(), strings.NewReader("not a tarball"), "bad:image")
	if err == nil {
		t.Fatal("expected error for invalid tarball, got nil")
	}
}

func TestImportEmptyReader(t *testing.T) {
	builder := NewBuilder(&BuildOptions{
		CacheDir: t.TempDir(),
	})
	builder.store = NewStore(t.TempDir())

	_, err := builder.Import(context.Background(), strings.NewReader(""), "empty:image")
	if err == nil {
		t.Fatal("expected error for empty reader, got nil")
	}
}
