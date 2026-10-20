//go:build !linux

package qemukernel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnsureNotQEMUKernelOffLinux locks the non-Linux stub contract: the darwin
// build of this package must still expose Ensure (tests/acceptance imports and
// calls it while cross-compiling), and it must return the documented sentinel
// and an empty path rather than a usable kernel.
func TestEnsureNotQEMUKernelOffLinux(t *testing.T) {
	path, err := Ensure()
	require.ErrorIs(t, err, ErrNotQEMUKernel)
	require.Empty(t, path)
}
