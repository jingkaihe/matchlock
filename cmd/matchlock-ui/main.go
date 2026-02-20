package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
)

func main() {
	var host string
	var port int
	var shutdownTimeout time.Duration

	cmd := &cobra.Command{
		Use:           "matchlock-ui",
		Short:         "Run Matchlock management UI",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if port <= 0 || port > 65535 {
				return fmt.Errorf("--port must be between 1 and 65535")
			}
			if shutdownTimeout <= 0 {
				return fmt.Errorf("--shutdown-timeout must be > 0")
			}

			server, err := newUIServer(shutdownTimeout)
			if err != nil {
				return err
			}

			addr := net.JoinHostPort(host, strconv.Itoa(port))
			httpServer := &http.Server{
				Addr:              addr,
				Handler:           server.routes(),
				ReadHeaderTimeout: 5 * time.Second,
			}

			ctx, cancel := contextWithSignal(context.Background())
			defer cancel()

			go func() {
				<-ctx.Done()
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
				defer shutdownCancel()
				_ = httpServer.Shutdown(shutdownCtx)
			}()

			fmt.Fprintf(cmd.ErrOrStderr(), "Matchlock UI listening on http://%s\n", addr)
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return errx.Wrap(ErrUIStartServer, err)
			}

			closeCtx, closeCancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer closeCancel()
			if err := server.Close(closeCtx); err != nil {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Host interface to bind the UI server")
	cmd.Flags().IntVar(&port, "port", 8540, "Port to bind the UI server")
	cmd.Flags().DurationVar(&shutdownTimeout, "shutdown-timeout", 20*time.Second, "Graceful shutdown timeout")

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
