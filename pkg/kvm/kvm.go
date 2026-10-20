//go:build linux

// Package kvm probes whether KVM is usable on this Linux host, so the sandbox
// can prefer Firecracker (which requires KVM) and only fall back to another
// backend when KVM is genuinely unavailable.
//
// The probe opens /dev/kvm, checks the KVM API version, and creates and closes
// a throwaway VM descriptor. Creating a disposable VM is a stronger signal of
// usability than merely opening the device, because on many hosts the device
// exists but VM creation fails (e.g. no nested virtualization).
//
// Syscalls are injectable so every failure class is unit-testable without a
// KVM-capable machine. Results preserve the failing operation and underlying
// error so callers can give actionable remediation rather than a bare status.
package kvm

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	// kvmDevice is the KVM control device path.
	kvmDevice = "/dev/kvm"

	// ioctlGetAPIVersion is KVM_GET_API_VERSION (_IO(KVMIO, 0x00)).
	ioctlGetAPIVersion = 0xAE00
	// ioctlCreateVM is KVM_CREATE_VM (_IO(KVMIO, 0x01), flag argument 0).
	ioctlCreateVM = 0xAE01
	// kvmAPIVersion is the KVM API version this driver expects.
	kvmAPIVersion = 12
)

// Status classifies KVM usability.
type Status int

const (
	// StatusAvailable means /dev/kvm opened, API version matched, and a VM was created.
	StatusAvailable Status = iota
	// StatusNotPresent means /dev/kvm does not exist (missing module / no KVM).
	StatusNotPresent
	// StatusPermissionDenied means the device exists but this caller cannot access it.
	StatusPermissionDenied
	// StatusIncompatible means the device opened but the KVM API version differs.
	StatusIncompatible
	// StatusUnusable means the device opened and the API version matched, but a
	// throwaway VM could not be created (often no nested virtualization).
	StatusUnusable
	// StatusUnexpected covers any other ioctl/syscall failure.
	StatusUnexpected
)

// String returns a human-readable label for the status.
func (s Status) String() string {
	switch s {
	case StatusAvailable:
		return "kvm available"
	case StatusNotPresent:
		return "kvm device not present"
	case StatusPermissionDenied:
		return "kvm device present but callers lack permission"
	case StatusIncompatible:
		return "kvm api version incompatible"
	case StatusUnusable:
		return "kvm present but unusable (vm create failed)"
	default:
		return "kvm unexpected error"
	}
}

// Result captures the outcome of a probe: the status, the operation that
// failed (if any), and the underlying error (if any).
type Result struct {
	Status Status
	Op     string // one of "open", "get_api_version", "create_vm"
	Err    error
}

// Usable reports whether KVM is ready to run VMs.
func (r Result) Usable() bool { return r.Status == StatusAvailable }

// String renders a single-line diagnostic.
func (r Result) String() string {
	if r.Err == nil {
		return r.Status.String()
	}
	if r.Op == "" {
		return fmt.Sprintf("%s: %v", r.Status, r.Err)
	}
	return fmt.Sprintf("%s (%s): %v", r.Status, r.Op, r.Err)
}

// Probe runs the KVM check against injectable syscall implementations.
type Probe struct {
	open  func(string) (int, error)
	ioctl func(int, uint) (int, error)
	close func(int) error
}

// NewProbe returns a Probe backed by real syscalls (the default).
func NewProbe() *Probe {
	return &Probe{open: realOpen, ioctl: realIoctl, close: realClose}
}

// Check probes KVM usability using real syscalls.
func Check() Result {
	return NewProbe().Check()
}

// Check runs the probe and returns the full Result.
func (p *Probe) Check() Result {
	p.fillDefaults()
	return p.run()
}

func (p *Probe) fillDefaults() {
	if p.open == nil {
		p.open = realOpen
	}
	if p.ioctl == nil {
		p.ioctl = realIoctl
	}
	if p.close == nil {
		p.close = realClose
	}
}

// run performs the probe. The control device is always closed on every path
// after it is opened; the throwaway VM descriptor is closed immediately after
// creation.
func (p *Probe) run() Result {
	fd, err := p.open(kvmDevice)
	if err != nil {
		return Result{Status: classifyOpen(err), Op: "open", Err: err}
	}
	// Release the control device on all later paths.
	release := func() { _ = p.close(fd) }
	defer release()

	ver, err := p.ioctl(fd, ioctlGetAPIVersion)
	if err != nil {
		return Result{Status: classifyIoctl(err), Op: "get_api_version", Err: err}
	}
	if ver != kvmAPIVersion {
		return Result{Status: StatusIncompatible, Op: "get_api_version", Err: fmt.Errorf("api version %d, want %d", ver, kvmAPIVersion)}
	}

	vmfd, err := p.ioctl(fd, ioctlCreateVM)
	if err != nil {
		return Result{Status: classifyCreate(err), Op: "create_vm", Err: err}
	}
	_ = p.close(vmfd)

	return Result{Status: StatusAvailable}
}

func realOpen(path string) (int, error) {
	return unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
}

func realIoctl(fd int, req uint) (int, error) {
	return unix.IoctlRetInt(fd, req)
}

func realClose(fd int) error {
	return unix.Close(fd)
}

func errnoOf(err error) (syscall.Errno, bool) {
	var e syscall.Errno
	if errors.As(err, &e) {
		return e, true
	}
	return 0, false
}

func classifyOpen(err error) Status {
	if e, ok := errnoOf(err); ok {
		switch e {
		case syscall.ENOENT, syscall.ENXIO, syscall.ENODEV:
			return StatusNotPresent
		case syscall.EACCES, syscall.EPERM:
			return StatusPermissionDenied
		}
	}
	return StatusUnexpected
}

func classifyIoctl(err error) Status {
	if e, ok := errnoOf(err); ok {
		switch e {
		case syscall.EACCES, syscall.EPERM:
			return StatusPermissionDenied
		case syscall.ENOTTY, syscall.ENODEV, syscall.EOPNOTSUPP, syscall.EINVAL:
			return StatusIncompatible
		}
	}
	return StatusUnexpected
}

// classifyCreate distinguishes hardware-absence/resource errors from transient
// resource exhaustion so callers can retry honestly. EBUSY is transient, not a
// hardware-absence signal, so it belongs with the retryable classification.
func classifyCreate(err error) Status {
	if e, ok := errnoOf(err); ok {
		switch e {
		case syscall.EACCES, syscall.EPERM:
			return StatusPermissionDenied
		case syscall.ENODEV, syscall.ENOTTY, syscall.EINVAL, syscall.EOPNOTSUPP:
			return StatusUnusable
		case syscall.ENOMEM, syscall.EMFILE, syscall.ENFILE, syscall.EBUSY:
			return StatusUnexpected
		}
	}
	return StatusUnexpected
}
