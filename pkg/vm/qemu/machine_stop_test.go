//go:build linux

package qemu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SCOPE OF THIS FILE, stated so the tests are not over-read.
//
//   - termIgnoringChild exercises the REAL lifecycle: a child that ignores SIGTERM, a single
//     owner of cmd.Wait started immediately after the process exists, a real done channel, and
//     the production Stop path. Assertions are on the actual terminating signal, so "the
//     process died" cannot be satisfied by it dying the convenient way.
//   - controlledChannelMachine exercises POLICY: branch selection and window honouring, with the
//     completion channel driven by the test. That is the only way to place "Wait returned later"
//     precisely; it is NOT an observation of slow kernel termination, and each use says so.
//   - the TestStopProduction* tests bind the SHIPPED stopConfirm constant through the public
//     Stop(), so reverting the constant fails them (mutation-verified).
//
// All timing assertions use monotonic elapsed time, never wall-clock stamps.

// termIgnoringChild is a real process that cannot be stopped by SIGTERM.
type termIgnoringChild struct {
	cmd     *exec.Cmd
	pid     int
	done    chan struct{}
	mu      sync.Mutex
	reaped  bool
	waitErr error
}

// startTermIgnoringChild starts a child that ignores SIGTERM across its process group and
// returns it with a Machine wired to its real completion channel.
//
// Why `exec sleep` and not a plain `sh -c 'trap "" TERM; sleep 60'`: the plain form leaves a
// shell with a sleep child, so a group SIGTERM kills the child and the shell then exits through
// the TERM path, letting a test pass without ever exercising escalation. With `exec`, the
// ignored-TERM disposition survives execve and replaces the shell, giving ONE process that has
// no descendants and cannot be stopped by TERM. That also lets cleanup use os.Process.Kill
// instead of a raw group signal (see forceKill). Verified on this host: 0 children and the
// process survives TERM.
//
// The Wait owner starts immediately after a successful Start, so every exit path — including an
// early readiness failure — tears down a reaped process rather than an orphan.
func startTermIgnoringChild(t *testing.T) (*termIgnoringChild, *Machine) {
	t.Helper()

	readyR, readyW, err := os.Pipe()
	require.NoError(t, err)

	cmd := exec.Command("sh", "-c", `trap "" TERM; printf 'READY\n'; exec sleep 60`)
	cmd.Stdout = readyW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	_ = readyW.Close() // the child holds its own duplicate of the write end

	c := &termIgnoringChild{cmd: cmd, pid: cmd.Process.Pid, done: make(chan struct{})}
	m := &Machine{pid: c.pid, done: c.done, started: true}

	go func() {
		werr := cmd.Wait()
		c.mu.Lock()
		c.waitErr = werr
		c.reaped = true
		c.mu.Unlock()
		m.mutex.Lock()
		m.waitErr = werr
		m.mutex.Unlock()
		close(c.done)
	}()

	t.Cleanup(func() {
		_ = readyR.Close()
		c.forceKill()
		c.join(t, 5*time.Second)
	})

	// Readiness handshake: the child announces only AFTER the trap is installed. Without it the
	// SIGTERM could land first and the escalation path would never run.
	ready := make(chan bool, 1)
	go func() {
		buf := make([]byte, 16)
		n, _ := readyR.Read(buf)
		ready <- strings.Contains(string(buf[:n]), "READY")
	}()
	select {
	case ok := <-ready:
		require.True(t, ok, "child must ignore TERM before we signal it")
	case <-time.After(5 * time.Second):
		t.Fatal("child never signalled readiness")
	}
	return c, m
}

// forceKill kills the child without racing its reaper.
//
// An earlier version read a "reaped" flag, released the mutex and then sent a RAW group signal.
// That is unsound: Wait can complete in the gap, the pid gets recycled, and the signal lands on
// an unrelated process. Holding the mutex would not fix it either, because the reap happens
// inside cmd.Wait, outside that mutex.
//
// The fixture is therefore a single process with no descendants (the ignored-TERM disposition
// survives exec, so `sleep` itself ignores TERM), and cleanup signals it through os.Process,
// which Go guards internally: after Wait it returns ErrProcessDone instead of signalling a
// recycled pid.
func (c *termIgnoringChild) forceKill() {
	_ = c.cmd.Process.Kill()
}

func (c *termIgnoringChild) join(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(within):
		t.Errorf("child was not reaped within %s during cleanup", within)
	}
}

