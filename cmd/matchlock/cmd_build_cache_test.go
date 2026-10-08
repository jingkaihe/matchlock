package main

import (
	"os"
	"os/exec"
	"testing"
)

// A representative dumpe2fs -h header (fields trimmed to the relevant lines).
const dumpe2fsSample = `Filesystem features: has_journal ext_attr resize_inode dir_index filetype extent 64bit flex_bg metadata_csum_seed sparse_super large_file huge_file dir_nlink extra_isize metadata_csum
Filesystem flags: signed_directory_hash
Default mount options: user_xattr acl
Filesystem state:         clean
Errors behavior:          Continue
Filesystem OS type:       Linux
Inode count:              4194304
Block count:              16777216
Reserved block count:     838860
Free blocks:              16467321
Free inodes:              4177053
First block:              0
Block size:               4096
Cluster size:             4096
`

func TestDumpe2fsBlockField(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		want   int64
		ok     bool
	}{
		{"Block count", "Block count:", 16777216, true},
		{"Block size", "Block size:", 4096, true},
		{"Reserved block count", "Reserved block count:", 838860, true},
		{"Free blocks", "Free blocks:", 16467321, true},
		{"missing label", "Units (blocks):", 0, false},
		{"empty value", "Block count:", 0, false},
		{"non-numeric", "Block count:", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(dumpe2fsSample)
			if c.name == "empty value" {
				in = []byte("Block count:   \n")
			}
			if c.name == "non-numeric" {
				in = []byte("Block count: not-a-number\n")
			}
			got, ok := dumpe2fsBlockField(in, c.prefix)
			if ok != c.ok {
				t.Errorf("ok = %v, want %v", ok, c.ok)
			}
			if ok && got != c.want {
				t.Errorf("value = %d, want %d", got, c.want)
			}
		})
	}
}

func TestExt4FSSizeBytes(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	if _, err := exec.LookPath("dumpe2fs"); err != nil {
		t.Skip("dumpe2fs not available")
	}
	dir := t.TempDir()
	img := dir + "/cache.ext4"
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(16 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command("mkfs.ext4", "-q", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}

	got, err := ext4FSSizeBytes(img)
	if err != nil {
		t.Fatalf("ext4FSSizeBytes: %v", err)
	}
	if got <= 0 {
		t.Errorf("fs size = %d, want > 0", got)
	}
	if got > 16*1024*1024 {
		t.Errorf("fs size = %d, want <= 16 MiB (never exceed backing file)", got)
	}
}

func TestFsSizeBytesAtLeast(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	if _, err := exec.LookPath("dumpe2fs"); err != nil {
		t.Skip("dumpe2fs not available")
	}
	dir := t.TempDir()
	img := dir + "/cache.ext4"
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(16 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command("mkfs.ext4", "-q", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}

	if !fsSizeBytesAtLeast(img, 4*1024*1024) {
		t.Errorf("fsSizeBytesAtLeast(16MiB, 4MiB) = false, want true")
	}
	if fsSizeBytesAtLeast(img, 128*1024*1024) {
		t.Errorf("fsSizeBytesAtLeast(16MiB,128MiB) = true, want false")
	}
}

// TestEmptyFileReportsInsufficientFS ensures a backing file of the correct size
// but with no valid ext4 filesystem is reported as insufficient, so the grow
// path runs instead of trusting the file size.
func TestEmptyFileReportsInsufficientFS(t *testing.T) {
	dir := t.TempDir()
	img := dir + "/empty.ext4"
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if fsSizeBytesAtLeast(img, 64*1024*1024) {
		t.Errorf("empty file reported as sufficient, want false")
	}
}

// TestGrowExt4ImageNeverShrinks verifies that a cache already larger than the
// target is never truncated down (which would destroy cached layers).
func TestGrowExt4ImageNeverShrinks(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not available")
	}
	if _, err := exec.LookPath("resize2fs"); err != nil {
		t.Skip("resize2fs not available")
	}
	dir := t.TempDir()
	img := dir + "/cache.ext4"

	// Create a 64 MiB ext4 image.
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command("mkfs.ext4", "-q", img).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v: %s", err, out)
	}
	before, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}

	// Grow with a target SMALLER than the current file (16 MiB). growExt4Image
	// must NOT shrink the file.
	if err := growExt4Image(img, 16*1024*1024); err != nil {
		t.Fatalf("growExt4Image(64MiB file, 16MiB target): %v", err)
	}
	after, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Errorf("file size changed: before=%d after=%d (grow must not shrink)", before.Size(), after.Size())
	}
}

// TestEnsureBigFileUninspectableFSNotTouched verifies that when the cache file
// is already at/above the target but its filesystem is NOT inspectable (e.g.
// dumpe2fs absent), ensureBuildCacheImage does NOT shrink it — the grow path is
// only reached for genuinely small files or a verified FS-not-grown state.
func TestEnsureBigFileUninspectableFSNotTouched(t *testing.T) {
	dir := t.TempDir()
	img := dir + "/cache.ext4"

	// A big file that is NOT a valid ext4 filesystem (like a sparse placeholder).
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(128 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// ensureBuildCacheImage with a small target (64 MiB). The file is 128 MiB
	// (>= target). ext4FSSizeBytes on a non-ext4 file errors, so the repair
	// branch is skipped and the file must be left untouched (not truncated).
	if err := ensureBuildCacheImage(img, 64); err != nil {
		t.Fatalf("ensureBuildCacheImage on uninspectable big file: %v", err)
	}
	after, err := os.Stat(img)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != 128*1024*1024 {
		t.Errorf("file size changed: got %d, want 128 MiB (must not shrink uninspectable big file)", after.Size())
	}
}
