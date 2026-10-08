//go:build linux

package kvm

import (
	"errors"
	"syscall"
	"testing"
)

// testProbe wires an injected open/ioctl/close triple into a Probe and runs it.
func testProbe(t *testing.T, open func(string) (int, error), ioctl func(int, uint) (int, error), close func(int) error) Result {
	t.Helper()
	p := &Probe{open: open, ioctl: ioctl, close: close}
	if close == nil {
		p.close = func(int) error { return nil }
	}
	return p.Check()
}

func TestAvailable(t *testing.T) {
	var closed []int
	res := testProbe(t,
		func(string) (int, error) { return 3, nil },
		func(fd int, req uint) (int, error) {
			if req == ioctlGetAPIVersion {
				return kvmAPIVersion, nil
			}
			// create_vm -> a non-3 fd that must be closed
			return 7, nil
		},
		func(fd int) error { closed = append(closed, fd); return nil },
	)
	if res.Status != StatusAvailable {
		t.Fatalf("status=%v (%s), want available", res.Status, res)
	}
	if !res.Usable() {
		t.Fatal("Usable()=false for available")
	}
	// Both the control fd (3) and the throwaway VM fd (7) must be closed.
	if got := len(closed); got != 2 {
		t.Fatalf("closed %d fds (%v), want 2", got, closed)
	}
	seen := map[int]bool{}
	for _, fd := range closed {
		seen[fd] = true
	}
	if !seen[3] || !seen[7] {
		t.Fatalf("expected fds 3 and 7 closed, got %v", closed)
	}
}

func testOpenErr(t *testing.T, err error, want Status) Result {
	return testProbe(t,
		func(string) (int, error) { return -1, err },
		nil, nil,
	).withWantOpenErr(t, want)
}

// withWantOpenErr is a tiny helper to keep the table below terse; it just
// asserts the status and returns the result.
func (r Result) withWantOpenErr(t *testing.T, want Status) Result {
	t.Helper()
	if r.Status != want {
		t.Fatalf("open->status=%v (%s), want %v", r.Status, r, want)
	}
	if r.Op != "open" {
		t.Fatalf("Op=%q, want open", r.Op)
	}
	if r.Err == nil {
		t.Fatal("Err is nil for an open failure")
	}
	return r
}

func TestClassifyOpen(t *testing.T) {
	cases := []struct {
		err  error
		want Status
	}{
		{syscall.ENOENT, StatusNotPresent},
		{syscall.ENXIO, StatusNotPresent},
		{syscall.ENODEV, StatusNotPresent},
		{syscall.EACCES, StatusPermissionDenied},
		{syscall.EPERM, StatusPermissionDenied},
		{errors.New("boom"), StatusUnexpected},
	}
	for _, c := range cases {
		res := testProbe(t,
			func(string) (int, error) { return -1, c.err },
			nil, nil,
		)
		if res.Status != c.want {
			t.Fatalf("open err=%v -> %v (%s), want %v", c.err, res.Status, res, c.want)
		}
	}
}

func TestAPIVersionMismatch(t *testing.T) {
	res := testProbe(t,
		func(string) (int, error) { return 3, nil },
		func(fd int, req uint) (int, error) { return kvmAPIVersion + 1, nil },
		nil,
	)
	if res.Status != StatusIncompatible {
		t.Fatalf("status=%v (%s), want Incompatible", res.Status, res)
	}
	if res.Op != "get_api_version" {
		t.Fatalf("Op=%q, want get_api_version", res.Op)
	}
}

func TestIoctlErrClass(t *testing.T) {
	cases := []struct {
		err  error
		want Status
	}{
		{syscall.EACCES, StatusPermissionDenied},
		{syscall.EPERM, StatusPermissionDenied},
		{syscall.ENOTTY, StatusIncompatible},
		{syscall.ENODEV, StatusIncompatible},
		{syscall.EOPNOTSUPP, StatusIncompatible},
		{syscall.EINVAL, StatusIncompatible},
		{errors.New("x"), StatusUnexpected},
	}
	for _, c := range cases {
		res := testProbe(t,
			func(string) (int, error) { return 3, nil },
			func(fd int, req uint) (int, error) { return 0, c.err },
			nil,
		)
		if res.Status != c.want {
			t.Fatalf("ioctl err=%v -> %v (%s), want %v", c.err, res.Status, res, c.want)
		}
	}
}

func TestCreateVMFailureKinds(t *testing.T) {
	// API version OK, then create_vm fails.
	mk := func(err error) Result {
		return testProbe(t,
			func(string) (int, error) { return 3, nil },
			func(fd int, req uint) (int, error) {
				if req == ioctlGetAPIVersion {
					return kvmAPIVersion, nil
				}
				return 0, err
			},
			nil,
		)
	}

	if r := mk(syscall.ENODEV); r.Status != StatusUnusable {
		t.Fatalf("ENODEV -> %v, want Unusable", r)
	}
	// EBUSY is transient resource pressure, not absent hardware.
	if r := mk(syscall.EBUSY); r.Status != StatusUnexpected {
		t.Fatalf("EBUSY -> %v, want Unexpected", r)
	}
	if r := mk(syscall.EACCES); r.Status != StatusPermissionDenied {
		t.Fatalf("EACCES -> %v, want PermissionDenied", r)
	}
	// Transient resource exhaustion should be distinguishable, not mislabeled
	// as absent hardware.
	if r := mk(syscall.ENOMEM); r.Status != StatusUnexpected {
		t.Fatalf("ENOMEM -> %v, want Unexpected", r)
	}
	if r := mk(syscall.EMFILE); r.Status != StatusUnexpected {
		t.Fatalf("EMFILE -> %v, want Unexpected", r)
	}
}
