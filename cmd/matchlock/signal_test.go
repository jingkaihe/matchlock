package main

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchTerminalResizeStopWaitsForWatcher(t *testing.T) {
	getSizeStarted := make(chan struct{})
	releaseGetSize := make(chan struct{})
	var winchCh chan<- os.Signal

	resizeCh, stop := watchTerminalResizeWith(
		0,
		func(int) (int, int, error) {
			close(getSizeStarted)
			<-releaseGetSize
			return 120, 40, nil
		},
		func(ch chan<- os.Signal, _ ...os.Signal) {
			winchCh = ch
		},
		func(chan<- os.Signal) {},
	)
	require.NotNil(t, winchCh)

	winchCh <- syscall.SIGWINCH

	select {
	case <-getSizeStarted:
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for resize watcher")
	}

	stopDone := make(chan struct{})
	go func() {
		stop()
		close(stopDone)
	}()

	close(releaseGetSize)

	select {
	case <-stopDone:
	case <-time.After(1 * time.Second):
		require.Fail(t, "timed out waiting for stop")
	}

	size, ok := <-resizeCh
	if ok {
		assert.Equal(t, [2]uint16{40, 120}, size)
		_, ok = <-resizeCh
	}
	assert.False(t, ok)
}
