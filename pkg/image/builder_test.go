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
)

func fakeImage(t *testing.T, user, workdir string, entrypoint, cmd, env []string) v1.Image {
	t.Helper()
	base := empty.Image
	cfg, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Config.User = user
	cfg.Config.WorkingDir = workdir
	cfg.Config.Entrypoint = entrypoint
	cfg.Config.Cmd = cmd
	cfg.Config.Env = env
	img, err := mutate.ConfigFile(base, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestExtractOCIConfig_Normal(t *testing.T) {
	img := fakeImage(t, "nobody", "/app",
		[]string{"python3"}, []string{"app.py"},
		[]string{"PATH=/usr/bin", "FOO=bar=baz"})

	oci := extractOCIConfig(img)
	if oci == nil {
		t.Fatal("expected non-nil OCIConfig")
	}
	if oci.User != "nobody" {
		t.Errorf("User = %q, want %q", oci.User, "nobody")
	}
	if oci.WorkingDir != "/app" {
		t.Errorf("WorkingDir = %q, want %q", oci.WorkingDir, "/app")
	}
	assertStrSlice(t, "Entrypoint", oci.Entrypoint, []string{"python3"})
	assertStrSlice(t, "Cmd", oci.Cmd, []string{"app.py"})
	if oci.Env["PATH"] != "/usr/bin" {
		t.Errorf("Env[PATH] = %q, want %q", oci.Env["PATH"], "/usr/bin")
	}
	if oci.Env["FOO"] != "bar=baz" {
		t.Errorf("Env[FOO] = %q, want %q (should preserve = in value)", oci.Env["FOO"], "bar=baz")
	}
}

func TestExtractOCIConfig_EmptyConfig(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, nil)
	oci := extractOCIConfig(img)
	if oci == nil {
		t.Fatal("expected non-nil OCIConfig even for empty config")
	}
	if oci.User != "" {
		t.Errorf("User = %q, want empty", oci.User)
	}
	if len(oci.Entrypoint) != 0 {
		t.Errorf("Entrypoint = %v, want empty", oci.Entrypoint)
	}
	if len(oci.Cmd) != 0 {
		t.Errorf("Cmd = %v, want empty", oci.Cmd)
	}
}

func TestExtractOCIConfig_EnvWithoutEquals(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, []string{"NOEQUALS", "KEY=val"})
	oci := extractOCIConfig(img)
	if oci == nil {
		t.Fatal("expected non-nil OCIConfig")
	}
	if _, ok := oci.Env["NOEQUALS"]; ok {
		t.Error("env entry without '=' should be skipped")
	}
	if oci.Env["KEY"] != "val" {
		t.Errorf("Env[KEY] = %q, want %q", oci.Env["KEY"], "val")
	}
}

func TestExtractOCIConfig_EnvEmptyValue(t *testing.T) {
	img := fakeImage(t, "", "", nil, nil, []string{"EMPTY="})
	oci := extractOCIConfig(img)
	if oci == nil {
		t.Fatal("expected non-nil OCIConfig")
	}
	if v, ok := oci.Env["EMPTY"]; !ok || v != "" {
		t.Errorf("Env[EMPTY] = %q, ok=%v; want empty string, true", v, ok)
	}
}

