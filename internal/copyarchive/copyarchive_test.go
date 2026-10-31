package copyarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTree creates files, and directories for names ending in "/".
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			require.NoError(t, os.MkdirAll(p, 0o755))
			continue
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
}

// readTree is the inverse of writeTree; symlinks are shown as "-> target".
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		require.NoError(t, err)
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
		case info.IsDir():
			out[rel+"/"] = ""
		case info.Mode()&os.ModeSymlink != 0:
			link, _ := os.Readlink(p)
			out[rel] = "-> " + link
		default:
			data, _ := os.ReadFile(p)
			out[rel] = string(data)
		}
		return nil
	}))
	return out
}

func tarOf(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range headers {
		// Regular file content is the link name field, for brevity.
		content := ""
		if h.Typeflag == tar.TypeReg {
			content, h.Linkname, h.Size = h.Linkname, "", int64(len(h.Linkname))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		require.NoError(t, tw.WriteHeader(&h))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

func dir(name string) tar.Header { return tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755} }
func file(name, content string) tar.Header {
	return tar.Header{Name: name, Typeflag: tar.TypeReg, Linkname: content}
}
func symlink(name, target string) tar.Header {
	return tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}
}

func leftovers(t *testing.T, root string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".*.matchlock-cp-*"))
	require.NoError(t, err)
	return matches
}

func umask() os.FileMode {
	mask := syscall.Umask(0)
	syscall.Umask(mask)
	return os.FileMode(mask)
}

// TestCopyPathRules covers the docker cp rules for sources and destinations.
func TestCopyPathRules(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		dest    string
		setup   map[string]string
		want    map[string]string
		wantErr error
	}{
		{name: "dir to new path", src: "src", dest: "out",
			want: map[string]string{"out/": "", "out/a.txt": "a", "out/sub/": "", "out/sub/b.txt": "b"}},
		{name: "dir into existing dir", src: "src", dest: "out", setup: map[string]string{"out/": ""},
			want: map[string]string{"out/": "", "out/src/": "", "out/src/a.txt": "a", "out/src/sub/": "", "out/src/sub/b.txt": "b"}},
		{name: "dir contents into existing dir", src: "src/.", dest: "out", setup: map[string]string{"out/keep": "k"},
			want: map[string]string{"out/": "", "out/keep": "k", "out/a.txt": "a", "out/sub/": "", "out/sub/b.txt": "b"}},
		{name: "dir contents to new path", src: "src/.", dest: "out/",
			want: map[string]string{"out/": "", "out/a.txt": "a", "out/sub/": "", "out/sub/b.txt": "b"}},
		{name: "file into existing dir", src: "file.txt", dest: "out/", setup: map[string]string{"out/": ""},
			want: map[string]string{"out/": "", "out/file.txt": "f"}},
		{name: "file to new name", src: "file.txt", dest: "new.txt", want: map[string]string{"new.txt": "f"}},
		{name: "file replaces file", src: "file.txt", dest: "old.txt", setup: map[string]string{"old.txt": "old"},
			want: map[string]string{"old.txt": "f"}},
		{name: "symlink copied as link", src: "link", dest: "copy", want: map[string]string{"copy": "-> file.txt"}},
		{name: "trailing slash on file source", src: "file.txt/", dest: "out", wantErr: ErrSource},
		{name: "file into missing dir", src: "file.txt", dest: "missing/", wantErr: ErrDestination},
		{name: "dir onto file", src: "src", dest: "old.txt", setup: map[string]string{"old.txt": "old"}, wantErr: ErrDestination},
		{name: "missing parent", src: "src", dest: "missing/out", wantErr: ErrDestination},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srcRoot, destRoot := t.TempDir(), t.TempDir()
			writeTree(t, srcRoot, map[string]string{"src/a.txt": "a", "src/sub/b.txt": "b", "file.txt": "f"})
			require.NoError(t, os.Symlink("file.txt", filepath.Join(srcRoot, "link")))
			writeTree(t, destRoot, tt.setup)

			err := func() error {
				source, readPath, err := StatSource(srcRoot+"/"+tt.src, false)
				if err != nil {
					return err
				}
				var buf bytes.Buffer
				require.NoError(t, WriteArchive(context.Background(), &buf, readPath, source, WriteOptions{}))
				plan, err := PlanDestination(destRoot+"/"+tt.dest, &source)
				if err != nil {
					return err
				}
				return Extract(context.Background(), &buf, plan, HostExtractOptions(0, 0, nil))
			}()
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, readTree(t, destRoot))
			assert.Empty(t, leftovers(t, destRoot))
		})
	}
}

