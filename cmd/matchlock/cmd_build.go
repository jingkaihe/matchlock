package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
	"github.com/jingkaihe/matchlock/pkg/vsock"
)

var buildCmd = &cobra.Command{
	Use:   "build [flags] <context>",
	Short: "Build image from a Dockerfile using BuildKit-in-VM",
	Long: `Build an image from a Dockerfile using BuildKit-in-VM.

The argument is the build context directory. If a Dockerfile exists in the context directory,
it is picked up automatically. Use -f/--file to specify an alternative Dockerfile.

To pull a pre-built container image, use "matchlock pull" instead.`,
	Example: `  matchlock build -t myapp:latest .
  matchlock build -t myapp:latest ./myapp
  matchlock build -f Dockerfile.dev -t myapp:latest .`,
	Args: cobra.ExactArgs(1),
	RunE: runBuild,
}

func init() {
	buildCmd.Flags().StringP("tag", "t", "", "Tag the built image locally")
	buildCmd.Flags().StringP("file", "f", "Dockerfile", "Path to Dockerfile")
	buildCmd.Flags().Float64("build-cpus", 0, "Number of CPUs for BuildKit VM (supports fractional values, 0 = all available)")
	buildCmd.Flags().Int("build-memory", 0, "Memory in MB for BuildKit VM (0 = all available)")
	buildCmd.Flags().Int("build-disk", 10240, "Disk size in MB for BuildKit VM (holds the uploaded build context)")
	buildCmd.Flags().Bool("no-cache", false, "Do not use BuildKit build cache")
	buildCmd.Flags().Int("build-cache-size", 10240, "BuildKit cache disk size in MB")
	buildCmd.Flags().Int("mtu", api.DefaultNetworkMTU, "Network MTU for BuildKit guest interface")

	rootCmd.AddCommand(buildCmd)
}

func runBuild(cmd *cobra.Command, args []string) error {
	dockerfile, _ := cmd.Flags().GetString("file")
	tag, _ := cmd.Flags().GetString("tag")

	// If -f was not explicitly set, resolve the default relative to the context directory.
	if !cmd.Flags().Changed("file") {
		dockerfile = filepath.Join(args[0], dockerfile)
	}

	return runDockerfileBuild(cmd, args[0], dockerfile, tag)
}

// buildCachePath returns the path to the persistent BuildKit cache ext4 image.
func buildCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errx.Wrap(ErrGetHomeDir, err)
	}
	cacheDir := filepath.Join(home, ".cache", "matchlock", "buildkit")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", errx.Wrap(ErrCreateCacheDir, err)
	}
	return filepath.Join(cacheDir, "cache.ext4"), nil
}

// ensureBuildCacheImage creates an ext4 image at cachePath if it doesn't already exist.
// If the image exists but is smaller than sizeMB, it is grown in-place.
// Must be called while holding the build cache lock.
func ensureBuildCacheImage(cachePath string, sizeMB int) error {
	if sizeMB <= 0 {
		return fmt.Errorf("build-cache-size must be positive, got %d", sizeMB)
	}

	targetBytes := int64(sizeMB) * 1024 * 1024

	if fi, err := os.Stat(cachePath); err == nil {
		if fi.Size() >= targetBytes {
			return nil
		}
		return growExt4Image(cachePath, targetBytes)
	}

	f, err := os.Create(cachePath)
	if err != nil {
		return errx.Wrap(ErrCreateCacheImage, err)
	}
	if err := f.Truncate(targetBytes); err != nil {
		f.Close()
		os.Remove(cachePath)
		return errx.Wrap(ErrTruncateCacheImage, err)
	}
	f.Close()

	mkfs := exec.Command("mkfs.ext4", "-q", cachePath)
	if out, err := mkfs.CombinedOutput(); err != nil {
		os.Remove(cachePath)
		return fmt.Errorf("mkfs.ext4: %w: %s", err, out)
	}

	return nil
}

// growExt4Image expands an existing ext4 image to targetBytes using truncate + resize2fs.
func growExt4Image(path string, targetBytes int64) error {
	fmt.Fprintf(os.Stderr, "Growing build cache to %d MB...\n", targetBytes/(1024*1024))

	if err := os.Truncate(path, targetBytes); err != nil {
		return errx.Wrap(ErrTruncateCacheImage, err)
	}

	if e2fsck, err := exec.LookPath("e2fsck"); err == nil {
		cmd := exec.Command(e2fsck, "-fy", path)
		cmd.CombinedOutput()
	}

	resize2fs, err := exec.LookPath("resize2fs")
	if err != nil {
		return fmt.Errorf("resize2fs not found; install e2fsprogs to grow cache")
	}

	cmd := exec.Command(resize2fs, "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs: %w: %s", err, out)
	}

	return nil
}

