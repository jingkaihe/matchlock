package image

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeImage(t *testing.T, user, workdir string, entrypoint, cmd, env []string) v1.Image {
	t.Helper()
	base := empty.Image
	cfg, err := base.ConfigFile()
	require.NoError(t, err)
	cfg.Config.User = user
	cfg.Config.WorkingDir = workdir
	cfg.Config.Entrypoint = entrypoint
	cfg.Config.Cmd = cmd
	cfg.Config.Env = env
	img, err := mutate.ConfigFile(base, cfg)
	require.NoError(t, err)
	return img
}

func TestExtractOCIConfig_Normal(t *testing.T) {
	img := fakeImage(t, "nobody", "/app",
		[]string{"python3"}, []string{"app.py"},
		[]string{"PATH=/usr/bin", "FOO=bar=baz"})

	oci := extractOCIConfig(img)
	require.NotNil(t, oci)
	assert.Equal(t, "nobody", oci.User)
	assert.Equal(t, "/app", oci.WorkingDir)
	assert.Equal(t, []string{"python3"}, oci.Entrypoint)
	assert.Equal(t, []string{"app.py"}, oci.Cmd)
	assert.Equal(t, "/usr/bin", oci.Env["PATH"])
	assert.Equal(t, "bar=baz", oci.Env["FOO"])
}

func TestExtractOCIConfig_EmptyConfig(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, nil)
	oci := extractOCIConfig(img)
	require.NotNil(t, oci)
	assert.Empty(t, oci.User)
	assert.Empty(t, oci.Entrypoint)
	assert.Empty(t, oci.Cmd)
}

func TestExtractOCIConfig_EnvWithoutEquals(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, []string{"NOEQUALS", "KEY=val"})
	oci := extractOCIConfig(img)
	require.NotNil(t, oci)
	assert.NotContains(t, oci.Env, "NOEQUALS")
	assert.Equal(t, "val", oci.Env["KEY"])
}

func TestExtractOCIConfig_EnvEmptyValue(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, []string{"EMPTY="})
	oci := extractOCIConfig(img)
	require.NotNil(t, oci)
	v, ok := oci.Env["EMPTY"]
	require.True(t, ok, "EMPTY key should exist")
	assert.Equal(t, "", v)
}

// --- Test helpers for tar/image construction ---

func buildTarLayer(t *testing.T, entries []tar.Header, contents map[string][]byte) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		h := hdr
		if h.Typeflag == tar.TypeReg {
			if data, ok := contents[h.Name]; ok {
				h.Size = int64(len(data))
			}
		}
		require.NoError(t, tw.WriteHeader(&h))
		if h.Typeflag == tar.TypeReg {
			if data, ok := contents[h.Name]; ok {
				_, err := tw.Write(data)
				require.NoError(t, err)
			}
		}
	}
	tw.Close()

	data := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	require.NoError(t, err)
	return layer
}

func buildTarImage(t *testing.T, entries []tar.Header, contents map[string][]byte) v1.Image {
	t.Helper()
	layer := buildTarLayer(t, entries, contents)
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	return img
}

func buildMultiLayerImage(t *testing.T, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	require.NoError(t, err)
	return img
}

// --- ensureRealDir tests ---

func TestEnsureRealDir_CreatesNewDirs(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")

	require.NoError(t, ensureRealDir(root, target))

	for _, rel := range []string{"a", "a/b", "a/b/c"} {
		fi, err := os.Lstat(filepath.Join(root, rel))
		require.NoError(t, err, "expected %s to exist", rel)
		assert.True(t, fi.IsDir(), "%s is not a directory", rel)
	}
}

func TestEnsureRealDir_ReplacesSymlinkWithDir(t *testing.T) {
	root := t.TempDir()

	os.Symlink("/nonexistent", filepath.Join(root, "a"))

	target := filepath.Join(root, "a", "b")
	require.NoError(t, ensureRealDir(root, target))

	fi, err := os.Lstat(filepath.Join(root, "a"))
	require.NoError(t, err)
	assert.Zero(t, fi.Mode()&os.ModeSymlink, "expected symlink to be replaced with real dir")
	assert.True(t, fi.IsDir(), "expected a to be a directory")

	fi, err = os.Lstat(filepath.Join(root, "a", "b"))
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "expected a/b to be a directory")
}

func TestEnsureRealDir_ReplacesFileWithDir(t *testing.T) {
	root := t.TempDir()

	os.WriteFile(filepath.Join(root, "a"), []byte("file"), 0644)

	target := filepath.Join(root, "a", "b")
	require.NoError(t, ensureRealDir(root, target))

	fi, err := os.Lstat(filepath.Join(root, "a"))
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "expected a to be a directory after replacing file")
}

