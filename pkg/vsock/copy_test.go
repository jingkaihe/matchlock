package vsock

import (
	"context"
	"encoding/binary"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
)

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
