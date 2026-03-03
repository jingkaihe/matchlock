//go:build darwin

package darwin

import (
	"errors"
	"os"
	"sync"
	"syscall"
)

type SocketPair struct {
	hostFD  int
	guestFD int
	hostF   *os.File
	guestF  *os.File
	closed  bool
	mu      sync.Mutex
}

func createSocketPair() (*SocketPair, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, err
	}

	if err := syscall.SetNonblock(fds[0], true); err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return nil, err
	}
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return nil, err
	}

	return &SocketPair{
		hostFD:  fds[0],
		guestFD: fds[1],
		hostF:   os.NewFile(uintptr(fds[0]), "host-net"),
		guestF:  os.NewFile(uintptr(fds[1]), "guest-net"),
	}, nil
}

func (sp *SocketPair) HostFD() int {
	return sp.hostFD
}

func (sp *SocketPair) GuestFD() int {
	return sp.guestFD
}

func (sp *SocketPair) HostFile() *os.File {
	return sp.hostF
}

func (sp *SocketPair) GuestFile() *os.File {
	return sp.guestF
}

func (sp *SocketPair) Close() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.closed {
		return nil
	}
	sp.closed = true
	var errs []error
	if sp.hostF != nil {
		if err := sp.hostF.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, err)
		}
		sp.hostF = nil
	}
	if sp.guestF != nil {
		if err := sp.guestF.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, err)
		}
		sp.guestF = nil
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
