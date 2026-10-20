//go:build !linux

package qemukernel

// Ensure is the non-Linux implementation of the vendored QEMU amd64 kernel
// resolver.
//
// The vendored artifact is an amd64 bzImage that only the amd64 QEMU backend
// consumes. On darwin the Virtualization.framework backend is selected and the
// QEMU branch is unreachable (it is guarded by runtime.GOARCH == "amd64"), but
// the cross-platform acceptance suite still imports this package and calls
// Ensure while compiling, so the symbol must exist here too. Returning the
// documented ErrNotQEMUKernel keeps the failure loud instead of silently
// handing back the wrong kernel.
func Ensure() (string, error) {
	return "", ErrNotQEMUKernel
}
