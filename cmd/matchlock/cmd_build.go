package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
)

var buildCmd = &cobra.Command{
	Use:   "build [flags] <image-or-context>",
	Short: "Build rootfs from container image or Dockerfile",
	Long: `Build a rootfs from a container image, or build from a Dockerfile using BuildKit-in-VM.

When used with -f/--file, boots a privileged VM with BuildKit to build the Dockerfile.
The build context is the directory argument (defaults to current directory).`,
	Example: `  matchlock build alpine:latest
  matchlock build -t myapp:latest alpine:latest
  matchlock build -f Dockerfile -t myapp:latest .
  matchlock build -f Dockerfile -t myapp:latest ./myapp`,
	Args: cobra.ExactArgs(1),
	RunE: runBuild,
}

func init() {
	buildCmd.Flags().Bool("pull", false, "Always pull image from registry (ignore cache)")
	buildCmd.Flags().StringP("tag", "t", "", "Tag the image locally")
	buildCmd.Flags().StringP("file", "f", "", "Path to Dockerfile (enables BuildKit-in-VM build)")
	buildCmd.Flags().Int("build-cpus", 0, "Number of CPUs for BuildKit VM (0 = all available)")
	buildCmd.Flags().Int("build-memory", 0, "Memory in MB for BuildKit VM (0 = all available)")
	buildCmd.Flags().Int("build-disk", 10240, "Disk size in MB for BuildKit VM")
	buildCmd.Flags().Bool("no-cache", false, "Do not use BuildKit build cache")
	buildCmd.Flags().Int("build-cache-size", 10240, "BuildKit cache disk size in MB")

	rootCmd.AddCommand(buildCmd)
}

func runBuild(cmd *cobra.Command, args []string) error {
	dockerfile, _ := cmd.Flags().GetString("file")
	tag, _ := cmd.Flags().GetString("tag")
	pull, _ := cmd.Flags().GetBool("pull")

	if dockerfile != "" {
		return runDockerfileBuild(cmd, args[0], dockerfile, tag)
	}

	imageRef := args[0]
	builder := image.NewBuilder(&image.BuildOptions{
		ForcePull: pull,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Printf("Building rootfs from %s...\n", imageRef)
	result, err := builder.Build(ctx, imageRef)
	if err != nil {
		return err
	}

	if tag != "" {
		if err := builder.SaveTag(tag, result); err != nil {
			return fmt.Errorf("saving tag: %w", err)
		}
		fmt.Printf("Tagged: %s\n", tag)
	}

	fmt.Printf("Built: %s\n", result.RootfsPath)
	fmt.Printf("Digest: %s\n", result.Digest)
	fmt.Printf("Size: %.1f MB\n", float64(result.Size)/(1024*1024))
	return nil
}

func runDockerfileBuild(cmd *cobra.Command, contextDir, dockerfile, tag string) error {
	cpus, _ := cmd.Flags().GetInt("build-cpus")
	memory, _ := cmd.Flags().GetInt("build-memory")
	disk, _ := cmd.Flags().GetInt("build-disk")
	noCache, _ := cmd.Flags().GetBool("no-cache")
	buildCacheSize, _ := cmd.Flags().GetInt("build-cache-size")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	ctx, cancel = contextWithSignal(ctx)
	defer cancel()

	builder := image.NewBuilder(&image.BuildOptions{})
	result, err := builder.BuildDockerfile(ctx, image.DockerfileBuildOptions{
		ContextDir:   contextDir,
		Dockerfile:   dockerfile,
		Tag:          tag,
		CPUs:         cpus,
		MemoryMB:     memory,
		DiskSizeMB:   disk,
		BuildCacheMB: buildCacheSize,
		NoCache:      noCache,
		Stdout:       os.Stderr,
		Stderr:       os.Stderr,
	}, newSandboxFactory())
	if err != nil {
		return err
	}

	fmt.Printf("Successfully built and tagged %s\n", tag)
	fmt.Printf("Rootfs: %s\n", result.RootfsPath)
	fmt.Printf("Size: %.1f MB\n", float64(result.Size)/(1024*1024))
	return nil
}

// newSandboxFactory returns an image.SandboxFactory that bridges to pkg/sandbox.
func newSandboxFactory() image.SandboxFactory {
	return func(ctx context.Context, rootfsPath string, opts *image.DockerfileBuildOptions, extraDisks []image.DiskMountInfo, mounts map[string]image.MountInfo) (image.SandboxRunner, error) {
		apiMounts := make(map[string]api.MountConfig, len(mounts))
		for guestPath, m := range mounts {
			apiMounts[guestPath] = api.MountConfig{
				Type:     "real_fs",
				HostPath: m.HostPath,
				Readonly: m.Readonly,
			}
		}

		apiDisks := make([]api.DiskMount, len(extraDisks))
		for i, d := range extraDisks {
			apiDisks[i] = api.DiskMount{
				HostPath:   d.HostPath,
				GuestMount: d.GuestMount,
			}
		}

		config := &api.Config{
			Image:      "moby/buildkit:rootless",
			Privileged: true,
			Resources: &api.Resources{
				CPUs:           opts.CPUs,
				MemoryMB:       opts.MemoryMB,
				DiskSizeMB:     opts.DiskSizeMB,
				TimeoutSeconds: 1800,
			},
			Network:    &api.NetworkConfig{},
			ExtraDisks: apiDisks,
			VFS: &api.VFSConfig{
				Workspace: "/workspace",
				Mounts:    apiMounts,
			},
		}

		sb, err := sandbox.New(ctx, config, &sandbox.Options{RootfsPath: rootfsPath})
		if err != nil {
			return nil, err
		}
		return &sandboxAdapter{sb: sb}, nil
	}
}

// sandboxAdapter adapts *sandbox.Sandbox to the image.SandboxRunner interface.
type sandboxAdapter struct {
	sb *sandbox.Sandbox
}

func (a *sandboxAdapter) Start(ctx context.Context) error {
	return a.sb.Start(ctx)
}

func (a *sandboxAdapter) Exec(ctx context.Context, command string, stdout, stderr io.Writer, workingDir string) (int, error) {
	result, err := a.sb.Exec(ctx, command, &api.ExecOptions{
		WorkingDir: workingDir,
		Stdout:     stdout,
		Stderr:     stderr,
	})
	if err != nil {
		return -1, err
	}
	return result.ExitCode, nil
}

func (a *sandboxAdapter) WriteFile(ctx context.Context, path string, content []byte, mode uint32) error {
	return a.sb.WriteFile(ctx, path, content, mode)
}

func (a *sandboxAdapter) Close() error {
	return a.sb.Close()
}
