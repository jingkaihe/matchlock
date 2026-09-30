package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/kernel"
)

// kernelPullTimeout bounds a single kernel pull so a stalled registry download
// can never hang `matchlock setup`. It is applied to every runKernelPull call.
const kernelPullTimeout = 5 * time.Minute

// kernelEnsureFn is the injectable seam used to warm the kernel cache. It
// defaults to the real Manager method and is substituted in tests so error and
// cancellation paths can be exercised without any network I/O.
var kernelEnsureFn = (*kernel.Manager).EnsureKernel

var kernelCmd = &cobra.Command{
	Use:   "kernel",
	Short: "Manage cached guest kernels",
}

var kernelLsCmd = &cobra.Command{
	Use:     "ls",
	Aliases: []string{"list"},
	Short:   "List cached guest kernels",
	RunE:    runKernelLs,
}

var kernelRmCmd = &cobra.Command{
	Use:   "rm [version]",
	Short: "Remove a cached guest kernel",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runKernelRm,
}

var kernelPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Download and cache a guest kernel without booting a VM",
	Args:  cobra.NoArgs,
	RunE:  runKernelPullCmd,
}

func init() {
	kernelLsCmd.Flags().Bool("json", false, "Output machine-readable JSON")
	kernelRmCmd.Flags().Bool("all", false, "Remove all cached kernels")
	kernelRmCmd.Flags().String("ref", "", "Remove a cached kernel by OCI reference")
	kernelPullCmd.Flags().String("version", "", "Kernel version to pull (defaults to the built-in version)")

	kernelCmd.AddCommand(kernelLsCmd)
	kernelCmd.AddCommand(kernelRmCmd)
	kernelCmd.AddCommand(kernelPullCmd)
	rootCmd.AddCommand(kernelCmd)
}

func runKernelPullCmd(cmd *cobra.Command, args []string) error {
	version, _ := cmd.Flags().GetString("version")
	ctx := cmd.Context()
	if ctx == nil {
		// cobra always injects a context during Execute; guard direct RunE calls.
		ctx = context.Background()
	}
	return runKernelPull(ctx, kernel.NewManager(), version)
}

// runKernelPull warms the local kernel cache for the given version (or the
// built-in default when version is empty) and prints the resolved path. A
// kernel that is already present is an idempotent no-op. The pull is bounded by
// kernelPullTimeout and any failure is wrapped in ErrPullKernel so callers can
// match it with errors.Is and see the offending registry reference.
func runKernelPull(ctx context.Context, mgr *kernel.Manager, version string) error {
	arch := kernel.CurrentArch()
	if version == "" {
		version = kernel.Version
	}

	ctx, cancel := context.WithTimeout(ctx, kernelPullTimeout)
	defer cancel()

	path, err := kernelEnsureFn(mgr, ctx, arch, version)
	if err != nil {
		return errx.With(ErrPullKernel, ": %s: %w", kernel.ImageReference(version), err)
	}

	fmt.Printf("kernel %s (%s): %s\n", version, arch, path)
	return nil
}

func runKernelLs(cmd *cobra.Command, args []string) error {
	jsonOutput, _ := cmd.Flags().GetBool("json")
	mgr := kernel.NewManager()
	entries, err := mgr.List()
	if err != nil {
		return err
	}

	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(entries)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "IDENTIFIER\tARCH\tSIZE")
	for _, entry := range entries {
		identifier := entry.SourceRef
		if identifier == "" {
			identifier = entry.Version
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n",
			identifier,
			entry.Arch,
			humanizeMB(entry.Size),
		)
	}
	w.Flush()
	return nil
}

func runKernelRm(cmd *cobra.Command, args []string) error {
	removeAll, _ := cmd.Flags().GetBool("all")
	ref, _ := cmd.Flags().GetString("ref")
	mgr := kernel.NewManager()

	switch {
	case removeAll:
		if len(args) != 0 || ref != "" {
			return fmt.Errorf("--all cannot be combined with a version argument or --ref")
		}
		if err := mgr.RemoveAll(); err != nil {
			return err
		}
		fmt.Println("Removed all cached kernels")
		return nil
	case ref != "":
		if len(args) != 0 {
			return fmt.Errorf("--ref cannot be combined with a version argument")
		}
		if err := mgr.RemoveRef(ref); err != nil {
			return err
		}
		fmt.Printf("Removed cached kernel ref %s\n", ref)
		return nil
	default:
		if len(args) != 1 {
			return fmt.Errorf("accepts 1 arg(s), received %d", len(args))
		}
		version := args[0]
		if err := mgr.RemoveVersion(version); err != nil {
			return err
		}
		fmt.Printf("Removed cached kernel %s\n", version)
		return nil
	}
}
