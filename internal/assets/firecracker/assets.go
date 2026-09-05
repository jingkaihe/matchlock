//go:build linux

package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/jingkaihe/matchlock/internal/errx"
)

// Embedded vendored assets.
//
//go:embed manifest.json
var manifestJSON []byte

//go:embed tarballs/*.tgz
var tarballs embed.FS

const (
	// BinaryFirecracker is the Firecracker VMM binary name.
	BinaryFirecracker = "firecracker"
	// BinaryJailer is the Firecracker jailer binary name.
	BinaryJailer = "jailer"
)

// Manifest mirrors manifest.json.
type Manifest struct {
	Version       string    `json:"version"`
	ReleaseCommit string    `json:"release_commit"` // upstream git commit for the release
	TagObject     string    `json:"tag_object"`     // upstream annotated tag object ID
	SourceURL     string    `json:"source_url"`
	License       string    `json:"license"`
	Archives      []Archive `json:"archives"`
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
			manifestErr = errx.Wrap(ErrOpenArchive, err)
			return
		}
		if m.Version == "" || len(m.Archives) == 0 {
			manifestErr = ErrOpenArchive
			return
		}
		manifest = &m
	})
	return manifest, manifestErr
}

// ResolveArch maps a Go GOARCH value to a Firecracker asset architecture.
func ResolveArch(goarch string) (Arch, error) {
	switch goarch {
	case "arm64", "aarch64":
		return ArchAarch64, nil
	case "amd64", "x86_64":
		return ArchX86_64, nil
	default:
		return "", errx.With(ErrUnsupportedArch, ": %q", goarch)
	}
}

// PinnedVersion returns the vendored Firecracker version (e.g. "v1.10.1").
func PinnedVersion() string {
	m, err := getManifest()
	if err != nil {
		return ""
	}
	return m.Version
}

// ReleaseCommit returns the upstream git commit that produced the release.
func ReleaseCommit() string {
	m, err := getManifest()
	if err != nil {
		return ""
	}
	return m.ReleaseCommit
}

// archiveForArch returns the vendored archive descriptor for the given arch.
func archiveForArch(arch Arch) (*Archive, error) {
	m, err := getManifest()
	if err != nil {
		return nil, err
	}
	for i := range m.Archives {
		if m.Archives[i].Arch == string(arch) {
			return &m.Archives[i], nil
		}
	}
	return nil, errx.With(ErrUnsupportedArch, ": %s", arch)
}

// loadArchive returns the archive descriptor and its raw bytes for an arch.
// The bytes are read from the embedded store exactly once per call and are
// SHA-256 verified against the manifest.
func loadArchive(arch Arch) (*Archive, []byte, error) {
	a, err := archiveForArch(arch)
	if err != nil {
		return nil, nil, err
	}
	data, err := tarballs.ReadFile("tarballs/" + a.File)
	if err != nil {
		return nil, nil, errx.With(ErrOpenArchive, ": %s: %w", a.File, err)
	}
	if err := verifyChecksum(data, a.Sha256); err != nil {
		return nil, nil, errx.With(ErrChecksumMismatch, ": %s: %w", a.File, err)
	}
	return a, data, nil
}

// verifyChecksum compares the SHA-256 of data against the expected hex digest,
// returning ErrChecksumMismatch on failure. It is the single checksum gate used
// by loadArchive; keeping it separate lets it be exercised directly.
func verifyChecksum(data []byte, expected string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != expected {
		return errx.With(ErrChecksumMismatch, ": got %s want %s", got, expected)
	}
	return nil
}

// VerifyArchive confirms an embedded archive's SHA-256 against the manifest.
// It is the canonical check: extraction also runs it, but callers may call it
// directly to validate a candidate archive before any filesystem writes.
func VerifyArchive(arch Arch) error {
	_, _, err := loadArchive(arch)
	return err
}

// Install extracts the named binary (firecracker or jailer) for the given arch
// into destPath, writing it atomically (temp file + rename) with mode 0755.
// destPath is the full destination file path, not a directory.
func Install(arch Arch, binary, destPath string) error {
	if binary != BinaryFirecracker && binary != BinaryJailer {
		return errx.With(ErrInvalidBinaryName, ": %q", binary)
	}
	a, data, err := loadArchive(arch)
	if err != nil {
		return err
	}
	member, ok := a.Members[binary]
	if !ok || member == "" {
		return errx.With(ErrMissingMember, ": %s in %s", binary, a.File)
	}
	return extractMember(data, member, destPath)
}

// InstallBoth extracts both firecracker and jailer from one verified archive.
func InstallBoth(arch Arch, destDir string) error {
	a, data, err := loadArchive(arch)
	if err != nil {
		return err
	}
	for _, binary := range []string{BinaryFirecracker, BinaryJailer} {
		member, ok := a.Members[binary]
		if !ok || member == "" {
			return errx.With(ErrMissingMember, ": %s in %s", binary, a.File)
		}
		if err := extractMember(data, member, filepath.Join(destDir, binary)); err != nil {
			return err
		}
	}
	return nil
}

// extractMember decodes a gzip/tar archive and writes exactly the named member
// to destPath. The member name comes from the manifest, so no untrusted paths
// are traversed. The destination is written via a same-directory temp file and
// renamed into place on success, so an interrupted install never leaves a
// half-written executable.
func extractMember(archive []byte, member, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return errx.Wrap(ErrExtract, err)
	}

	f, err := os.CreateTemp(filepath.Dir(destPath), "."+filepath.Base(destPath)+"-*")
	if err != nil {
		return errx.Wrap(ErrExtract, err)
	}
	tmp := f.Name()
	// Best-effort cleanup; the temp file is removed even on the success path
	// after the rename (rename means the temp path no longer exists).
	defer func() { _ = os.Remove(tmp) }()

	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return errx.Wrap(ErrExtract, err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		f.Close()
		return errx.Wrap(ErrExtract, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			return errx.Wrap(ErrExtract, err)
		}
		if hdr.Name != member {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			f.Close()
			return errx.With(ErrExtract, ": member %s is not a regular file", member)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return errx.Wrap(ErrExtract, err)
		}
		if err := f.Close(); err != nil {
			return errx.Wrap(ErrExtract, err)
		}
		if err := os.Rename(tmp, destPath); err != nil {
			return errx.Wrap(ErrExtract, err)
		}
		return nil
	}

	f.Close()
	return errx.With(ErrMissingMember, ": %s", member)
}

// FileForArch returns the vendored archive filename for a given Go arch.
func FileForArch(goarch string) (string, error) {
	arch, err := ResolveArch(goarch)
	if err != nil {
		return "", err
	}
	a, err := archiveForArch(arch)
	if err != nil {
		return "", err
	}
	return a.File, nil
}