// fakeGuest serves a directory as the guest filesystem.
type fakeGuest struct {
	root string
	last CopyInRequest
}

func (g *fakeGuest) CopyIn(ctx context.Context, req CopyInRequest, archive io.Reader) error {
	g.last = req
	plan, err := PlanDestination(g.root+req.Dest, req.Source)
	if err != nil {
		return err
	}
	return Extract(ctx, archive, plan, ExtractOptions{ExactModes: true})
}

func (g *fakeGuest) CopyOut(ctx context.Context, req CopyOutRequest) (Source, io.ReadCloser, error) {
	source, readPath, err := StatSource(g.root+req.Path, req.FollowLink)
	if err != nil {
		return Source{}, nil, err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(WriteArchive(ctx, pw, readPath, source, WriteOptions{Exclude: req.Exclude})) }()
	return source, pr, nil
}

// TestRoundTrip copies a tree into a guest and back, checking excludes,
// skipped sockets, symlinks, modes, and times.
func TestRoundTrip(t *testing.T) {
	// Unix socket paths are short, so avoid t.TempDir.
	host, err := os.MkdirTemp("", "cp")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(host) })
	writeTree(t, host, map[string]string{
		"src/bin/run.sh": "#!/bin/sh", "src/.env": "secret", "src/node_modules/x.js": "x",
		"src/logs/a.log": "a", "src/logs/keep.log": "k",
	})
	require.NoError(t, os.Chmod(filepath.Join(host, "src/bin/run.sh"), 0o755))
	require.NoError(t, os.Symlink("bin/run.sh", filepath.Join(host, "src/run")))
	mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, os.Chtimes(filepath.Join(host, "src/bin"), mtime, mtime))
	listener, err := net.Listen("unix", filepath.Join(host, "src/sock"))
	require.NoError(t, err)
	defer listener.Close()

	guest := &fakeGuest{root: t.TempDir()}
	require.NoError(t, os.Mkdir(guest.root+"/workspace", 0o755))
	var skipped []string
	require.NoError(t, ToGuest(context.Background(), guest, host+"/src/.", "/workspace", ToGuestOptions{
		Owner:   "1000",
		Exclude: []string{".env", "node_modules", "logs/*.log", "!logs/keep.log"},
		OnSkip:  func(name, _ string) { skipped = append(skipped, name) },
	}))
	assert.Equal(t, []string{"src/sock"}, skipped)
	assert.Equal(t, "1000", guest.last.Owner)

	out := filepath.Join(host, "out")
	require.NoError(t, FromGuest(context.Background(), guest, "/workspace", out, FromGuestOptions{}))
	assert.Equal(t, map[string]string{
		"bin/": "", "bin/run.sh": "#!/bin/sh", "logs/": "", "logs/keep.log": "k", "run": "-> bin/run.sh",
	}, readTree(t, out))
	info, err := os.Stat(filepath.Join(out, "bin/run.sh"))
	require.NoError(t, err)
	assert.Equal(t, 0o755&^umask(), info.Mode().Perm())
	info, err = os.Stat(filepath.Join(out, "bin"))
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(mtime), "dir mtime %s", info.ModTime())
}

