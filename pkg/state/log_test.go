package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockedBuffer is a bytes.Buffer whose Write and String are mutex-guarded: the
// race detector flagged the snapshot read performed by require.Eventually's
// goroutine against CopyLogFile's concurrent write. The lock exists so this
// test's own polling is race-clean; no product code changes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestReadLogFileMissingReturnsEmpty(t *testing.T) {
	t.Parallel()

	data, err := ReadLogFile(filepath.Join(t.TempDir(), "missing.log"))
	require.NoError(t, err)
	assert.Nil(t, data)
}

func TestCopyLogFileFollow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "vm.log")
	require.NoError(t, os.WriteFile(path, []byte("first\n"), 0644))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out lockedBuffer
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		done <- CopyLogFile(ctx, path, &out, true)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("CopyLogFile did not return after cancellation")
		}
	})

	require.Eventually(t, func() bool {
		return out.String() == "first\n"
	}, 2*time.Second, 20*time.Millisecond)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	require.NoError(t, err)
	_, err = f.WriteString("second\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.Eventually(t, func() bool {
		return out.String() == "first\nsecond\n"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}
