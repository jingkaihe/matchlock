package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- CopyLogFile(ctx, path, &out, true)
	}()

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
