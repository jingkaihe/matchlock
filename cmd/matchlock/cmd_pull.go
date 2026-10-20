package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/image"
)

// pullTimeoutEnv optionally overrides defaultPullTimeout. It accepts a Go
// duration string such as "30m" or "24h".
const pullTimeoutEnv = "MATCHLOCK_PULL_TIMEOUT"

// defaultPullTimeout bounds a single `matchlock pull`. It is deliberately
// generous — multi-GB images on slow links legitimately take hours — while
// still terminating a stalled registry download.
const defaultPullTimeout = 12 * time.Hour

// pullBuildFn is the injectable seam used to run the image build. It defaults
// to the real builder method and is substituted in tests so the pull wiring
// (timeout resolution, cancellation) is exercised without any network I/O.
var pullBuildFn = (*image.Builder).Build

var pullCmd = &cobra.Command{
	Use:   "pull [flags] <image>",
	Short: "Pull a container image and build rootfs",
	Long: `Pull a container image from a registry and build a rootfs for use with matchlock run.

The pull runs under a 12h deadline. Set the MATCHLOCK_PULL_TIMEOUT environment
variable to a positive Go duration (e.g. "30m", "24h") to override it; an
invalid or non-positive value is rejected with an explicit error.`,
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

// resolvePullTimeout returns the deadline for a single pull. An unset
// MATCHLOCK_PULL_TIMEOUT falls back to defaultPullTimeout; when set, the value
// must parse as a positive Go duration, otherwise the pull fails fast with
// ErrInvalidPullTimeout naming the variable (never a silent fallback).
func resolvePullTimeout() (time.Duration, error) {
	raw, ok := os.LookupEnv(pullTimeoutEnv)
	if !ok {
		return defaultPullTimeout, nil
	}

	timeout, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errx.With(ErrInvalidPullTimeout, ": %s=%q: %w", pullTimeoutEnv, raw, err)
	}
	if timeout <= 0 {
		return 0, errx.With(ErrInvalidPullTimeout, ": %s=%q: must be a positive Go duration (e.g. \"30m\", \"24h\")", pullTimeoutEnv, raw)
	}
	return timeout, nil
}

func runPull(cmd *cobra.Command, args []string) error {
	force, _ := cmd.Flags().GetBool("force")
	tag, _ := cmd.Flags().GetString("tag")

	timeout, err := resolvePullTimeout()
	if err != nil {
		return err
	}

	imageRef := args[0]
	builder := image.NewBuilder(&image.BuildOptions{
		ForcePull: force,
	})

	ctx := cmd.Context()
	if ctx == nil {
		// cobra always injects a context during Execute; guard direct RunE calls.
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, cancel = contextWithSignal(ctx)
	defer cancel()

	fmt.Printf("Pulling %s...\n", imageRef)
	result, err := pullBuildFn(builder, ctx, imageRef)
	if err != nil {
		return err
	}

	if tag != "" {
		if err := builder.SaveTag(tag, result); err != nil {
			return errx.Wrap(ErrSaveTag, err)
		}
		fmt.Printf("Tagged: %s\n", tag)
	}

	fmt.Printf("Digest: %s\n", result.Digest)
	fmt.Printf("Size: %.1f MB\n", float64(result.Size)/(1024*1024))
	return nil
}
