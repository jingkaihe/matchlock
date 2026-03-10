package main

import (
	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/state"
)

var logCmd = &cobra.Command{
	Use:   "log [flags] <id>",
	Short: "Show sandbox logs",
	Long: `Show sandbox logs.

By default this prints the current VM log once. Use -f to follow appended log output.`,
	Args: cobra.ExactArgs(1),
	RunE: runLog,
}

func init() {
	logCmd.Flags().BoolP("follow", "f", false, "Follow log output")
	rootCmd.AddCommand(logCmd)
}

func runLog(cmd *cobra.Command, args []string) error {
	follow, _ := cmd.Flags().GetBool("follow")
	vmID := args[0]

	ctx := context.Background()
	cancel := func() {}
	if follow {
		ctx, cancel = contextWithSignal(context.Background())
	}
	defer cancel()

	mgr := state.NewManager()
	if _, err := mgr.Get(vmID); err != nil {
		return errx.With(ErrVMNotFound, " %s: %w", vmID, err)
	}
	if err := state.CopyLogFile(ctx, mgr.LogPath(vmID), os.Stdout, follow); err != nil {
		if follow && errors.Is(err, context.Canceled) {
			return nil
		}
		return errx.With(ErrReadVMLog, " %s: %w", vmID, err)
	}
	return nil
}
