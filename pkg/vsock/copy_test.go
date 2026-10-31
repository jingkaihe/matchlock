package vsock

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
)

func TestCopyOutReportsGuestSkips(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	go func() {
		header := make([]byte, 5)
		_, _ = ReadFull(guest, header)
		_, _ = ReadFull(guest, make([]byte, binary.BigEndian.Uint32(header[1:])))
		_ = SendMessage(guest, MsgTypeCopyInfo, []byte(`{"name":"w","is_dir":true}`))
		_ = SendMessage(guest, MsgTypeStderr, copyarchive.EncodeSkip("w/fifo", "unsupported file type"))
		_ = SendMessage(guest, MsgTypeStdout, []byte("archive"))
		_ = SendMessage(guest, MsgTypeFileResult, []byte(`{}`))
	}()

	var skipped []string
	_, archive, err := CopyOut(context.Background(), host, copyarchive.CopyOutRequest{
		Path:   "/w",
		OnSkip: func(name, _ string) { skipped = append(skipped, name) },
	})
	require.NoError(t, err)
	defer archive.Close()
	data, err := io.ReadAll(archive)
	require.NoError(t, err)
	assert.Equal(t, "archive", string(data))
	assert.Equal(t, []string{"w/fifo"}, skipped)
}

// TestCopyOutRejectsMaliciousGuest checks that guest replies cannot name an
// unsafe archive root or make the host allocate an oversized frame.
func TestCopyOutRejectsMaliciousGuest(t *testing.T) {
	oversized := make([]byte, 5)
	oversized[0] = MsgTypeCopyInfo
	binary.BigEndian.PutUint32(oversized[1:], maxCopyFrameSize+1)

	tests := []struct {
		name    string
		reply   func(conn net.Conn)
		wantErr error
	}{
		{
			name:    "unsafe source name",
			reply:   func(conn net.Conn) { _ = SendMessage(conn, MsgTypeCopyInfo, []byte(`{"name":"..","is_dir":true}`)) },
			wantErr: copyarchive.ErrArchive,
		},
		{
			name:    "oversized frame",
			reply:   func(conn net.Conn) { _, _ = conn.Write(oversized) },
			wantErr: ErrCopyFrameTooLarge,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, guest := net.Pipe()
			defer guest.Close()
			go func() {
				// Read the request, then send the malicious reply.
				header := make([]byte, 5)
				_, _ = ReadFull(guest, header)
				_, _ = ReadFull(guest, make([]byte, binary.BigEndian.Uint32(header[1:])))
				tt.reply(guest)
			}()

			_, _, err := CopyOut(context.Background(), host, copyarchive.CopyOutRequest{Path: "/x"})
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}
