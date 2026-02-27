package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sdk"
	"golang.org/x/term"
)

var (
	errCreateClient     = errors.New("create client")
	errLaunchSandbox    = errors.New("launch sandbox")
	errTerminalRequired = errors.New("terminal required")
	errSetRawMode       = errors.New("set raw mode")
	errRestoreTerm      = errors.New("restore terminal mode")
	errExecTTY          = errors.New("exec_tty")
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := sdk.DefaultConfig()
	if os.Getenv("MATCHLOCK_BIN") == "" {
		cfg.BinaryPath = "./bin/matchlock"
	}

	client, err := sdk.NewClient(cfg)
	if err != nil {
		return errx.Wrap(errCreateClient, err)
	}
	defer client.Remove()
	defer client.Close(0)

	sandbox := sdk.New("alpine:latest").WithWorkspace("/workspace").MountMemory("/workspace")

	vmID, err := client.Launch(sandbox)
	if err != nil {
		return errx.Wrap(errLaunchSandbox, err)
	}
	slog.Info("sandbox ready", "vm", vmID)

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errx.With(errTerminalRequired, ": run this example in an interactive terminal")
	}

	ctx := context.Background()
	stdinFD := int(os.Stdin.Fd())

	cols, rows, err := term.GetSize(stdinFD)
	if err != nil {
		rows, cols = 24, 80
	}

	fmt.Println("Connected to sandbox shell. Type 'exit' or press Ctrl-D to quit.")

	oldState, err := term.MakeRaw(stdinFD)
	if err != nil {
		return errx.Wrap(errSetRawMode, err)
	}
	restored := false
	defer func() {
		if !restored {
			_ = term.Restore(stdinFD, oldState)
		}
	}()

	resizeCh := make(chan [2]uint16, 4)
	winchCh := make(chan os.Signal, 1)
	stopResize := make(chan struct{})
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)
	defer close(stopResize)

	go func() {
		for {
			select {
			case <-stopResize:
				return
			case <-winchCh:
				c, r, sizeErr := term.GetSize(stdinFD)
				if sizeErr != nil {
					continue
				}
				select {
				case resizeCh <- [2]uint16{uint16(r), uint16(c)}:
				default:
				}
			}
		}
	}()

	ttyResult, err := client.ExecInteractive(ctx, "sh", &sdk.ExecInteractiveOptions{
		WorkingDir: "/workspace",
		Rows:       uint16(rows),
		Cols:       uint16(cols),
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Resize:     resizeCh,
	})
	if err != nil {
		return errx.Wrap(errExecTTY, err)
	}
	if err := term.Restore(stdinFD, oldState); err != nil {
		return errx.Wrap(errRestoreTerm, err)
	}
	restored = true

	fmt.Printf("\nShell exited: code=%d duration_ms=%d\n", ttyResult.ExitCode, ttyResult.DurationMS)

	return nil
}
