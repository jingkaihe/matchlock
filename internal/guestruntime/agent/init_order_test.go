//go:build linux

package guestagent

import (
	"os"
	"sync"
	"testing"
	"time"
)

// TestRunServicesBindsExecBeforeReady verifies the invariant that prevents an
// intermittent "connection reset by peer" on a cold sandbox Launch: the exec
// service (port 5000) must be bound BEFORE the ready signal (port 5002) is
// served. The host's waitReady dials 5002 and returns the instant it accepts,
// then immediately dials 5000 via startImageEntrypoint -> Exec; if the 5000
// listener is not in place yet, that dial is reset.
//
// It drives the real runServices with an injected listenFn/acceptFn, so the
// production startup path is exercised (not a reimplementation).
func TestRunServicesBindsExecBeforeReady(t *testing.T) {
	var mu sync.Mutex
	var order []uint32
	execBound := make(chan struct{})
	stop := make(chan struct{})

	listenFn := func(port uint32) (int, error) {
		mu.Lock()
		order = append(order, port)
		mu.Unlock()
		if port == VsockPortExec {
			close(execBound)
		}
		r, w, err := os.Pipe()
		if err != nil {
			return -1, err
		}
		_ = w.Close()
		return int(r.Fd()), nil
	}

	// acceptFn fails immediately so neither accept loop blocks; the injected stop
	// channel terminates both loops so runServices returns cleanly.
	acceptFn := func(fd int) (int, error) {
		_ = fd
		return -1, os.ErrInvalid
	}

	readyFn := func(lf func(uint32) (int, error), af func(int) (int, error), st <-chan struct{}) {
		serveReady(lf, af, st)
	}

	done := make(chan struct{})
	go func() {
		runServices(listenFn, acceptFn, readyFn, stop)
		close(done)
	}()

	// runServices binds exec synchronously before launching readyFn, so exec must
	// be the first recorded bind once it resolves.
	<-execBound
	close(stop)
	<-done

	mu.Lock()
	got := append([]uint32(nil), order...)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no vsock port was bound")
	}
	if got[0] != VsockPortExec {
		t.Fatalf("first bound port = %d, want exec port %d (exec must bind before ready)", got[0], VsockPortExec)
	}
}

// TestRunServicesReadySignalsOnlyAfterExecBind asserts the full ordered loop:
// both the ready listener and the exec loop are served, and the ready listener
// is requested only AFTER the exec port is bound. This is the A/B guard: if a
// future edit reorders ready-before-exec, got[0] becomes VsockPortReady and this
// test fails.
func TestRunServicesReadySignalsOnlyAfterExecBind(t *testing.T) {
	var mu sync.Mutex
	var order []uint32
	readyBound := make(chan struct{})
	stop := make(chan struct{})

	listenFn := func(port uint32) (int, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return -1, err
		}
		_ = w.Close()
		mu.Lock()
		order = append(order, port)
		mu.Unlock()
		if port == VsockPortReady {
			close(readyBound)
		}
		return int(r.Fd()), nil
	}

	readyFn := func(lf func(uint32) (int, error), af func(int) (int, error), st <-chan struct{}) {
		serveReady(lf, af, st)
	}

	done := make(chan struct{})
	go func() {
		runServices(listenFn, func(fd int) (int, error) { _ = fd; return -1, os.ErrInvalid }, readyFn, stop)
		close(done)
	}()

	// Both binds happen in sequence (exec first, then ready); wait for ready to
	// be requested so the order slice is complete, then stop.
	select {
	case <-readyBound:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for ready listener to bind")
	}
	close(stop)
	<-done

	mu.Lock()
	got := append([]uint32(nil), order...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected exec+ready binds, got %v", got)
	}
	if got[0] != VsockPortExec || got[1] != VsockPortReady {
		t.Fatalf("bind order = %v, want [%d %d] (exec first)", got, VsockPortExec, VsockPortReady)
	}
}
