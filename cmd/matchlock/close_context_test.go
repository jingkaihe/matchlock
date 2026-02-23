package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCloseContextZeroTimeoutImmediatelyDone(t *testing.T) {
	ctx, cancel := closeContext(0)
	defer cancel()

	select {
	case <-ctx.Done():
		// ok
	default:
		t.Fatal("expected zero-timeout context to be done")
	}

	require.Error(t, ctx.Err())
}

func TestCloseContextPositiveTimeoutExpires(t *testing.T) {
	ctx, cancel := closeContext(10 * time.Millisecond)
	defer cancel()

	select {
	case <-ctx.Done():
		// ok
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected timeout context to expire")
	}
	require.Error(t, ctx.Err())
}