// lyingGuest reports a fixed source and archive for any copy-out request.
type lyingGuest struct {
	fakeGuest
	source  Source
	archive []byte
}

func (g *lyingGuest) CopyOut(context.Context, CopyOutRequest) (Source, io.ReadCloser, error) {
	return g.source, io.NopCloser(bytes.NewReader(g.archive)), nil
}

// TestFromGuestTrustsOnlyTheReportedType checks that a guest cannot choose
// where its entries land by misreporting the source name or contents flag.
func TestFromGuestTrustsOnlyTheReportedType(t *testing.T) {
	home := t.TempDir()
	guest := &lyingGuest{
		source:  Source{Name: ".", IsDir: true, ContentsOnly: true},
		archive: tarOf(t, file(".bashrc", "pwned"), file(".ssh/authorized_keys", "key")),
	}
	err := FromGuest(context.Background(), guest, "/out/report.txt", home, FromGuestOptions{})
	require.ErrorIs(t, err, ErrArchive)
	assert.Empty(t, readTree(t, home))

	require.ErrorIs(t, FromGuest(context.Background(), guest, "out/report.txt", home, FromGuestOptions{}), ErrSource)
}

// TestExtractUntrustedArchive covers what a malicious guest archive can try
// when it is extracted on the host.
func TestExtractUntrustedArchive(t *testing.T) {
	src := &Source{Name: "src", IsDir: true}
	tests := []struct {
		name        string
		source      *Source
		setup       func(t *testing.T, dest, outside string)
		headers     []tar.Header
		wantErr     error
		wantSkipped []string
		want        map[string]string
	}{
		{name: "parent traversal", headers: []tar.Header{file("a/../../evil", "x")}, wantErr: ErrArchive},
		{name: "absolute name", headers: []tar.Header{file("/evil", "x")}, wantErr: ErrArchive},
		{name: "entry outside the source", source: src, headers: []tar.Header{dir("src/"), file("other", "x")}, wantErr: ErrArchive},
		{name: "invalid source name", source: &Source{Name: "..", IsDir: true}, wantErr: ErrArchive},
		{
			name:        "symlinks must stay inside the destination",
			headers:     []tar.Header{file("f", "x"), symlink("in", "f"), symlink("up", "../outside.txt"), symlink("abs", "/etc/passwd")},
			wantSkipped: []string{"up", "abs"},
			want:        map[string]string{"f": "x", "in": "-> f"},
		},
		{
			name: "existing links are replaced, not written through",
			setup: func(t *testing.T, dest, outside string) {
				require.NoError(t, os.Symlink(outside, filepath.Join(dest, "sym")))
				require.NoError(t, os.Link(outside, filepath.Join(dest, "hard")))
			},
			headers: []tar.Header{file("sym", "new"), file("hard", "new")},
			want:    map[string]string{"sym": "new", "hard": "new"},
		},
		{
			name: "hard links only to files in the archive",
			setup: func(t *testing.T, dest, outside string) {
				require.NoError(t, os.WriteFile(filepath.Join(dest, "existing"), []byte("e"), 0o644))
			},
			headers: []tar.Header{
				file("a", "a"),
				{Name: "b", Typeflag: tar.TypeLink, Linkname: "a"},
				{Name: "c", Typeflag: tar.TypeLink, Linkname: "existing"},
			},
			wantSkipped: []string{"c"},
			want:        map[string]string{"a": "a", "b": "a", "existing": "e"},
		},
		{
			name:        "devices and FIFOs are skipped",
			headers:     []tar.Header{{Name: "null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, {Name: "fifo", Typeflag: tar.TypeFifo}},
			wantSkipped: []string{"null", "fifo"},
			want:        map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dest, outside := filepath.Join(root, "dest"), filepath.Join(root, "outside.txt")
			require.NoError(t, os.Mkdir(dest, 0o755))
			require.NoError(t, os.WriteFile(outside, []byte("original"), 0o644))
			if tt.setup != nil {
				tt.setup(t, dest, outside)
			}

			var skipped []string
			plan, err := PlanDestination(dest, tt.source)
			if err == nil {
				err = Extract(context.Background(), bytes.NewReader(tarOf(t, tt.headers...)), plan,
					HostExtractOptions(0, 0, func(name, _ string) { skipped = append(skipped, name) }))
			}
			got, readErr := os.ReadFile(outside)
			require.NoError(t, readErr)
			assert.Equal(t, "original", string(got), "file outside the destination changed")
			assert.NoFileExists(t, filepath.Join(root, "evil"))
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSkipped, skipped)
			assert.Equal(t, tt.want, readTree(t, dest))
		})
	}
}

