//go:build linux

package qemukernel

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// helpers reused from assets_test.go: sumOf, gunzipForTest, IsAMD64Host, getManifest.

// corruptKernelCache writes arbitrary (wrong) bytes over the kernel at path. The
// point: Ensure() must NOT trust a corrupted cache (zero-length, same-length
// wrong, or longer wrong) and return it; it must re-extract and republish the
// authentic manifest blob.
func corruptKernelCache(t *testing.T, path string, corrupt []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, corrupt, 0644))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(len(corrupt)), fi.Size(), "on-disk corruption length")
}

func assertKernelMatchesManifest(t *testing.T, path string) {
	t.Helper()
	m, err := getManifest()
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, m.SHA256, sumOf(data), "published kernel must match manifest SHA-256")
}

// TestEnsureRepairsCorruptCache proves Ensure() detects a corrupt cached kernel
// (not merely a missing one) and re-extracts the authentic manifest blob rather
// than returning the corrupted bytes. We cover three corruption shapes: zero
// length, same-length wrong bytes, and longer wrong bytes. Before each case we
// first produce a known-good cache by extracting (Ensure), then corrupt it, then
// call Ensure again and require the restored manifest digest.
func TestEnsureRepairsCorruptCache(t *testing.T) {
	if !IsAMD64Host() {
		t.Skipf("vendored qemu kernel is amd64-only; host is %s", os.Getenv("GOARCH"))
	}
	m, err := getManifest()
	require.NoError(t, err)

	home := t.TempDir()
	t.Setenv("HOME", home)

	// Build a known-good cache first so the "corrupt" step is meaningful.
	goodAt, err := Ensure()
	require.NoError(t, err)
	good, err := os.ReadFile(goodAt)
	require.NoError(t, err)
	assertKernelMatchesManifest(t, goodAt)

	cases := []struct {
		name string
		data []byte
	}{
		{"zero-length", []byte{}},
		{"same-length-wrong", sameLengthDifferent(good)},
		{"longer-wrong", append(append([]byte{}, good...), []byte("corruptiontail")...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			corruptKernelCache(t, goodAt, c.data)
			repaired, err := Ensure()
			require.NoError(t, err, "Ensure must repair a corrupt cache")
			require.Equal(t, goodAt, repaired, "repair must reuse the canonical path")
			assertKernelMatchesManifest(t, repaired)
			// The repaired bytes must equal the originally-verified good bytes.
			repairedData, err := os.ReadFile(repaired)
			require.NoError(t, err)
			require.Equal(t, m.SHA256, sumOf(repairedData))
		})
	}
}

// TestEnsureConcurrentExtraction launches several Ensure() calls against an
// initially-absent cache and requires that every caller gets the identical,
// manifest-correct path, that no partial/temp extraction files remain, and that
// the publication is race-free. This tests concurrency of the extract-and-verify
// publisher: the first caller extracts+verifies+renames; the rest must find the
// same artifact and agree on its path.
func TestEnsureConcurrentExtraction(t *testing.T) {
	if !IsAMD64Host() {
		t.Skipf("vendored qemu kernel is amd64-only; host is %s", os.Getenv("GOARCH"))
	}
	m, err := getManifest()
	require.NoError(t, err)

	home := t.TempDir()
	t.Setenv("HOME", home)

	const workers = 8
	results := make([]string, workers)
	errs := make([]error, workers)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // synchronize so they race for the absent cache
			path, err := Ensure()
			results[idx] = path
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	first := results[0]
	for i := 1; i < workers; i++ {
		require.NoError(t, errs[i], "worker %d Ensure failed", i)
		require.Equal(t, first, results[i], "all workers must agree on the kernel path")
	}
	require.NoError(t, errs[0])

	// The published artifact must be manifest-correct.
	assertKernelMatchesManifest(t, first)

	// No temporary extraction files may remain.
	dir := filepath.Dir(first)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, isTempKernelName(e.Name()), "temp extraction file left behind: %s", e.Name())
	}

	// Explicit digest assertions are in the manifest; verify the byte identity too.
	data, err := os.ReadFile(first)
	require.NoError(t, err)
	require.Equal(t, m.SHA256, hex.EncodeToString(hashOf(data)), "published kernel must match manifest digest")
}

func isTempKernelName(name string) bool {
	return len(name) > len(".kernel-qemu-") && name[:len(".kernel-qemu-")] == ".kernel-qemu-"
}

// sameLengthDifferent returns a byte slice of identical length to 'good' but with
// every byte flipped (XOR 0xFF), guaranteeing it is wrong while preserving size.
func sameLengthDifferent(good []byte) []byte {
	out := make([]byte, len(good))
	for i := range good {
		out[i] = good[i] ^ 0xff
	}
	return out
}

func hashOf(data []byte) []byte {
	s := sha256.Sum256(data)
	return s[:]
}