// signal returns the signal that terminated the child, or 0 if it did not die by signal.
func (c *termIgnoringChild) signal(t *testing.T) syscall.Signal {
	t.Helper()
	require.NotNil(t, c.cmd.ProcessState, "child state unavailable: it was not waited for")
	ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus)
	require.True(t, ok, "no WaitStatus")
	return ws.Signal()
}

// controlledChannelMachine is a POLICY fixture: a real process this test owns, with the
// completion channel driven by the test so "Wait returns later" is deterministic. It does not
// observe real reaping; that is what termIgnoringChild is for.
func controlledChannelMachine(t *testing.T) (*Machine, chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	done := make(chan struct{})
	m := &Machine{pid: pid, done: done}

	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		})
	})
	return m, done
}

// markIntentionalKill records the state a real Stop leaves behind for a self-inflicted kill.
func markIntentionalKill(m *Machine) {
	m.stopping = true
	m.waitErr = errors.New("signal: killed")
}

func TestStopUnstartedMachineIsNoop(t *testing.T) {
	m := &Machine{}
	require.NoError(t, m.Stop(context.Background()))
}

func TestStopAlreadyDoneIsNoop(t *testing.T) {
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	close(done)
	require.NoError(t, m.stopWith(context.Background(), time.Second, time.Second))
}

func TestStopPromptGracefulExit(t *testing.T) {
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	go func() { time.Sleep(5 * time.Millisecond); close(done) }()

	start := time.Now()
	require.NoError(t, m.stopWith(context.Background(), 2*time.Second, 2*time.Second))
	require.Less(t, time.Since(start), time.Second, "prompt exit must not wait out the windows")
}

// TestStopRealChildEscalatesToSIGKILL: the real child ignores TERM, so the only way this passes
// is the forced kill. The assertion is on the terminating SIGNAL rather than on the word
// "signal" appearing in an error — a TERM death would satisfy that weaker check.
func TestStopRealChildEscalatesToSIGKILL(t *testing.T) {
	c, m := startTermIgnoringChild(t)

	start := time.Now()
	require.NoError(t, m.stopWith(context.Background(), 100*time.Millisecond, 5*time.Second))
	require.Less(t, time.Since(start), 4*time.Second)

	c.join(t, 2*time.Second)
	require.Equal(t, syscall.SIGKILL, c.signal(t), "child ignored TERM, so it must die by KILL")
	require.ErrorIs(t, syscall.Kill(c.pid, 0), syscall.ESRCH, "process must be gone")
}

// TestStopTermIgnoringChildOnGracePath: the grace path must also reach a forced kill.
func TestStopTermIgnoringChildOnGracePath(t *testing.T) {
	c, m := startTermIgnoringChild(t)

	require.NoError(t, m.stopWith(context.Background(), 100*time.Millisecond, 5*time.Second))
	c.join(t, 2*time.Second)
	require.Equal(t, syscall.SIGKILL, c.signal(t))
}

// TestStopCancellationDuringGraceKillsBeforeGraceExpires is load-bearing: the child never
// completes on its own, so success is only possible if the cancellation branch killed it. If
// cancellation were ignored, this would wait out the 10s grace and the elapsed assertion would
// fail.
func TestStopCancellationDuringGraceKillsBeforeGraceExpires(t *testing.T) {
	c, m := startTermIgnoringChild(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	require.NoError(t, m.stopWith(ctx, 10*time.Second, 5*time.Second))
	elapsed := time.Since(start)

	require.Less(t, elapsed, 3*time.Second, "grace must not be waited out once ctx expires")
	require.GreaterOrEqual(t, elapsed, 150*time.Millisecond)
	c.join(t, 2*time.Second)
	require.Equal(t, syscall.SIGKILL, c.signal(t))
}

// TestStopWaitsPastFormerConfirmationWindow (POLICY): completion lands at 2.2s, after the former
// 2s confirmation bound. The controlled channel is what places completion precisely.
func TestStopWaitsPastFormerConfirmationWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~2.2s by construction")
	}
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	go func() { time.Sleep(2200 * time.Millisecond); close(done) }()

	start := time.Now()
	require.NoError(t, m.stopWith(context.Background(), 20*time.Millisecond, 5*time.Second))
	require.GreaterOrEqual(t, time.Since(start), 2*time.Second)
}

func TestStopConfirmationHonoursWindow(t *testing.T) {
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	go func() { time.Sleep(300 * time.Millisecond); close(done) }()

	start := time.Now()
	require.NoError(t, m.stopWith(context.Background(), 20*time.Millisecond, 5*time.Second))
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, 250*time.Millisecond)
	require.Less(t, elapsed, 3*time.Second)
}