func TestEnsureRealDir_ExistingDirUnchanged(t *testing.T) {
	root := t.TempDir()

	os.MkdirAll(filepath.Join(root, "a", "b"), 0755)
	os.WriteFile(filepath.Join(root, "a", "marker.txt"), []byte("keep"), 0644)

	target := filepath.Join(root, "a", "b", "c")
	require.NoError(t, ensureRealDir(root, target))

	data, err := os.ReadFile(filepath.Join(root, "a", "marker.txt"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(data), "existing directory content should be preserved")
}

func TestEnsureRealDir_DeepSymlinkChain(t *testing.T) {
	root := t.TempDir()

	os.Symlink("nonexist1", filepath.Join(root, "a"))
	target := filepath.Join(root, "a", "deep", "path")

	require.NoError(t, ensureRealDir(root, target))

	fi, err := os.Lstat(filepath.Join(root, "a"))
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "expected symlink 'a' to be replaced with dir")
}

// --- safeCreate tests ---

func TestSafeCreate_NormalFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sub", "file.txt")

	f, err := safeCreate(root, target, 0644)
	require.NoError(t, err)
	f.Write([]byte("hello"))
	f.Close()

	data, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

func TestSafeCreate_ReplacesSymlinkAtTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "link")

	os.Symlink("/etc/passwd", target)

	f, err := safeCreate(root, target, 0644)
	require.NoError(t, err)
	f.Write([]byte("safe"))
	f.Close()

	fi, err := os.Lstat(target)
	require.NoError(t, err)
	assert.Zero(t, fi.Mode()&os.ModeSymlink, "expected symlink to be removed")

	data, _ := os.ReadFile(target)
	assert.Equal(t, "safe", string(data))
}

func TestSafeCreate_ReplacesSymlinkParent(t *testing.T) {
	root := t.TempDir()

	os.Symlink("/tmp", filepath.Join(root, "parent"))

	target := filepath.Join(root, "parent", "file.txt")
	f, err := safeCreate(root, target, 0644)
	require.NoError(t, err)
	f.Write([]byte("ok"))
	f.Close()

	fi, err := os.Lstat(filepath.Join(root, "parent"))
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "expected symlink parent to be replaced with real dir")
}

// --- extractImage tests ---

func TestExtractImage_RegularFilesAndDirs(t *testing.T) {
	img := buildTarImage(t, []tar.Header{
		{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "etc/config", Typeflag: tar.TypeReg, Mode: 0644, Uid: 100, Gid: 200},
	}, map[string][]byte{
		"etc/config": []byte("value=1"),
	})

	b := &Builder{}
	dest := t.TempDir()
	meta, err := b.extractImage(img, dest)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dest, "etc", "config"))
	require.NoError(t, err)
	assert.Equal(t, "value=1", string(data))

	fm, ok := meta["/etc/config"]
	require.True(t, ok, "expected metadata for /etc/config")
	assert.Equal(t, 100, fm.uid)
	assert.Equal(t, 200, fm.gid)
	assert.Equal(t, os.FileMode(0644), fm.mode)
}

func TestExtractImage_Symlinks(t *testing.T) {
	img := buildTarImage(t, []tar.Header{
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "usr/bin/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "usr/bin/python", Typeflag: tar.TypeReg, Mode: 0755},
		{Name: "usr/bin/python3", Typeflag: tar.TypeSymlink, Linkname: "python", Mode: 0777},
	}, map[string][]byte{
		"usr/bin/python": []byte("#!/bin/sh"),
	})

	b := &Builder{}
	dest := t.TempDir()
	_, err := b.extractImage(img, dest)
	require.NoError(t, err)

	link, err := os.Readlink(filepath.Join(dest, "usr", "bin", "python3"))
	require.NoError(t, err)
	assert.Equal(t, "python", link)
}

func TestExtractImage_Hardlinks(t *testing.T) {
	img := buildTarImage(t, []tar.Header{
		{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "bin/original", Typeflag: tar.TypeReg, Mode: 0755},
		{Name: "bin/hardlink", Typeflag: tar.TypeLink, Linkname: "bin/original"},
	}, map[string][]byte{
		"bin/original": []byte("binary"),
	})

	b := &Builder{}
	dest := t.TempDir()
	_, err := b.extractImage(img, dest)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dest, "bin", "hardlink"))
	require.NoError(t, err)
	assert.Equal(t, "binary", string(data))

	origFi, _ := os.Stat(filepath.Join(dest, "bin", "original"))
	linkFi, _ := os.Stat(filepath.Join(dest, "bin", "hardlink"))
	assert.True(t, os.SameFile(origFi, linkFi), "expected hardlink and original to be same file")
}

