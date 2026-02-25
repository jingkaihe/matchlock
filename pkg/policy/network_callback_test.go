package policy

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/stretchr/testify/require"
)

func TestUnixNetworkHookInvoker_DoesNotUseFixedTwoSecondDeadline(t *testing.T) {
	socketPath, errCh := startTestNetworkHookServer(t, func(conn net.Conn) error {
		dec := json.NewDecoder(conn)
		var req api.NetworkHookCallbackRequest
		if err := dec.Decode(&req); err != nil {
			return err
		}

		time.Sleep(2200 * time.Millisecond)
		return json.NewEncoder(conn).Encode(api.NetworkHookCallbackResponse{Action: "allow"})
	})

	invoker := &unixNetworkHookInvoker{socketPath: socketPath}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := invoker.Invoke(ctx, &api.NetworkHookCallbackRequest{
		CallbackID: "cb_after",
		Phase:      "after",
	})
	require.NoError(t, err)
	require.NoError(t, <-errCh)
}

func TestUnixNetworkHookInvoker_RespectsContextDeadline(t *testing.T) {
	socketPath, errCh := startTestNetworkHookServer(t, func(conn net.Conn) error {
		dec := json.NewDecoder(conn)
		var req api.NetworkHookCallbackRequest
		if err := dec.Decode(&req); err != nil {
			return err
		}

		time.Sleep(300 * time.Millisecond)
		return json.NewEncoder(conn).Encode(api.NetworkHookCallbackResponse{Action: "allow"})
	})

	invoker := &unixNetworkHookInvoker{socketPath: socketPath}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := invoker.Invoke(ctx, &api.NetworkHookCallbackRequest{
		CallbackID: "cb_before",
		Phase:      "before",
	})
	require.Error(t, err)

	// Drain server completion to avoid leaking goroutines in tests.
	<-errCh
}

func startTestNetworkHookServer(t *testing.T, handle func(conn net.Conn) error) (string, <-chan error) {
	t.Helper()

	// Keep the Unix socket path short; macOS rejects long socket paths.
	tempDir, err := os.MkdirTemp("", "mlnh-*")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = os.RemoveAll(tempDir)
	})

	socketPath := filepath.Join(tempDir, "s.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			errCh <- acceptErr
			return
		}
		defer conn.Close()
		errCh <- handle(conn)
	}()

	t.Cleanup(func() {
		_ = listener.Close()
	})

	return socketPath, errCh
}