// TestExtractCleansUpOnFailure checks that limits, truncation, and
// cancellation leave the destination as it was.
func TestExtractCleansUpOnFailure(t *testing.T) {
	dirArchive := tarOf(t, dir("src/"), file("src/a", "12345"), file("src/b", "67890"))
	fileArchive := tarOf(t, file("f", strings.Repeat("x", 4096)))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		source  Source
		archive []byte
		opts    ExtractOptions
		wantErr error
	}{
		{name: "byte limit", source: Source{Name: "src", IsDir: true}, archive: dirArchive, opts: HostExtractOptions(8, 0, nil), wantErr: ErrLimit},
		{name: "entry limit", source: Source{Name: "src", IsDir: true}, archive: dirArchive, opts: HostExtractOptions(0, 2, nil), wantErr: ErrLimit},
		{name: "cancelled", ctx: cancelled, source: Source{Name: "src", IsDir: true}, archive: dirArchive, wantErr: context.Canceled},
		{name: "truncated file", source: Source{Name: "f"}, archive: fileArchive[:1024], wantErr: ErrArchive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dest := filepath.Join(root, "dest")
			if !tt.source.IsDir {
				require.NoError(t, os.WriteFile(dest, []byte("original"), 0o644))
			}
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			plan, err := PlanDestination(dest, &tt.source)
			require.NoError(t, err)
			require.ErrorIs(t, Extract(ctx, bytes.NewReader(tt.archive), plan, tt.opts), tt.wantErr)

			if tt.source.IsDir {
				assert.NoDirExists(t, dest)
			} else {
				got, err := os.ReadFile(dest)
				require.NoError(t, err)
				assert.Equal(t, "original", string(got))
			}
			assert.Empty(t, leftovers(t, root))
		})
	}
}

// TestExtractGuestOptions covers how copies into a guest keep modes and set
// ownership, unlike host extraction, which drops special bits.
func TestExtractGuestOptions(t *testing.T) {
	archive := tarOf(t, dir("src/"), tar.Header{Name: "src/sub/suid", Typeflag: tar.TypeReg, Mode: 0o4755, Linkname: "x"})
	source := Source{Name: "src", IsDir: true}
	for _, guest := range []bool{false, true} {
		root := t.TempDir()
		plan, err := PlanDestination(filepath.Join(root, "out"), &source)
		require.NoError(t, err)
		opts := HostExtractOptions(0, 0, nil)
		if guest {
			opts = ExtractOptions{ExactModes: true}
			if os.Geteuid() == 0 {
				opts.Owner = &Owner{UID: 1234, GID: 5678}
			}
		}
		require.NoError(t, Extract(context.Background(), bytes.NewReader(archive), plan, opts))

		info, err := os.Lstat(filepath.Join(root, "out/sub/suid"))
		require.NoError(t, err)
		assert.Equal(t, guest, info.Mode()&os.ModeSetuid != 0, "setuid kept only in the guest")
		if opts.Owner != nil {
			for _, name := range []string{"out", "out/sub", "out/sub/suid"} {
				info, err := os.Lstat(filepath.Join(root, name))
				require.NoError(t, err)
				assert.Equal(t, uint32(1234), info.Sys().(*syscall.Stat_t).Uid, name)
			}
		}
	}
}
