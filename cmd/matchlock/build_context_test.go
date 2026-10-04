package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteBuildContext(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch\nCOPY . /\n"), 0644))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "bin"), 0755))
	payload := bytes.Repeat([]byte{0, 1, 255, '\n'}, 20000)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin", "tool"), payload, 0755))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "empty"), 0750))
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("not uploaded"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "outside")))
	require.NoError(t, os.Symlink("bin/tool", filepath.Join(dir, "tool")))

	var archive bytes.Buffer
	require.NoError(t, writeBuildContext(context.Background(), &archive, dir, dockerfile))
	headers, files := readContextArchive(t, &archive)
	assert.Equal(t, payload, files["context/bin/tool"])
	assert.Equal(t, int64(0755), headers["context/bin/tool"].Mode)
	assert.Equal(t, byte(tar.TypeDir), headers["context/empty"].Typeflag)
	assert.Equal(t, byte(tar.TypeSymlink), headers["context/outside"].Typeflag)
	assert.Equal(t, outside, headers["context/outside"].Linkname)
	assert.Equal(t, "bin/tool", headers["context/tool"].Linkname)
	assert.NotContains(t, headers, "context/outside/secret")
	assert.Equal(t, "FROM scratch\nCOPY . /\n", string(files["dockerfile/Dockerfile"]))
}

func TestWriteBuildContextSkipsSockets(t *testing.T) {
	// Keep the absolute socket path below the Unix socket limit on macOS too.
	dir, err := os.MkdirTemp("", "build-context-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dockerfile := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch\nCOPY . /\n"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".runtime", "sockets"), 0755))
	for _, name := range []string{"active", "stale.sock"} {
		listener, err := net.ListenUnix("unix", &net.UnixAddr{
			Net: "unix", Name: filepath.Join(dir, ".runtime", "sockets", name),
		})
		require.NoError(t, err)
		listener.SetUnlinkOnClose(false)
		t.Cleanup(func() { _ = listener.Close() })
		if name == "stale.sock" {
			require.NoError(t, listener.Close())
		}
	}
	// A .sock filename does not imply a socket: regular files must still upload.
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".runtime", "sockets", "data.sock"), []byte("keep"), 0644))
	var archive bytes.Buffer
	require.NoError(t, writeBuildContext(context.Background(), &archive, dir, dockerfile))
	headers, files := readContextArchive(t, &archive)
	assert.NotContains(t, headers, "context/.runtime/sockets/active")
	assert.NotContains(t, headers, "context/.runtime/sockets/stale.sock")
	assert.Equal(t, "keep", string(files["context/.runtime/sockets/data.sock"]))
	assert.Equal(t, "FROM scratch\nCOPY . /\n", string(files["dockerfile/Dockerfile"]))
}

func TestWriteBuildContextIgnores(t *testing.T) {
	for _, specific := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "Dockerfile-specific"}[specific], func(t *testing.T) {
			dir := t.TempDir()
			dockerfile := filepath.Join(t.TempDir(), "Custom Dockerfile")
			require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0644))
			// A different file with the same name in the context must not replace -f.
			require.NoError(t, os.WriteFile(filepath.Join(dir, filepath.Base(dockerfile)), []byte("wrong Dockerfile"), 0644))
			require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0755))
			for _, name := range []string{"keep.txt", "secret.txt", "nested/keep.txt", "nested/secret.txt"} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name), 0644))
			}
			rules := "# comment\n**/secret.txt\nnested\n!nested/keep.txt\nCustom Dockerfile\n"
			if specific {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("*\n"), 0644))
				require.NoError(t, os.WriteFile(dockerfile+".dockerignore", []byte(rules), 0644))
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte(rules), 0644))
			}
			var archive bytes.Buffer
			require.NoError(t, writeBuildContext(context.Background(), &archive, dir, dockerfile))
			headers, files := readContextArchive(t, &archive)
			assert.Contains(t, headers, "context/keep.txt")
			assert.Contains(t, headers, "context/nested/keep.txt")
			assert.NotContains(t, headers, "context/secret.txt")
			assert.NotContains(t, headers, "context/nested/secret.txt")
			assert.NotContains(t, headers, "context/Custom Dockerfile")
			assert.Equal(t, "FROM scratch\n", string(files["dockerfile/Dockerfile"]))
			assert.Equal(t, rules, string(files["dockerfile/Dockerfile.dockerignore"]))
		})
	}
}

