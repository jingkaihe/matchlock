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
		Rules: []sdk.VFSHookRule{
			{
				Name:   "host-block-create",
				Phase:  sdk.VFSHookPhaseBefore,
				Ops:    []sdk.VFSHookOp{sdk.VFSHookOpCreate},
				Path:   "/workspace/blocked-create.txt",
				Action: sdk.VFSHookActionBlock,
			},
			{
				Name:  "sdk-block-write",
				Phase: sdk.VFSHookPhaseBefore,
				Ops:   []sdk.VFSHookOp{sdk.VFSHookOpWrite},
				Path:  "/workspace/blocked-write.txt",
				ActionHook: func(ctx context.Context, req sdk.VFSActionRequest) sdk.VFSHookAction {
					return sdk.VFSHookActionBlock
				},
			},
			{
				Name:  "mutate-write",
				Phase: sdk.VFSHookPhaseBefore,
				Ops:   []sdk.VFSHookOp{sdk.VFSHookOpWrite},
				Path:  "/workspace/mutated.txt",
				MutateHook: func(ctx context.Context, req sdk.VFSMutateRequest) ([]byte, error) {
					payload := fmt.Sprintf(
						"mutated-by-hook size=%d mode=%#o uid=%d gid=%d",
						req.Size, req.Mode, req.UID, req.GID,
					)
					return []byte(payload), nil
				},
			},
			{
				Name:      "audit-after-write",
				Phase:     sdk.VFSHookPhaseAfter,
				Ops:       []sdk.VFSHookOp{sdk.VFSHookOpWrite},
				Path:      "/workspace/trigger.txt",
				TimeoutMS: 2000,
				DangerousHook: func(ctx context.Context, hookClient *sdk.Client, event sdk.VFSHookEvent) error {
					line := fmt.Sprintf(
						"op=%s path=%s size=%d mode=%#o uid=%d gid=%d",
						event.Op, event.Path, event.Size, event.Mode, event.UID, event.GID,
					)
					cmd := fmt.Sprintf("echo 1 >> /tmp/hook_runs; printf '%%s\\n' %q >> /workspace/hook.log", line)
					_, err := hookClient.Exec(ctx, cmd)
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
	_, _ = client.Exec(ctx, "rm -f /tmp/hook_runs /workspace/hook.log /workspace/blocked-create.txt /workspace/blocked-write.txt /workspace/mutated.txt /workspace/trigger.txt")

	if err := client.WriteFile(ctx, "/workspace/blocked-create.txt", []byte("blocked")); err != nil {
		fmt.Printf("host create block rejected as expected: %v\n", err)
	} else {
		fmt.Println("host create block unexpectedly succeeded")
	}

	if err := client.WriteFile(ctx, "/workspace/blocked-write.txt", []byte("blocked")); err != nil {
		fmt.Printf("local write block rejected as expected: %v\n", err)
	} else {
		fmt.Println("local write block unexpectedly succeeded")
	}

	if err := client.WriteFileMode(ctx, "/workspace/mutated.txt", []byte("original-content"), 0640); err != nil {
		return errx.Wrap(errWriteMutatedFile, err)
	}

	mutated, err := client.ReadFile(ctx, "/workspace/mutated.txt")
	if err != nil {
		return errx.Wrap(errReadMutatedFile, err)
	}
	fmt.Printf("mutated file content: %q\n", strings.TrimSpace(string(mutated)))

	if err := client.WriteFileMode(ctx, "/workspace/trigger.txt", []byte("trigger"), 0600); err != nil {
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
