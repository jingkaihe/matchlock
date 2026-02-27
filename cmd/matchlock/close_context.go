package main

import (
	"context"
	"time"
)

// closeContext returns a context for sandbox shutdown.
func closeContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}
