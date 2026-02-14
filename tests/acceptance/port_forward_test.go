//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	forwardedProbePayload  = "port-forward-payload"
	portForwardWaitTimeout = 30 * time.Second
)

func TestCLIRunPublishForwardsTCP(t *testing.T) {
	bin := matchlockBin(t)
	localPort := reserveTCPPort(t)

	cmd, runDone, runStderr := startPersistentRunWithArgs(t, bin, "-p", fmt.Sprintf("%d:8080", localPort))
	runPID := cmd.Process.Pid

	var vmID string
	t.Cleanup(func() {
		cleanupPersistentRun(t, bin, vmID, runPID, runDone)
	})

	vmID = waitForRunningVMID(t, runPID, runDone, runStderr)
	waitForExecReady(t, vmID, runPID, runDone, runStderr)

	require.Eventually(t, func() bool {
		return strings.Contains(runStderr.String(), fmt.Sprintf("Forwarding 127.0.0.1:%d -> sandbox:8080", localPort))
	}, 10*time.Second, 100*time.Millisecond, "run stderr: %s", runStderr.String())

	serverCmd, serverDone, serverStdout, serverStderr := startGuestTCPServerCLIProcess(t, bin, vmID, 8080)
	t.Cleanup(func() {
		stopBackgroundCommand(serverCmd, serverDone)
	})
	requireCommandRunning(t, serverDone, "guest TCP server", serverStdout, serverStderr)

	waitForForwardedTCPEcho(t, localPort, forwardedProbePayload)
}

func TestCLIPortForwardCommandForwardsTCP(t *testing.T) {
	bin := matchlockBin(t)

	cmd, runDone, runStderr := startPersistentRun(t, bin)
	runPID := cmd.Process.Pid

	var vmID string
	t.Cleanup(func() {
		cleanupPersistentRun(t, bin, vmID, runPID, runDone)
	})

	vmID = waitForRunningVMID(t, runPID, runDone, runStderr)
	waitForExecReady(t, vmID, runPID, runDone, runStderr)

	serverCmd, serverDone, serverStdout, serverStderr := startGuestTCPServerCLIProcess(t, bin, vmID, 8080)
	t.Cleanup(func() {
		stopBackgroundCommand(serverCmd, serverDone)
	})
	requireCommandRunning(t, serverDone, "guest TCP server", serverStdout, serverStderr)

	localPort := reserveTCPPort(t)
	pfCmd, pfDone, pfStdout, pfStderr := startBackgroundCommand(t, bin, "port-forward", vmID, fmt.Sprintf("%d:8080", localPort))
	t.Cleanup(func() {
		stopBackgroundCommand(pfCmd, pfDone)
	})

	require.Eventually(t, func() bool {
		return strings.Contains(pfStderr.String(), fmt.Sprintf("Forwarding 127.0.0.1:%d -> sandbox:8080", localPort))
	}, 10*time.Second, 100*time.Millisecond, "port-forward stdout: %s stderr: %s", pfStdout.String(), pfStderr.String())

	waitForForwardedTCPEcho(t, localPort, forwardedProbePayload)
}

func TestSDKPortForwardCreatesBinding(t *testing.T) {
	client := launchAlpine(t)

	localPort := reserveTCPPort(t)
	bindings, err := client.PortForward(context.Background(), fmt.Sprintf("%d:8080", localPort))
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	assert.Equal(t, "127.0.0.1", bindings[0].Address)
	assert.Equal(t, localPort, bindings[0].LocalPort)
	assert.Equal(t, 8080, bindings[0].RemotePort)

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 2*time.Second)
	require.NoError(t, err, "expected SDK port-forward listener to accept connections")
	_ = conn.Close()
}

func startPersistentRunWithArgs(t *testing.T, bin string, runArgs ...string) (*exec.Cmd, <-chan error, *lockedBuffer) {
	t.Helper()
	args := []string{"run", "--image", "alpine:latest", "--rm=false"}
	args = append(args, runArgs...)

	cmd := exec.Command(bin, args...)
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	require.NoErrorf(t, cmd.Start(), "failed to start run with args: %v", args)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	return cmd, done, stderr
}

func startBackgroundCommand(t *testing.T, bin string, args ...string) (*exec.Cmd, <-chan error, *lockedBuffer, *lockedBuffer) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	stdout := &lockedBuffer{}
	stderr := &lockedBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	require.NoErrorf(t, cmd.Start(), "failed to start command: %s %v", bin, args)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		close(done)
	}()
	return cmd, done, stdout, stderr
}

func stopBackgroundCommand(cmd *exec.Cmd, done <-chan error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)

	select {
	case <-done:
		return
	case <-time.After(2 * time.Second):
	}

	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func startGuestTCPServerCLIProcess(t *testing.T, bin, vmID string, port int) (*exec.Cmd, <-chan error, *lockedBuffer, *lockedBuffer) {
	t.Helper()
	return startBackgroundCommand(
		t,
		bin,
		"exec",
		vmID,
		"--",
		"nc",
		"-lk",
		"-p",
		strconv.Itoa(port),
		"-e",
		"/bin/cat",
	)
}

func requireCommandRunning(t *testing.T, done <-chan error, name string, stdout, stderr *lockedBuffer) {
	t.Helper()
	select {
	case err := <-done:
		require.Failf(t, "%s exited early", "error=%v stdout=%q stderr=%q", name, err, stdout.String(), stderr.String())
	default:
	}
}

func waitForForwardedTCPEcho(t *testing.T, localPort int, payload string) {
	t.Helper()
	var lastErr error
	deadline := time.Now().Add(portForwardWaitTimeout)
	for time.Now().Before(deadline) {
		addr := fmt.Sprintf("127.0.0.1:%d", localPort)
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}

		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.WriteString(conn, payload); err != nil {
			lastErr = err
			_ = conn.Close()
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if tcpConn, ok := conn.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}

		echoed := make([]byte, len(payload))
		_, err = io.ReadFull(conn, echoed)
		_ = conn.Close()
		if err == nil && string(echoed) == payload {
			return
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("unexpected echo: %q", string(echoed))
		}
		time.Sleep(200 * time.Millisecond)
	}

	require.Failf(t, "timed out waiting for forwarded TCP echo", "addr=127.0.0.1:%d payload=%q last_err=%v", localPort, payload, lastErr)
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "reserve local tcp port")
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "expected TCPAddr, got %T", ln.Addr())
	return addr.Port
}
