package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCloseContextZeroTimeoutNotImmediatelyDone(t *testing.T) {
	ctx, cancel := closeContext(0)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("expected context not to be done immediately")
	default:
	}
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
