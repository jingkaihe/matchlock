//go:build linux

package qemukernel

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func sumOf(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

func TestManifestValid(t *testing.T) {
	m, err := getManifest()
	require.NoError(t, err)
	require.Equal(t, "6.19.8", m.Version)
	require.Equal(t, "kernel-qemu-bzImage-6.19.8.gz", m.Kernel)
	digest := "must be a full 64-hex digest"
	require.Len(t, m.SHA256, 64, digest)
	require.Len(t, m.CompressedSHA256, 64)
	require.Len(t, m.ConfigSHA256, 64)
	require.Len(t, m.ResolvedConfigSHA256, 64)
	require.NotEmpty(t, m.Source.TarSHA256)
	// All four digests must be valid hex of exactly 32 bytes (64 chars),
	// and different from each other (each names a distinct artifact).
	isHex := func(s string) bool {
		for i := 0; i < len(s); i++ {
			c := s[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return len(s) == 64
	}
	require.True(t, isHex(m.SHA256))
	require.True(t, isHex(m.CompressedSHA256))
	require.True(t, isHex(m.ConfigSHA256))
	require.True(t, isHex(m.Source.TarSHA256))
	require.NotEqual(t, m.SHA256, m.CompressedSHA256)
	require.NotEqual(t, m.SHA256, m.ConfigSHA256)
	require.NotEqual(t, m.CompressedSHA256, m.ConfigSHA256)
}

func TestPinnedVersion(t *testing.T) {
	require.Equal(t, "6.19.8", PinnedVersion())
}

func TestVerifySHA256(t *testing.T) {
	require.NoError(t, verifySHA256([]byte("hello"), sumOf([]byte("hello"))))
	require.ErrorIs(t, verifySHA256([]byte("hello"), "deadbeef"), ErrChecksumMismatch)
}

func TestEmbeddedKernelMatchesManifestDigests(t *testing.T) {
	m, err := getManifest()
	require.NoError(t, err)

	// Decompress the embedded kernel and confirm it matches the manifest's
	// uncompressed SHA-256 AND the compressed SHA-256. This is the single point
	// of truth that the vendored blob is intact and matches what was boot-tested.
	require.Equal(t, m.CompressedSHA256, sumOf(kernelGz))
	uncompressed := gunzipForTest(t, kernelGz)
	require.Equal(t, m.SHA256, sumOf(uncompressed))
}

func TestEnsureExtractsAndVerifies(t *testing.T) {
	m, err := getManifest()
	require.NoError(t, err)
	if !IsAMD64Host() {
		t.Skipf("vendored qemu kernel is amd64-only; host is %s", os.Getenv("GOARCH"))
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, err := Ensure()
	require.NoError(t, err)
	require.FileExists(t, path)

	// Extracted kernel must match the manifest SHA-256.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, m.SHA256, sumOf(data))

	// Idempotent fast path.
	again, err := Ensure()
	require.NoError(t, err)
	require.Equal(t, path, again)
}

func TestEnsureArchGuard(t *testing.T) {
	if IsAMD64Host() {
		t.Skip("arch guard needs a non-amd64 view")
	}
	_, err := Ensure()
	require.ErrorIs(t, err, ErrNotQEMUKernel)
}

func TestKernelCachePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := KernelCachePath()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".cache", "matchlock", "kernels", "6.19.8", "kernel-qemu"), p)
}

// gunzipForTest decompresses the embedded kernel blob for verification.
func gunzipForTest(t *testing.T, gz []byte) []byte {
	t.Helper()
	// Reuse the real decompress path via a tiny helper is over-engineered; do it
	// directly so the test genuinely verifies the blob.
	r, err := gzip.NewReader(bytes.NewReader(gz))
	require.NoError(t, err)
	defer r.Close()
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return out
}