// TestStopConfirmationWindowIsLoadBearing is the negative control: the same completion timing
// with a shorter window MUST fail. Otherwise a suite that simply waits long enough everywhere
// would be indistinguishable from a working one.
func TestStopConfirmationWindowIsLoadBearing(t *testing.T) {
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	go func() { time.Sleep(600 * time.Millisecond); close(done) }()

	err := m.stopWith(context.Background(), 20*time.Millisecond, 300*time.Millisecond)
	require.Error(t, err, "a completion after the window must be reported as unconfirmed")
	require.Contains(t, err.Error(), "timed out")
}

// TestStopFailsClosedWhenNeverReaped pins the safety property: an unconfirmed termination is an
// ERROR, never success, however long we waited.
func TestStopFailsClosedWhenNeverReaped(t *testing.T) {
	m, _ := controlledChannelMachine(t)
	markIntentionalKill(m)

	start := time.Now()
	err := m.stopWith(context.Background(), 10*time.Millisecond, 60*time.Millisecond)
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Contains(t, err.Error(), "stop qemu")
	require.Contains(t, err.Error(), "timed out")
	require.GreaterOrEqual(t, elapsed, 60*time.Millisecond)
	require.Less(t, elapsed, 2*time.Second, "must stay bounded by the window")
}

// TestStopProductionGracePathWaitsForLateCompletion binds the SHIPPED constant through the
// public Stop(). Completion at grace+2.5s is past the former 2s bound. Reverting stopConfirm to
// 2s makes this fail (mutation-verified).
func TestStopProductionGracePathWaitsForLateCompletion(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~7.6s by construction (real stopGrace + late completion)")
	}
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	go func() { time.Sleep(stopGrace + 2500*time.Millisecond); close(done) }()

	start := time.Now()
	require.NoError(t, m.Stop(context.Background()))
	require.GreaterOrEqual(t, time.Since(start), stopGrace+2*time.Second)
}

// TestStopProductionCtxPathWaitsForLateCompletion binds the shipped constant on the
// already-cancelled path, where the kill is immediate. Completion at 2.5s is past the former 2s
// bound. Reverting stopConfirm to 2s makes this fail (mutation-verified).
func TestStopProductionCtxPathWaitsForLateCompletion(t *testing.T) {
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go func() { time.Sleep(2500 * time.Millisecond); close(done) }()

	require.NoError(t, m.Stop(ctx))
}

// TestStopCancellationDuringConfirmationDoesNotShortenTheWindow pins the approved behaviour: once
// the completion window is entered, an expiring caller context does not cut it short.
func TestStopCancellationDuringConfirmationDoesNotShortenTheWindow(t *testing.T) {
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { time.Sleep(2500 * time.Millisecond); close(done) }()

	require.NoError(t, m.stopWith(ctx, 20*time.Millisecond, 5*time.Second))
}

func TestStopAlreadyCancelledContextFailsClosed(t *testing.T) {
	m, _ := controlledChannelMachine(t)
	markIntentionalKill(m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := m.stopWith(ctx, time.Second, 60*time.Millisecond)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestStopAlreadyCancelledContextStillWaitsForCompletion(t *testing.T) {
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go func() { time.Sleep(20 * time.Millisecond); close(done) }()

	require.NoError(t, m.stopWith(ctx, time.Second, 2*time.Second))
}

// TestStopUnexpectedWaitErrorPropagates guards the other safety direction: a genuine crash must
// not be laundered into success by the stop path.
func TestStopUnexpectedWaitErrorPropagates(t *testing.T) {
	m, done := controlledChannelMachine(t)
	m.stopping = true
	m.waitErr = errors.New("exit status 1")
	close(done)

	err := m.stopWith(context.Background(), time.Second, time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exit status 1")
}

// TestConfirmExitCompletionAtBoundary pins the select-priority fix: a completion already visible
// when the timer fires must be honoured, not reported as a timeout.
func TestConfirmExitCompletionAtBoundary(t *testing.T) {
	m, done := controlledChannelMachine(t)
	markIntentionalKill(m)
	close(done)

	require.NoError(t, m.confirmExit(time.Nanosecond, errors.New("timed out")))
}

func TestConfirmExitTimeoutReturnsTimeoutError(t *testing.T) {
	m, _ := controlledChannelMachine(t)
	sentinel := errors.New("timed out")
	err := m.confirmExit(20*time.Millisecond, sentinel)
	require.ErrorIs(t, err, sentinel)
}
