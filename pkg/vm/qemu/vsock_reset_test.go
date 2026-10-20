//go:build linux

package qemu

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestIsVsockReset classifies transport resets distinctly from an orderly EOF
// so a QEMU-TCG acceptance failure can be attributed to the guest virtio-vsock
// transport rather than a normal host close.
func TestIsVsockReset(t *testing.T) {
	assert.True(t, isVsockReset(unix.ECONNRESET))
	assert.True(t, isVsockReset(errors.Join(io.EOF, unix.ECONNRESET)))
	assert.True(t, isVsockReset(unix.EPIPE))
	assert.False(t, isVsockReset(io.EOF))
	assert.False(t, isVsockReset(nil))
}

// TestRecordVsockResetPersistsDiagnostics verifies a reset persists both QEMU's
// own stderr and the guest serial tail, so a guest panic or PID-1 OOM kill on a
// post-boot VM is visible, and that the returned error carries ErrVsockReset.
func TestRecordVsockResetPersistsDiagnostics(t *testing.T) {
	dir := t.TempDir()
	serial := filepath.Join(dir, "vm-serial.log")
	require.NoError(t, os.WriteFile(serial, []byte("Booting Linux...\nKernel panic - not syncing\n"), 0o644))

	stderr := &syncBuffer{}
	_, err := stderr.Write([]byte("qemu: vhost-vsock-pci: reset requested\n"))
	require.NoError(t, err)

	m := &Machine{serialLog: serial, qemuStderr: stderr}
	resetErr := m.recordVsockReset(unix.ECONNRESET)

	require.Error(t, resetErr)
	assert.ErrorIs(t, resetErr, ErrVsockReset)

	data, readErr := os.ReadFile(serial + ".vsock-reset")
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "vhost-vsock-pci: reset requested")
	assert.Contains(t, string(data), "Kernel panic - not syncing")
}
