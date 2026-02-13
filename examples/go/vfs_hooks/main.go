package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/sdk"
)

var (
	errCreateClient     = errors.New("create client")
	errLaunchSandbox    = errors.New("launch sandbox")
	errWriteMutatedFile = errors.New("write mutated file")
	errReadMutatedFile  = errors.New("read mutated file")
	errWriteTriggerFile = errors.New("write trigger file")
	errExecHookRuns     = errors.New("exec hook runs")
	errReadHookLog      = errors.New("read hook log")
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

	sandbox := sdk.New("alpine:latest").WithVFSInterception(&sdk.VFSInterceptionConfig{
		MaxExecDepth: 1,
		Rules: []sdk.VFSHookRule{
			{
				Name:   "block-create",
				Phase:  "before",
				Ops:    []string{"create"},
				Path:   "/workspace/blocked.txt",
				Action: "block",
			},
			{
				Name:   "mutate-write",
				Phase:  "before",
				Ops:    []string{"write"},
				Path:   "/workspace/mutated.txt",
				Action: "mutate_write",
				Data:   "mutated-by-hook",
			},
			{
				Name:      "audit-after-write",
				Phase:     "after",
				Ops:       []string{"write"},
				Path:      "/workspace/*",
				TimeoutMS: 2000,
				Hook: func(ctx context.Context, hookClient *sdk.Client) error {
					_, err := hookClient.Exec(ctx, "echo 1 >> /tmp/hook_runs; if [ ! -f /workspace/hook.log ]; then echo hook > /workspace/hook.log; fi")
					return err
				},
			},
		},
	})

	vmID, err := client.Launch(sandbox)
	if err != nil {
		return errx.Wrap(errLaunchSandbox, err)
	}
	slog.Info("sandbox ready", "vm", vmID)

	ctx := context.Background()
	_, _ = client.Exec(ctx, "rm -f /tmp/hook_runs /workspace/hook.log /workspace/blocked.txt /workspace/mutated.txt /workspace/trigger.txt")

	if err := client.WriteFile(ctx, "/workspace/blocked.txt", []byte("blocked")); err != nil {
		fmt.Printf("blocked write rejected as expected: %v\n", err)
	} else {
		fmt.Println("blocked write unexpectedly succeeded")
	}

	if err := client.WriteFile(ctx, "/workspace/mutated.txt", []byte("original-content")); err != nil {
		return errx.Wrap(errWriteMutatedFile, err)
	}

	mutated, err := client.ReadFile(ctx, "/workspace/mutated.txt")
	if err != nil {
		return errx.Wrap(errReadMutatedFile, err)
	}
	fmt.Printf("mutated file content: %q\n", strings.TrimSpace(string(mutated)))

	if err := client.WriteFile(ctx, "/workspace/trigger.txt", []byte("trigger")); err != nil {
		return errx.Wrap(errWriteTriggerFile, err)
	}

	time.Sleep(400 * time.Millisecond)

	runsResult, err := client.Exec(ctx, "if [ -f /tmp/hook_runs ]; then wc -l < /tmp/hook_runs; else echo 0; fi")
	if err != nil {
		return errx.Wrap(errExecHookRuns, err)
	}
	fmt.Printf("hook exec runs: %s", runsResult.Stdout)

	hookLog, err := client.ReadFile(ctx, "/workspace/hook.log")
	if err != nil {
		return errx.Wrap(errReadHookLog, err)
	}
	fmt.Printf("hook log content: %q\n", strings.TrimSpace(string(hookLog)))

	return nil
}