func TestExtractImage_SkipsPathTraversal(t *testing.T) {
	img := buildTarImage(t, []tar.Header{
		{Name: "good.txt", Typeflag: tar.TypeReg, Mode: 0644},
		{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0644},
	}, map[string][]byte{
		"good.txt":      []byte("ok"),
		"../escape.txt": []byte("evil"),
	})

	b := &Builder{}
	dest := t.TempDir()
	meta, err := b.extractImage(img, dest)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dest, "good.txt"))
	assert.NoError(t, err, "good.txt should exist")

	assert.NotContains(t, meta, "/../escape.txt", "path traversal entry should not appear in metadata")
}

func TestExtractImage_SymlinkDirOverwritten(t *testing.T) {
	l1 := buildTarLayer(t, []tar.Header{
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "usr/lib", Typeflag: tar.TypeSymlink, Linkname: "../lib", Mode: 0777},
	}, nil)
	l2 := buildTarLayer(t, []tar.Header{
		{Name: "usr/lib/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "usr/lib/data.so", Typeflag: tar.TypeReg, Mode: 0644},
	}, map[string][]byte{
		"usr/lib/data.so": []byte("elf"),
	})
	img := buildMultiLayerImage(t, l1, l2)

	b := &Builder{}
	dest := t.TempDir()
	_, err := b.extractImage(img, dest)
	require.NoError(t, err)

	fi, err := os.Lstat(filepath.Join(dest, "usr", "lib"))
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "expected usr/lib to be a real directory, not symlink")

	data, err := os.ReadFile(filepath.Join(dest, "usr", "lib", "data.so"))
	require.NoError(t, err)
	assert.Equal(t, "elf", string(data))
}

func TestExtractImage_FileInSymlinkDir(t *testing.T) {
	l1 := buildTarLayer(t, []tar.Header{
		{Name: "opt/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "opt/dir", Typeflag: tar.TypeSymlink, Linkname: "/tmp", Mode: 0777},
	}, nil)
	l2 := buildTarLayer(t, []tar.Header{
		{Name: "opt/dir/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "opt/dir/file.txt", Typeflag: tar.TypeReg, Mode: 0644},
	}, map[string][]byte{
		"opt/dir/file.txt": []byte("content"),
	})
	img := buildMultiLayerImage(t, l1, l2)

	b := &Builder{}
	dest := t.TempDir()
	_, err := b.extractImage(img, dest)
	require.NoError(t, err)

	fi, err := os.Lstat(filepath.Join(dest, "opt", "dir"))
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "expected opt/dir to be a real dir")

	data, _ := os.ReadFile(filepath.Join(dest, "opt", "dir", "file.txt"))
	assert.Equal(t, "content", string(data))
}

func TestExtractImage_MetadataForAllTypes(t *testing.T) {
	img := buildTarImage(t, []tar.Header{
		{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0755, Uid: 0, Gid: 0},
		{Name: "file.txt", Typeflag: tar.TypeReg, Mode: 0600, Uid: 1000, Gid: 1000},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "file.txt", Mode: 0777, Uid: 0, Gid: 0},
	}, map[string][]byte{
		"file.txt": []byte("data"),
	})

	b := &Builder{}
	dest := t.TempDir()
	meta, err := b.extractImage(img, dest)
	require.NoError(t, err)

	tests := []struct {
		path string
		uid  int
		gid  int
		mode os.FileMode
	}{
		{"/dir", 0, 0, 0755},
		{"/file.txt", 1000, 1000, 0600},
		{"/link", 0, 0, 0777},
	}
	for _, tc := range tests {
		fm, ok := meta[tc.path]
		if !assert.Truef(t, ok, "missing metadata for %s", tc.path) {
			continue
		}
		assert.Equal(t, tc.uid, fm.uid, "%s uid", tc.path)
		assert.Equal(t, tc.gid, fm.gid, "%s gid", tc.path)
		assert.Equal(t, tc.mode, fm.mode, "%s mode", tc.path)
	}
}

func TestExtractImage_OverwriteSymlinkWithRegular(t *testing.T) {
	l1 := buildTarLayer(t, []tar.Header{
		{Name: "target", Typeflag: tar.TypeSymlink, Linkname: "/etc/shadow", Mode: 0777},
	}, nil)
	l2 := buildTarLayer(t, []tar.Header{
		{Name: "target", Typeflag: tar.TypeReg, Mode: 0644},
	}, map[string][]byte{
		"target": []byte("safe content"),
	})
	img := buildMultiLayerImage(t, l1, l2)

	b := &Builder{}
	dest := t.TempDir()
	_, err := b.extractImage(img, dest)
	require.NoError(t, err)

	fi, err := os.Lstat(filepath.Join(dest, "target"))
	require.NoError(t, err)
	assert.Zero(t, fi.Mode()&os.ModeSymlink, "expected regular file, got symlink")

	data, _ := os.ReadFile(filepath.Join(dest, "target"))
	assert.Equal(t, "safe content", string(data))
}

// --- lstatWalk tests ---

