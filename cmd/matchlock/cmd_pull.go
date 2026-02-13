package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/pkg/image"
)

var pullCmd = &cobra.Command{
	Use:   "pull [flags] <image>",
	Short: "Pull a container image and build rootfs",
	Long:  `Pull a container image from a registry and build a rootfs for use with matchlock run.`,
	Example: `  matchlock pull alpine:latest
  matchlock pull -t myapp:latest alpine:latest
  matchlock pull --force alpine:latest`,
	Args: cobra.ExactArgs(1),
	RunE: runPull,
}

func init() {
	pullCmd.Flags().Bool("force", false, "Always pull image from registry (ignore cache)")
	pullCmd.Flags().StringP("tag", "t", "", "Tag the image locally")

	rootCmd.AddCommand(pullCmd)
}

func runPull(cmd *cobra.Command, args []string) error {
	force, _ := cmd.Flags().GetBool("force")
	tag, _ := cmd.Flags().GetString("tag")

	imageRef := args[0]
	builder := image.NewBuilder(&image.BuildOptions{
		ForcePull: force,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Printf("Pulling %s...\n", imageRef)
	result, err := builder.Build(ctx, imageRef)
	if err != nil {
		return err
	}

	if tag != "" {
		if err := builder.SaveTag(tag, result); err != nil {
			return fmt.Errorf("%w: %w", ErrSaveTag, err)
		}
		fmt.Printf("Tagged: %s\n", tag)
	}

	fmt.Printf("Digest: %s\n", result.Digest)
	fmt.Printf("Size: %.1f MB\n", float64(result.Size)/(1024*1024))
	return nil
}