// lockBuildCache acquires an exclusive file lock on the build cache.
// Returns the lock file which must be closed to release the lock.
func lockBuildCache(cachePath string) (*os.File, error) {
	lockPath := cachePath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, errx.Wrap(ErrOpenLockFile, err)
	}

	// Try non-blocking lock first to avoid noisy message when uncontended.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr, "Waiting for build cache lock (another build is running)...\n")
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
			f.Close()
			return nil, errx.Wrap(ErrAcquireLock, err)
		}
	}

	return f, nil
}

func runDockerfileBuild(cmd *cobra.Command, contextDir, dockerfile, tag string) error {
	if tag == "" {
		return fmt.Errorf("-t/--tag is required when building from a Dockerfile")
	}

	cpus, _ := cmd.Flags().GetFloat64("build-cpus")
	memory, _ := cmd.Flags().GetInt("build-memory")

	disk, _ := cmd.Flags().GetInt("build-disk")
	noCache, _ := cmd.Flags().GetBool("no-cache")
	buildCacheSize, _ := cmd.Flags().GetInt("build-cache-size")
	networkMTU, _ := cmd.Flags().GetInt("mtu")

	if networkMTU <= 0 {
		return fmt.Errorf("--mtu must be > 0")
	}

	hostCPUs := runtime.NumCPU()
	if cpus == 0 {
		cpus = float64(hostCPUs)
	}
	vcpuCount, ok := api.VCPUCount(cpus)
	if !ok {
		return fmt.Errorf("--build-cpus must be a finite number > 0 (or 0 for all available)")
	}
	if vcpuCount > hostCPUs {
		return fmt.Errorf("--build-cpus must be <= host cpus (%d)", hostCPUs)
	}
	if memory == 0 {
		mem, err := totalMemoryMB()
		if err != nil {
			return errx.With(ErrAutoDetectMemory, ": %w (use --build-memory to set explicitly)", err)
		}
		memory = mem
	}

	absContext, err := filepath.Abs(contextDir)
	if err != nil {
		return errx.Wrap(ErrResolveContextDir, err)
	}
	if info, err := os.Stat(absContext); err != nil || !info.IsDir() {
		return fmt.Errorf("build context %q is not a directory", contextDir)
	}

	absDockerfile, err := filepath.Abs(dockerfile)
	if err != nil {
		return errx.Wrap(ErrResolveDockerfile, err)
	}
	if _, err := os.Stat(absDockerfile); err != nil {
		return fmt.Errorf("Dockerfile not found: %s", dockerfile)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	ctx, cancel = contextWithSignal(ctx)
	defer cancel()

	buildkitImage := "moby/buildkit:rootless"
	fmt.Fprintf(os.Stderr, "Preparing BuildKit image (%s)...\n", buildkitImage)
	builder := image.NewBuilder(&image.BuildOptions{})
	buildResult, err := builder.Build(ctx, buildkitImage)
	if err != nil {
		return errx.Wrap(ErrBuildBuildKitRootfs, err)
	}

	var extraDisks []api.DiskMount
	if !noCache {
		cachePath, err := buildCachePath()
		if err != nil {
			return errx.Wrap(ErrResolveCachePath, err)
		}
		lockFile, err := lockBuildCache(cachePath)
		if err != nil {
			return errx.Wrap(ErrLockBuildCache, err)
		}
		defer lockFile.Close()
		if err := ensureBuildCacheImage(cachePath, buildCacheSize); err != nil {
			return errx.Wrap(ErrPrepareBuildCache, err)
		}
		extraDisks = append(extraDisks, api.DiskMount{
			HostPath:   cachePath,
			GuestMount: "/var/lib/buildkit",
		})
		fmt.Fprintf(os.Stderr, "Using build cache at %s\n", cachePath)
	}

	config := &api.Config{
		Image:      buildkitImage,
		Privileged: true,
		Resources: &api.Resources{
			CPUs:           cpus,
			MemoryMB:       memory,
			DiskSizeMB:     disk,
			TimeoutSeconds: 1800,
		},
		Network: &api.NetworkConfig{
			MTU: networkMTU,
		},
		ExtraDisks: extraDisks,
	}

	sandboxOpts := &sandbox.Options{
		RootfsPaths:   buildResult.LowerPaths,
		RootfsFSTypes: buildResult.LowerFSTypes,
	}
	sb, err := sandbox.New(ctx, config, sandboxOpts)
	if err != nil {
		return errx.Wrap(ErrCreateBuildSandbox, err)
	}
	defer func() {
		closeCtx, cancel := closeContext(api.DefaultGracefulShutdownPeriod)
		defer cancel()
		sb.Close(closeCtx)
	}()

	if err := sb.Start(ctx); err != nil {
		return errx.Wrap(ErrStartBuildSandbox, err)
	}

	fmt.Fprintln(os.Stderr, "Uploading build context...")
	if err := uploadBuildContext(ctx, sb, absContext, absDockerfile); err != nil {
		return err
	}

	noCacheOpt := ""
	if noCache {
		noCacheOpt = "  --no-cache \\\n"
	}

	// Stdout carries only the docker-format image tarball from buildctl; every
	// other message goes to stderr so it cannot corrupt the stream.
	buildScript := fmt.Sprintf(`#!/bin/sh
set -e
exec 3>&1 1>&2
export HOME=/root
export TMPDIR=/var/lib/buildkit/tmp
mkdir -p $TMPDIR
SOCK=/tmp/buildkit.sock
buildkitd --root /var/lib/buildkit \
  --addr unix://$SOCK \
  --oci-worker-snapshotter native \
  >/tmp/buildkitd.log 2>&1 3>&- &
BKPID=$!
trap 'kill "$BKPID" 2>/dev/null || true' EXIT
for i in $(seq 1 30); do [ -S $SOCK ] && break; sleep 1; done
if [ ! -S $SOCK ]; then
  echo "BuildKit daemon failed to start" >&2
  cat /tmp/buildkitd.log >&2
  exit 1
fi
echo "BuildKit daemon ready" >&2
set +e
buildctl --addr unix://$SOCK build \
  --frontend dockerfile.v0 \
  --local context=/workspace/context \
  --local dockerfile=/workspace/dockerfile \
%s  --output type=docker,dest=- >&3
RC=$?
[ $RC -ne 0 ] && { echo "=== buildkitd log ===" >&2; cat /tmp/buildkitd.log >&2; }
exit $RC
`, noCacheOpt)

	if err := sb.WriteFile(ctx, "/workspace/buildkit-run.sh", []byte(buildScript), 0755); err != nil {
		return errx.Wrap(ErrWriteBuildScript, err)
	}

	fmt.Fprintf(os.Stderr, "Starting BuildKit daemon and building image from %s as %s...\n", dockerfile, tag)

	importResult, err := buildAndImportImage(ctx, sb, builder, "/workspace/buildkit-run.sh", tag)
	if err != nil {
		return err
	}

	fmt.Printf("Successfully built and tagged %s\n", tag)
	fmt.Printf("Layers: %d\n", len(importResult.LowerPaths))
	fmt.Printf("Size: %.1f MB\n", float64(importResult.Size)/(1024*1024))
	return nil
}

// uploadBuildContext transfers a bounded-memory tar stream through the exec
// service. All build files live on guest storage, never on a shared host mount.
func uploadBuildContext(ctx context.Context, sb buildExecer, contextDir, dockerfile string) error {
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := writeBuildContext(ctx, w, contextDir, dockerfile)
		w.CloseWithError(err)
		done <- err
	}()
	result, execErr := sb.Exec(ctx, "mkdir -p /workspace && tar -xf - -C /workspace", &api.ExecOptions{
		WorkingDir: "/",
		Stdin:      r,
		Stdout:     os.Stderr,
		Stderr:     os.Stderr,
	})
	// Unblock the producer if the guest exits or is canceled before consuming stdin.
	r.Close()
	archiveErr := <-done
	// A closed pipe only means the guest stopped reading; report why it did.
	if archiveErr != nil && !errors.Is(archiveErr, io.ErrClosedPipe) {
		return errx.Wrap(ErrBuildContext, archiveErr)
	}
	if execErr != nil {
		return errx.Wrap(ErrUploadBuildContext, execErr)
	}
	if result.ExitCode != 0 {
		return errx.With(ErrUploadBuildContext, ": exit code %d", result.ExitCode)
	}
	if archiveErr != nil {
		return errx.Wrap(ErrBuildContext, archiveErr)
	}
	return nil
}

