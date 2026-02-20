package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
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
