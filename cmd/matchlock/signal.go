package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// contextWithSignal returns a context that is cancelled when SIGINT or SIGTERM
// is received. The returned stop function should be deferred to clean up the
// signal handler.
func contextWithSignal(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
		signal.Stop(sigCh)
	}()
	return ctx, cancel
}

// watchTerminalResize listens for SIGWINCH and sends the new terminal
// size (rows, cols) to the returned channel. Call stop to clean up.
func watchTerminalResize(fd int) (resizeCh <-chan [2]uint16, stop func()) {
	ch := make(chan [2]uint16, 1)
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			if c, r, err := term.GetSize(fd); err == nil {
				select {
				case ch <- [2]uint16{uint16(r), uint16(c)}:
				default:
				}
			}
		}
	}()
	return ch, func() {
		signal.Stop(winchCh)
		close(ch)
	}
}