func assertStrSlice(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v (len %d), want %v (len %d)", name, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
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
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if data, ok := contents[h.Name]; ok {
				if _, err := tw.Write(data); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	tw.Close()

	data := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

func buildTarImage(t *testing.T, entries []tar.Header, contents map[string][]byte) v1.Image {
	t.Helper()
	layer := buildTarLayer(t, entries, contents)
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func buildMultiLayerImage(t *testing.T, layers ...v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// --- ensureRealDir tests ---

func TestEnsureRealDir_CreatesNewDirs(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c")

	if err := ensureRealDir(root, target); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"a", "a/b", "a/b/c"} {
		fi, err := os.Lstat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("expected %s to exist: %v", rel, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", rel)
		}
	}
}

func TestEnsureRealDir_ReplacesSymlinkWithDir(t *testing.T) {
	root := t.TempDir()

	os.Symlink("/nonexistent", filepath.Join(root, "a"))

	target := filepath.Join(root, "a", "b")
	if err := ensureRealDir(root, target); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("expected symlink to be replaced with real dir")
	}
	if !fi.IsDir() {
		t.Error("expected a to be a directory")
	}

	fi, err = os.Lstat(filepath.Join(root, "a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Error("expected a/b to be a directory")
	}
}

func TestEnsureRealDir_ReplacesFileWithDir(t *testing.T) {
	root := t.TempDir()

	os.WriteFile(filepath.Join(root, "a"), []byte("file"), 0644)

	target := filepath.Join(root, "a", "b")
	if err := ensureRealDir(root, target); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Error("expected a to be a directory after replacing file")
	}
}

func TestEnsureRealDir_ExistingDirUnchanged(t *testing.T) {
	root := t.TempDir()

	os.MkdirAll(filepath.Join(root, "a", "b"), 0755)
	os.WriteFile(filepath.Join(root, "a", "marker.txt"), []byte("keep"), 0644)

	target := filepath.Join(root, "a", "b", "c")
	if err := ensureRealDir(root, target); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(root, "a", "marker.txt"))
	if err != nil || string(data) != "keep" {
		t.Error("existing directory content should be preserved")
	}
}

func TestEnsureRealDir_DeepSymlinkChain(t *testing.T) {
	root := t.TempDir()

	os.Symlink("nonexist1", filepath.Join(root, "a"))
	target := filepath.Join(root, "a", "deep", "path")

	if err := ensureRealDir(root, target); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Error("expected symlink 'a' to be replaced with dir")
	}
}

// --- safeCreate tests ---

func TestSafeCreate_NormalFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sub", "file.txt")

	f, err := safeCreate(root, target, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hello"))
	f.Close()

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", data, "hello")
	}
}

func TestSafeCreate_ReplacesSymlinkAtTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "link")

	os.Symlink("/etc/passwd", target)

	f, err := safeCreate(root, target, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("safe"))
	f.Close()

	fi, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("expected symlink to be removed")
	}

	data, _ := os.ReadFile(target)
	if string(data) != "safe" {
		t.Errorf("got %q, want %q", data, "safe")
	}
}

