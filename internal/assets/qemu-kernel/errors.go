package qemukernel

import "errors"

var (
	// ErrManifest is returned when the embedded manifest is unreadable/invalid.
	ErrManifest = errors.New("qemu kernel manifest")
	// ErrUnsupportedArch is returned when the host arch has no vendored qemu kernel.
	ErrUnsupportedArch = errors.New("unsupported arch for vendored qemu kernel")
	// ErrChecksumMismatch is returned when a vendored kernel fails SHA-256 verification.
	ErrChecksumMismatch = errors.New("qemu kernel sha256 mismatch")
	// ErrExtract is returned when the embedded kernel cannot be written to the cache.
	ErrExtract = errors.New("extract qemu kernel")
	// ErrNotQEMUKernel is returned when a caller asks for a qemu kernel on a path
	// that does not need one (arm64 host). arm64 QEMU uses the existing arch
	// kernel (Image), not the amd64 bzImage.
	ErrNotQEMUKernel = errors.New("qemu amd64 bzImage not applicable to this arch")
)