type buildExecer interface {
	Exec(ctx context.Context, command string, opts *api.ExecOptions) (*api.ExecResult, error)
}

type imageImporter interface {
	Import(ctx context.Context, reader io.Reader, tag string) (*image.BuildResult, error)
}

// buildAndImportImage runs the BuildKit script and streams the image tarball
// from its stdout straight into the host image store, so the image is never
// staged on the build VM's disk.
func buildAndImportImage(ctx context.Context, sb buildExecer, importer imageImporter, script, tag string) (*image.BuildResult, error) {
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		result, err := sb.Exec(ctx, script, &api.ExecOptions{
			WorkingDir: "/",
			Stdout:     w,
			Stderr:     os.Stderr,
		})
		if err != nil {
			err = errx.Wrap(ErrBuildKitBuild, err)
		} else if result.ExitCode != 0 {
			err = errx.With(ErrBuildKitBuild, ": exit code %d", result.ExitCode)
		}
		// A failed build must not look like a complete tarball to the importer.
		w.CloseWithError(err)
		done <- err
	}()
	result, importErr := importer.Import(ctx, r, tag)
	// Stop the build if the import fails before draining the stream (for
	// example, a full host disk); the guest command is terminated.
	r.Close()
	buildErr := <-done
	if buildErr != nil && (importErr == nil || !errors.Is(buildErr, vsock.ErrWriteExecOutput)) {
		return nil, buildErr
	}
	if importErr != nil {
		return nil, errx.Wrap(ErrImportImage, importErr)
	}
	return result, nil
}
