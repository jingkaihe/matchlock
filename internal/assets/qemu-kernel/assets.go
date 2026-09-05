//go:build linux

package qemukernel

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed" // required for //go:embed directives
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// Embedded vendored QEMU amd64 kernel.
//
//go:embed manifest.json
var manifestJSON []byte

//go:embed kernel-qemu-bzImage-6.19.8.gz
var kernelGz []byte

// Manifest mirrors manifest.json.
type Manifest struct {
	Version              string         `json:"version"`
	Kernel               string         `json:"kernel"`
	SHA256               string         `json:"sha256"`
	CompressedSHA256     string         `json:"compressed_sha256"`
	Config               string         `json:"config"`
	ConfigSHA256         string         `json:"config_sha256"`
	ResolvedConfig       string         `json:"resolved_config"`
	ResolvedConfigSHA256 string         `json:"resolved_config_sha256"`
	Source               SourceManifest `json:"source"`
	Recipe               string         `json:"recipe"`
	Notes                string         `json:"notes"`
}

// SourceManifest describes where the kernel source came from.
type SourceManifest struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	TarSHA256 string `json:"tar_sha256"`
}

var (
	manifestOnce sync.Once
	manifest     *Manifest
	manifestErr  error
)

func getManifest() (*Manifest, error) {
	manifestOnce.Do(func() {
		var m Manifest
		if err := json.Unmarshal(manifestJSON, &m); err != nil {
			manifestErr = errx.Wrap(ErrManifest, err)
			return
		}
		if m.Version == "" || m.Kernel == "" || m.SHA256 == "" {
			manifestErr = ErrManifest
			return
		}
		manifest = &m
	})
	return manifest, manifestErr
}

// PinnedVersion returns the vendored QEMU kernel version (e.g. "6.19.8").
func PinnedVersion() string {
	m, err := getManifest()
	if err != nil {
		return ""
	}
	return m.Version
}

// EmbeddedRef returns a stable, tamper-evident reference string for the
// vendored QEMU kernel, used to record kernel resolution in the lifecycle
// store. It is not a fetchable ref — it marks that the kernel came from the
// embedded asset and carries the payload SHA-256 prefix so a later cache/check
// can be attributed to exactly this artifact.
func EmbeddedRef() string {
	m, err := getManifest()
	if err != nil || m == nil {
		return ""
	}
	payload := m.SHA256
	if payload == "" {
		return "embedded://qemu-kernel-" + m.Version
	}
	prefix := payload
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	return "embedded://qemu-kernel-" + m.Version + "/" + prefix
}

// IsAMD64Host reports whether this host is amd64 (the only arch with a vendored
// QEMU amd64 bzImage). arm64 QEMU uses the standard arch kernel (Image).
func IsAMD64Host() bool {
	return runtime.GOARCH == "amd64" || runtime.GOARCH == "x86_64"
}

// verifySHA256 compares the SHA-256 of data against the expected hex digest.
func verifySHA256(data []byte, expected string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != expected {
		return errx.With(ErrChecksumMismatch, ": got %s want %s", got, expected)
	}
	return nil
}

// resolveCacheDir returns the cache directory for the vendored QEMU kernel,
// mirroring pkg/kernel's layout: ~/.cache/matchlock/kernels/<version>/.
func resolveCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errx.Wrap(ErrExtract, err)
	}
	m, mErr := getManifest()
	if mErr != nil {
		return "", mErr
	}
	return filepath.Join(home, ".cache", "matchlock", "kernels", m.Version), nil
}

// KernelCachePath returns the path where the QEMU kernel is (or will be)
// extracted, without forcing extraction. This is the path the QEMU backend
// passes to QEMU -kernel.
func KernelCachePath() (string, error) {
	dir, err := resolveCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "kernel-qemu"), nil
}

// Ensure extracts the vendored QEMU amd64 kernel to the cache if not already
// present, verifying BOTH the compressed (embedded) SHA-256 and the
// decompressed SHA-256 against the manifest, and returns its path. Extraction
// is atomic (gzip to a same-directory temp file, SHA-256 verify, then rename),
// so a concurrent or interrupted call never leaves a partial kernel at the
// final path. It is amd64-only.
func Ensure() (string, error) {
	if !IsAMD64Host() {
		return "", ErrNotQEMUKernel
	}

	m, err := getManifest()
	if err != nil {
		return "", err
	}

	// The embedded blob itself must match the manifest's compressed SHA-256
	// before we ever publish anything derived from it.
	if err := verifySHA256(kernelGz, m.CompressedSHA256); err != nil {
		return "", err
	}

	destPath, err := KernelCachePath()
	if err != nil {
		return "", err
	}

	// Fast path: already extracted and matches the manifest checksum.
	if data, rerr := os.ReadFile(destPath); rerr == nil && len(data) > 0 {
		if verifySHA256(data, m.SHA256) == nil {
			return destPath, nil
		}
	}

	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errx.Wrap(ErrExtract, err)
	}

	tmp, err := os.CreateTemp(dir, ".kernel-qemu-*")
	if err != nil {
		return "", errx.Wrap(ErrExtract, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	gz, err := gzip.NewReader(bytes.NewReader(kernelGz))
	if err != nil {
		tmp.Close()
		return "", errx.Wrap(ErrExtract, err)
	}
	if _, err := io.Copy(tmp, gz); err != nil {
		tmp.Close()
		gz.Close()
		return "", errx.Wrap(ErrExtract, err)
	}
	if err := gz.Close(); err != nil {
		tmp.Close()
		return "", errx.Wrap(ErrExtract, err)
	}
	if err := tmp.Close(); err != nil {
		return "", errx.Wrap(ErrExtract, err)
	}

	// Verify the extracted (uncompressed) bytes before publishing.
	extracted, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", errx.Wrap(ErrExtract, err)
	}
	if err := verifySHA256(extracted, m.SHA256); err != nil {
		return "", err
	}

	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return "", errx.Wrap(ErrExtract, err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return "", errx.Wrap(ErrExtract, err)
	}
	return destPath, nil
}