func TestLstatWalk_NormalTree(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b"), 0755)
	os.WriteFile(filepath.Join(root, "a", "f1.txt"), []byte("1"), 0644)
	os.WriteFile(filepath.Join(root, "a", "b", "f2.txt"), []byte("2"), 0644)

	var paths []string
	lstatWalk(root, func(path string, info os.FileInfo) {
		rel, _ := filepath.Rel(root, path)
		paths = append(paths, rel)
	})

	sort.Strings(paths)
	expected := []string{".", "a", "a/b", "a/b/f2.txt", "a/f1.txt"}
	sort.Strings(expected)

	assert.Equal(t, expected, paths)
}

func TestLstatWalk_CircularSymlinks(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "a")
	dirB := filepath.Join(root, "b")
	os.Mkdir(dirA, 0755)
	os.Mkdir(dirB, 0755)

	// a/link -> ../b, b/link -> ../a (circular)
	os.Symlink("../b", filepath.Join(dirA, "link"))
	os.Symlink("../a", filepath.Join(dirB, "link"))

	var paths []string
	lstatWalk(root, func(path string, info os.FileInfo) {
		rel, _ := filepath.Rel(root, path)
		paths = append(paths, rel)
	})

	sort.Strings(paths)
	expected := []string{".", "a", "a/link", "b", "b/link"}
	sort.Strings(expected)

	assert.Equal(t, expected, paths)
}

func TestLstatWalk_SymlinksNotFollowed(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "real"), 0755)
	os.WriteFile(filepath.Join(root, "real", "secret.txt"), []byte("hidden"), 0644)
	os.Symlink("real", filepath.Join(root, "link"))

	var visited []string
	lstatWalk(root, func(path string, info os.FileInfo) {
		rel, _ := filepath.Rel(root, path)
		visited = append(visited, rel)
	})

	sort.Strings(visited)
	expected := []string{".", "link", "real", "real/secret.txt"}
	sort.Strings(expected)

	assert.Equal(t, expected, visited)
}

func TestExtractImage_SetuidPreserved(t *testing.T) {
	img := buildTarImage(t, []tar.Header{
		{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "bin/ping", Typeflag: tar.TypeReg, Mode: 0o4755, Uid: 0, Gid: 0},
		{Name: "bin/wall", Typeflag: tar.TypeReg, Mode: 0o2755, Uid: 0, Gid: 5},
		{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o1777},
	}, map[string][]byte{
		"bin/ping": []byte("suid-binary"),
		"bin/wall": []byte("sgid-binary"),
	})

	b := &Builder{}
	dest := t.TempDir()
	meta, err := b.extractImage(img, dest)
	require.NoError(t, err)

	tests := []struct {
		path string
		mode os.FileMode
	}{
		{"/bin/ping", 0o4755},
		{"/bin/wall", 0o2755},
		{"/tmp", 0o1777},
	}
	for _, tc := range tests {
		fm, ok := meta[tc.path]
		if !assert.Truef(t, ok, "missing metadata for %s", tc.path) {
			continue
		}
		assert.Equal(t, tc.mode, fm.mode, "%s mode", tc.path)
	}
}

func TestExtractImage_HardlinkMetadataSkipped(t *testing.T) {
	img := buildTarImage(t, []tar.Header{
		{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "bin/original", Typeflag: tar.TypeReg, Mode: 0755, Uid: 0, Gid: 0},
		{Name: "bin/hardlink", Typeflag: tar.TypeLink, Linkname: "bin/original"},
	}, map[string][]byte{
		"bin/original": []byte("binary"),
	})

	b := &Builder{}
	dest := t.TempDir()
	meta, err := b.extractImage(img, dest)
	require.NoError(t, err)

	assert.NotContains(t, meta, "/bin/hardlink", "hardlink should not have its own metadata entry")

	fm, ok := meta["/bin/original"]
	require.True(t, ok, "expected metadata for /bin/original")
	assert.Equal(t, os.FileMode(0755), fm.mode)
}

func TestHasDebugfsUnsafeChars(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/normal/path", false},
		{"/path with spaces", false},
		{"/path\nwith\nnewlines", true},
		{"/path\rwith\rCR", true},
		{"/path\x00with\x00null", true},
		{"/clean-file.txt", false},
	}
	for _, tc := range tests {
		got := hasDebugfsUnsafeChars(tc.path)
		assert.Equal(t, tc.want, got, "hasDebugfsUnsafeChars(%q)", tc.path)
	}
}

func TestLstatWalkErr_PropagatesError(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("b"), 0644)

	errSentinel := io.ErrUnexpectedEOF
	err := lstatWalkErr(root, func(path string, info os.FileInfo) error {
		if filepath.Base(path) == "a.txt" {
			return errSentinel
		}
		return nil
	})
	assert.Equal(t, errSentinel, err)
}