func TestWriteBuildContextErrors(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0644))
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.ErrorIs(t, writeBuildContext(ctx, io.Discard, dir, dockerfile), context.Canceled)
	})
	t.Run("write error", func(t *testing.T) {
		err := errors.New("transfer failed")
		assert.ErrorIs(t, writeBuildContext(context.Background(), failingContextWriter{err}, dir, dockerfile), err)
	})
	t.Run("missing Dockerfile", func(t *testing.T) {
		assert.ErrorIs(t, writeBuildContext(context.Background(), io.Discard, dir, dockerfile+"missing"), os.ErrNotExist)
	})
	t.Run("invalid ignore pattern", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("["), 0644))
		assert.Error(t, writeBuildContext(context.Background(), io.Discard, dir, dockerfile))
	})
}

func TestWriteBuildContextRejectsSpecialFiles(t *testing.T) {
	for _, name := range []string{"input.pipe", "Dockerfile", ".dockerignore"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dockerfile := filepath.Join(dir, "Dockerfile")
			if name != "Dockerfile" {
				require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch"), 0644))
			}
			require.NoError(t, syscall.Mkfifo(filepath.Join(dir, name), 0600))
			assert.ErrorIs(t, writeBuildContext(context.Background(), io.Discard, dir, dockerfile), ErrBuildContext)
		})
	}
}

func TestWriteBuildContextFollowsSelectedDockerfileSymlink(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(t.TempDir(), "Dockerfile.shared")
	require.NoError(t, os.WriteFile(shared, []byte("FROM busybox\n"), 0644))
	dockerfile := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.Symlink(shared, dockerfile))

	var archive bytes.Buffer
	require.NoError(t, writeBuildContext(context.Background(), &archive, dir, dockerfile))
	headers, files := readContextArchive(t, &archive)
	assert.Equal(t, "FROM busybox\n", string(files["dockerfile/Dockerfile"]))
	assert.Equal(t, byte(tar.TypeSymlink), headers["context/Dockerfile"].Typeflag, "context symlinks stay links")
}

func TestWriteBuildContextRejectsEscapingIgnoreSymlink(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch"), 0644))
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("host secret"), 0600))
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, ".dockerignore")))
	var archive bytes.Buffer
	require.Error(t, writeBuildContext(context.Background(), &archive, dir, dockerfile))
	assert.NotContains(t, archive.String(), "host secret")
}

func TestWriteBuildContextPrunesUnrelatedIgnoredDirectory(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("keep"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("private-cache/\n!README.md\n"), 0644))
	private := filepath.Join(dir, "private-cache")
	require.NoError(t, os.Mkdir(private, 0000))
	t.Cleanup(func() { _ = os.Chmod(private, 0700) })
	var archive bytes.Buffer
	require.NoError(t, writeBuildContext(context.Background(), &archive, dir, dockerfile))
	headers, _ := readContextArchive(t, &archive)
	assert.NotContains(t, headers, "context/private-cache")
	assert.Contains(t, headers, "context/README.md")
}

func TestWriteBuildContextNegatedGlob(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "nested", "child"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nested", "child", "keep.txt"), []byte("keep"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("nested\n!**/keep.txt\n"), 0644))
	var archive bytes.Buffer
	require.NoError(t, writeBuildContext(context.Background(), &archive, dir, dockerfile))
	headers, _ := readContextArchive(t, &archive)
	assert.Contains(t, headers, "context/nested/child/keep.txt")
}

func TestWriteBuildContextBatchesSmallWrites(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.WriteFile(dockerfile, []byte("FROM scratch"), 0644))
	for i := 0; i < 500; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), []byte("x"), 0644))
	}
	var w countingContextWriter
	require.NoError(t, writeBuildContext(context.Background(), &w, dir, dockerfile))
	// Unbuffered, each file costs a header, data, and padding write.
	assert.Less(t, w.writes, 10)
	headers, _ := readContextArchive(t, &w.buf)
	assert.Contains(t, headers, "context/f499.txt")
}

type countingContextWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *countingContextWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

type failingContextWriter struct{ err error }

func (w failingContextWriter) Write([]byte) (int, error) { return 0, w.err }

func readContextArchive(t *testing.T, r io.Reader) (map[string]*tar.Header, map[string][]byte) {
	t.Helper()
	headers := make(map[string]*tar.Header)
	files := make(map[string][]byte)
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		headers[header.Name] = header
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		files[header.Name] = data
	}
	return headers, files
}
