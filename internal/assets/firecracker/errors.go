// Package firecracker vendors the Firecracker release binaries that Matchlock
// needs on a Linux host.
//
// The v1.10.1 tarballs (aarch64 and x86_64) are embedded into the Linux
// binary so that `matchlock setup linux` and the packaging pipeline can
// install Firecracker/jailer without any network access. Provenance
// (version, upstream release commit, annotated-tag object ID, archive
// SHA-256 per architecture) is recorded in manifest.json and verified at
// extract time.
//
// The error sentinels, arch constants, and manifest schema below are
// portable. The archive embedding and extraction logic lives in assets.go,
// which is build-tagged linux (those binaries are only consumed by the Linux
// host setup; the Darwin backend uses Virtualization.framework instead).
package firecracker

import "errors"

// Arch is a Firecracker target architecture as used in release assets.
type Arch string

const (
	// ArchAarch64 is the ARM64 asset name.
	ArchAarch64 Arch = "aarch64"
	// ArchX86_64 is the AMD64 asset name.
	ArchX86_64 Arch = "x86_64"
)

// Archive describes one vendored Firecracker release archive.
type Archive struct {
	Arch    string            `json:"arch"`
	File    string            `json:"file"`
	Sha256  string            `json:"sha256"`
	Members map[string]string `json:"members"` // binary name -> archive member path
}

var (
	// ErrUnsupportedArch indicates there is no vendored archive for the arch.
	ErrUnsupportedArch = errors.New("unsupported firecracker architecture")
	// ErrChecksumMismatch indicates an embedded archive does not match the manifest.
	ErrChecksumMismatch = errors.New("firecracker archive checksum mismatch")
	// ErrMissingMember indicates a requested binary is not in the archive.
	ErrMissingMember = errors.New("firecracker archive member not found")
	// ErrOpenArchive indicates the embedded archive could not be read.
	ErrOpenArchive = errors.New("open firecracker archive")
	// ErrExtract indicates the tarball could not be decoded or written.
	ErrExtract = errors.New("extract firecracker asset")
	// ErrInvalidBinaryName indicates a binary other than firecracker/jailer.
	ErrInvalidBinaryName = errors.New("invalid firecracker binary name")
)
