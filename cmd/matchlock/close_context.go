package main

import (
	"context"
	"time"
)

// closeContext returns a context for sandbox shutdown.
//
// timeout <= 0 means "graceful default" (no external timeout), allowing backend
// stop logic to apply its built-in grace period before force kill.
func closeContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout > 0 {
		return context.WithTimeout(context.Background(), timeout)
	}
	return context.WithCancel(context.Background())
}
