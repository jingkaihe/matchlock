//go:build linux

package guestagent

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestHandleExecServesSequentialRequestsOnOneConnection is the regression guard
// for the QEMU/TCG vsock reset under swap pressure. The agent used to close the
// exec-service connection after a single MsgTypeExec/MsgTypeExecStream/file
// request, so a host that reused a connection could never issue a second
// request. Coupled with the per-request dial in the host backends, every short
// sampler/readiness exec manufactured another vsock socket lifecycle; under the
// memory pressure of the swap test those RSTs overlapped and reset the long
// pressure exec. The serve loop must keep the connection open for sequential
// requests until the peer closes it.
//
// The test drives the real handleExec over a socketpair, so it compiles and
// fails on the pre-fix tree (the second response never arrives because the agent
// closed the fd after the first request).
func TestHandleExecServesSequentialRequestsOnOneConnection(t *testing.T) {
	dir := t.TempDir()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	server := fds[0]
	client := os.NewFile(uintptr(fds[1]), "exec-client")

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleExec(server)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("handleExec did not exit after the client closed")
		}
	})

	req, err := json.Marshal(&ListFilesRequest{Path: dir})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		require.NoErrorf(t, writeExecFrame(client, MsgTypeListFiles, req), "request %d", i)

		msgType, data := readExecFrame(t, client)
		require.Equalf(t, MsgTypeFileResult, msgType, "request %d response type", i)

		var resp FileResponse
		require.NoErrorf(t, json.Unmarshal(data, &resp), "request %d response decode", i)
		require.Emptyf(t, resp.Error, "request %d response error", i)
	}
}

// TestNewStreamSocketSetsCloexec verifies the socket-creation path sets
// FD_CLOEXEC atomically, so a workload child cannot inherit a vsock socket and
// keep an exec connection alive past the agent's close.
func TestNewStreamSocketSetsCloexec(t *testing.T) {
	fd, err := newStreamSocket(syscall.AF_UNIX)
	require.NoError(t, err)
	defer syscall.Close(fd)

	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	require.NoError(t, err)
	assert.NotZerof(t, flags&unix.FD_CLOEXEC, "newStreamSocket must set FD_CLOEXEC (flags=%#x)", flags)
}

// TestAcceptVsockSetsCloexec verifies the accept path uses accept4 with
// SOCK_CLOEXEC. It drives the real acceptVsock against an AF_UNIX listener
// (AF_VSOCK is unavailable in unit-test sandboxes) because accept4 is family
// agnostic and the CLOEXEC guarantee is identical.
func TestAcceptVsockSetsCloexec(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "listen.sock")

	lfd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	defer syscall.Close(lfd)
	require.NoError(t, syscall.Bind(lfd, &syscall.SockaddrUnix{Name: sockPath}))
	require.NoError(t, syscall.Listen(lfd, 4))

	type acceptResult struct {
		fd  int
		err error
	}
	resCh := make(chan acceptResult, 1)
	go func() {
		fd, err := acceptVsock(lfd)
		resCh <- acceptResult{fd: fd, err: err}
	}()

	cfd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	defer syscall.Close(cfd)
	require.NoError(t, syscall.Connect(cfd, &syscall.SockaddrUnix{Name: sockPath}))

	select {
	case res := <-resCh:
		require.NoError(t, res.err)
		defer syscall.Close(res.fd)
		flags, err := unix.FcntlInt(uintptr(res.fd), unix.F_GETFD, 0)
		require.NoError(t, err)
		assert.NotZerof(t, flags&unix.FD_CLOEXEC, "acceptVsock must set FD_CLOEXEC (flags=%#x)", flags)
	case <-time.After(5 * time.Second):
		t.Fatal("acceptVsock did not return")
	}
}

// TestMonitorVsockCancelStopUnblocksIdleWatcher verifies the cancellation
// watcher exits promptly on Stop even when the connection stays open and idle.
// The pre-fix watcher did an unbounded raw syscall.Read on the exec fd, so it
// stayed blocked after cmd.Wait and after the caller closed the fd, pinning a
// thread and risking a read from a recycled fd number. Stop must return without
// any data ever arriving on the connection.
func TestMonitorVsockCancelStopUnblocksIdleWatcher(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	// fds[0] stays open and idle for the whole test: no EOF, no data.
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	watch := monitorVsockCancel(fds[0], cmd)

	stopped := make(chan struct{})
	go func() {
		watch.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() blocked on an idle connection; the watcher is still pinned on the fd")
	}

	// Idempotent: a second Stop must not panic or block.
	watch.Stop()
}

// writeExecFrame writes one 1-byte-type + 4-byte-length + payload frame.
func writeExecFrame(w io.Writer, msgType uint8, payload []byte) error {
	header := make([]byte, 5)
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// readExecFrame reads one framed message, failing the test on any I/O error.
func readExecFrame(t *testing.T, r io.Reader) (uint8, []byte) {
	t.Helper()
	header := make([]byte, 5)
	_, err := io.ReadFull(r, header)
	require.NoError(t, err)
	length := binary.BigEndian.Uint32(header[1:])
	data := make([]byte, length)
	if length > 0 {
		_, err = io.ReadFull(r, data)
		require.NoError(t, err)
	}
	return header[0], data
}
