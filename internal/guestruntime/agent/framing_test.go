//go:build linux

package guestagent

import (
	"bytes"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendMessageConcurrentFramesDoNotInterleave(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	writer, reader := fds[0], fds[1]
	t.Cleanup(func() { syscall.Close(writer); syscall.Close(reader) })

	const senders, framesPerSender = 4, 200
	var wg sync.WaitGroup
	for sender := 0; sender < senders; sender++ {
		wg.Add(1)
		go func(fill byte) {
			defer wg.Done()
			for i := 0; i < framesPerSender; i++ {
				// Large payloads fill the socket buffer and force partial writes.
				sendMessage(writer, MsgTypeStdout+fill%2, bytes.Repeat([]byte{fill}, 1+(i*997)%65536))
			}
		}(byte('a' + sender))
	}
	writesDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(writesDone)
	}()

	for frame := 0; frame < senders*framesPerSender; frame++ {
		msgType, data, err := readMessage(reader)
		require.NoError(t, err)
		require.NotEmpty(t, data)
		assert.Equal(t, MsgTypeStdout+data[0]%2, msgType, "frame %d has a mismatched header", frame)
		require.Equal(t, bytes.Repeat(data[:1], len(data)), data, "frame %d payload interleaved with another frame", frame)
	}
	<-writesDone
}
