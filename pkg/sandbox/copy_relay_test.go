package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jingkaihe/matchlock/internal/copyarchive"
)

// TestRelayGuestCopyInDoesNotWaitForInput checks that a copy waiting for
// input, such as an idle stdin, still returns on rejection or cancellation.
func TestRelayGuestCopyInDoesNotWaitForInput(t *testing.T) {
	// Unix socket paths are short, so avoid t.TempDir.
	dir, err := os.MkdirTemp("", "relay")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, data, _ := readRelayMsg(conn)
				var req copyarchive.CopyInRequest
				_ = json.Unmarshal(data, &req)
				if req.Dest == "/rejected" {
					sendRelayCopyResult(conn, errors.New("destination rejected"))
				}
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		dest    string
		wantErr string
	}{
		{name: "rejected", ctx: context.Background(), dest: "/rejected", wantErr: "destination rejected"},
		{name: "cancelled", ctx: ctx, dest: "/w", wantErr: context.Canceled.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin, stdinWriter := io.Pipe()
			t.Cleanup(func() { stdinWriter.Close() })
			done := make(chan error, 1)
			go func() {
				done <- RelayGuest{SocketPath: socketPath}.CopyIn(tt.ctx, copyarchive.CopyInRequest{Dest: tt.dest}, stdin)
			}()
			select {
			case err := <-done:
				assert.ErrorContains(t, err, tt.wantErr)
			case <-time.After(5 * time.Second):
				t.Fatal("copy kept waiting for input")
			}
		})
	}
}