func TestSafeCreate_ReplacesSymlinkParent(t *testing.T) {
	root := t.TempDir()

	os.Symlink("/tmp", filepath.Join(root, "parent"))

	target := filepath.Join(root, "parent", "file.txt")
	f, err := safeCreate(root, target, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("ok"))
	f.Close()

	fi, err := os.Lstat(filepath.Join(root, "parent"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Error("expected symlink parent to be replaced with real dir")
	}
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
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "etc", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "value=1" {
		t.Errorf("file content = %q, want %q", data, "value=1")
	}

	fm, ok := meta["/etc/config"]
	if !ok {
		t.Fatal("expected metadata for /etc/config")
	}
	if fm.uid != 100 || fm.gid != 200 {
		t.Errorf("uid/gid = %d/%d, want 100/200", fm.uid, fm.gid)
	}
	if fm.mode != 0644 {
		t.Errorf("mode = %o, want 644", fm.mode)
	}
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
	if _, err := b.extractImage(img, dest); err != nil {
		t.Fatal(err)
	}

	link, err := os.Readlink(filepath.Join(dest, "usr", "bin", "python3"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "python" {
		t.Errorf("symlink target = %q, want %q", link, "python")
	}
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
	if _, err := b.extractImage(img, dest); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dest, "bin", "hardlink"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "binary" {
		t.Errorf("hardlink content = %q, want %q", data, "binary")
	}

	origFi, _ := os.Stat(filepath.Join(dest, "bin", "original"))
	linkFi, _ := os.Stat(filepath.Join(dest, "bin", "hardlink"))
	if !os.SameFile(origFi, linkFi) {
		t.Error("expected hardlink and original to be same file")
	}
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
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dest, "good.txt")); err != nil {
		t.Error("good.txt should exist")
	}

	if _, ok := meta["/../escape.txt"]; ok {
		t.Error("path traversal entry should not appear in metadata")
	}
}

func TestExtractImage_SymlinkDirOverwritten(t *testing.T) {
	// Simulates the Playwright/Chromium scenario: layer 1 creates a symlink "lib",
	// layer 2 overrides it with a real directory and adds a file underneath.
	// mutate.Extract flattens this so extractImage sees the dir + file (no symlink).
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
	if _, err := b.extractImage(img, dest); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(dest, "usr", "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Error("expected usr/lib to be a real directory, not symlink")
	}

	data, err := os.ReadFile(filepath.Join(dest, "usr", "lib", "data.so"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "elf" {
		t.Errorf("data.so = %q, want %q", data, "elf")
	}
}

func TestExtractImage_FileInSymlinkDir(t *testing.T) {
	// Layer 1 creates a symlink at opt/dir -> /tmp. Layer 2 adds a dir entry
	// at opt/dir/ (overriding the symlink) plus a file underneath.
	// This is how real OCI images override symlink directories.
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
	if _, err := b.extractImage(img, dest); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(dest, "opt", "dir"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Error("expected opt/dir to be a real dir")
	}

	data, _ := os.ReadFile(filepath.Join(dest, "opt", "dir", "file.txt"))
	if string(data) != "content" {
		t.Errorf("file content = %q, want %q", data, "content")
	}
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
	if err != nil {
		t.Fatal(err)
	}

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
		if !ok {
			t.Errorf("missing metadata for %s", tc.path)
			continue
		}
		if fm.uid != tc.uid || fm.gid != tc.gid {
			t.Errorf("%s: uid/gid = %d/%d, want %d/%d", tc.path, fm.uid, fm.gid, tc.uid, tc.gid)
		}
		if fm.mode != tc.mode {
			t.Errorf("%s: mode = %o, want %o", tc.path, fm.mode, tc.mode)
		}
	}
}

func TestExtractImage_OverwriteSymlinkWithRegular(t *testing.T) {
	// Layer 1 creates a symlink, layer 2 overwrites it with a regular file.
	// The regular file should win after mutate.Extract flattening.
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
	if _, err := b.extractImage(img, dest); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(filepath.Join(dest, "target"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("expected regular file, got symlink")
	}

	data, _ := os.ReadFile(filepath.Join(dest, "target"))
	if string(data) != "safe content" {
		t.Errorf("content = %q, want %q", data, "safe content")
	}
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

	if len(paths) != len(expected) {
		t.Fatalf("got %v, want %v", paths, expected)
	}
	for i := range expected {
		if paths[i] != expected[i] {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], expected[i])
		}
	}
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

	if len(paths) != len(expected) {
		t.Fatalf("got %v, want %v", paths, expected)
	}
	for i := range expected {
		if paths[i] != expected[i] {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], expected[i])
		}
	}
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

	if len(visited) != len(expected) {
		t.Fatalf("got %v, want %v", visited, expected)
	}
	for i := range expected {
		if visited[i] != expected[i] {
			t.Errorf("visited[%d] = %q, want %q", i, visited[i], expected[i])
		}
	}
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
	if err != nil {
		t.Fatal(err)
	}

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
		if !ok {
			t.Errorf("missing metadata for %s", tc.path)
			continue
		}
		if fm.mode != tc.mode {
			t.Errorf("%s: mode = %o, want %o", tc.path, fm.mode, tc.mode)
		}
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
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := meta["/bin/hardlink"]; ok {
		t.Error("hardlink should not have its own metadata entry (shares inode with target)")
	}

	fm, ok := meta["/bin/original"]
	if !ok {
		t.Fatal("expected metadata for /bin/original")
	}
	if fm.mode != 0755 {
		t.Errorf("/bin/original mode = %o, want 755", fm.mode)
	}
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
		if got != tc.want {
			t.Errorf("hasDebugfsUnsafeChars(%q) = %v, want %v", tc.path, got, tc.want)
		}
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
	if err != errSentinel {
		t.Errorf("got %v, want %v", err, errSentinel)
	}
}
