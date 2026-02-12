package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/state"
)

var execCmd = &cobra.Command{
	Use:   "exec [flags] <id> -- <command>",
	Short: "Execute a command in a running sandbox",
	Long: `Execute a command in a running sandbox.

The sandbox must have been started with --rm=false to remain running.`,
	Example: `  matchlock exec vm-abc123 echo hello
  matchlock exec vm-abc123 -it sh`,
	Args: cobra.MinimumNArgs(1),
	RunE: runExec,
}

func init() {
	execCmd.Flags().BoolP("tty", "t", false, "Allocate a pseudo-TTY")
	execCmd.Flags().BoolP("interactive", "i", false, "Keep STDIN open")
	execCmd.Flags().StringP("workdir", "w", "", "Working directory inside the sandbox (default: workspace path)")
	execCmd.Flags().StringP("user", "u", "", "Run as user (uid, uid:gid, or username)")

	rootCmd.AddCommand(execCmd)
}

func runExec(cmd *cobra.Command, args []string) error {
	vmID := args[0]
	cmdArgs := args[1:]

	tty, _ := cmd.Flags().GetBool("tty")
	interactive, _ := cmd.Flags().GetBool("interactive")
	workdir, _ := cmd.Flags().GetString("workdir")
	user, _ := cmd.Flags().GetString("user")
	interactiveMode := tty && interactive

	if len(cmdArgs) == 0 && !interactiveMode {
		return fmt.Errorf("command required (or use -it for interactive mode)")
	}

	mgr := state.NewManager()
	vmState, err := mgr.Get(vmID)
	if err != nil {
		return fmt.Errorf("VM %s not found: %w", vmID, err)
	}
	if vmState.Status != "running" {
		return fmt.Errorf("VM %s is not running (status: %s)", vmID, vmState.Status)
	}

	execSocketPath := mgr.ExecSocketPath(vmID)
	if _, err := os.Stat(execSocketPath); err != nil {
		return fmt.Errorf("exec socket not found for %s (was it started with --rm=false?)", vmID)
	}

	command := api.ShellQuoteArgs(cmdArgs)

	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	if interactiveMode {
		return runExecInteractive(ctx, execSocketPath, command, workdir, user)
	}

	if interactive {
		return runExecPipe(ctx, execSocketPath, command, workdir, user)
	}

	result, err := sandbox.ExecViaRelay(ctx, execSocketPath, command, workdir, user)
	if err != nil {
		return fmt.Errorf("exec failed: %w", err)
	}

	os.Stdout.Write(result.Stdout)
	os.Stderr.Write(result.Stderr)
	os.Exit(result.ExitCode)
	return nil
}

func runExecPipe(ctx context.Context, execSocketPath, command, workdir, user string) error {
	exitCode, err := sandbox.ExecPipeViaRelay(ctx, execSocketPath, command, workdir, user, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return fmt.Errorf("pipe exec failed: %w", err)
	}
	os.Exit(exitCode)
	return nil
}

func runExecInteractive(ctx context.Context, execSocketPath, command, workdir, user string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("-it requires a TTY")
	}

	cols, rows, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		rows, cols = 24, 80
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("setting raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	exitCode, err := sandbox.ExecInteractiveViaRelay(ctx, execSocketPath, command, workdir, user, uint16(rows), uint16(cols), os.Stdin, os.Stdout)
	if err != nil {
		term.Restore(int(os.Stdin.Fd()), oldState)
		return fmt.Errorf("interactive exec failed: %w", err)
	}

	term.Restore(int(os.Stdin.Fd()), oldState)
	os.Exit(exitCode)
	return nil
}
