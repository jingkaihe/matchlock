package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/rpc"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
)

var rpcCmd = &cobra.Command{
	Use:   "rpc",
	Short: "Run in RPC mode (for programmatic access)",
	RunE:  runRPC,
}

func init() {
	rootCmd.AddCommand(rpcCmd)
}

func runRPC(cmd *cobra.Command, args []string) error {
	ctx, cancel := contextWithSignal(context.Background())
	defer cancel()

	factory := func(ctx context.Context, config *api.Config) (rpc.VM, error) {
		if config.Image == "" {
			return nil, fmt.Errorf("image is required")
		}

		builder := image.NewBuilder(&image.BuildOptions{})

		result, err := builder.Build(ctx, config.Image)
		if err != nil {
			return nil, fmt.Errorf("failed to build rootfs: %w", err)
		}

		return sandbox.New(ctx, config, &sandbox.Options{RootfsPath: result.RootfsPath})
	}

	return rpc.RunRPC(ctx, factory)
}
