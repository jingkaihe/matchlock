package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

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

// buildCachePath returns the path to the persistent BuildKit cache ext4 image.
func buildCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	cacheDir := filepath.Join(home, ".cache", "matchlock", "buildkit")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
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
		return fmt.Errorf("create cache image: %w", err)
	}
	if err := f.Truncate(targetBytes); err != nil {
		f.Close()
		os.Remove(cachePath)
		return fmt.Errorf("truncate cache image: %w", err)
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
		return fmt.Errorf("truncate cache image: %w", err)
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
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	// Try non-blocking lock first to avoid noisy message when uncontended.
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		fmt.Fprintf(os.Stderr, "Waiting for build cache lock (another build is running)...\n")
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
			f.Close()
			return nil, fmt.Errorf("acquire lock: %w", err)
		}
	}

	return f, nil
}

func runDockerfileBuild(cmd *cobra.Command, contextDir, dockerfile, tag string) error {
	if tag == "" {
		return fmt.Errorf("-t/--tag is required when building from a Dockerfile")
	}

	cpus, _ := cmd.Flags().GetInt("build-cpus")
	memory, _ := cmd.Flags().GetInt("build-memory")

	disk, _ := cmd.Flags().GetInt("build-disk")
	noCache, _ := cmd.Flags().GetBool("no-cache")
	buildCacheSize, _ := cmd.Flags().GetInt("build-cache-size")

	if cpus == 0 {
		cpus = runtime.NumCPU()
	}
	if memory == 0 {
		mem, err := totalMemoryMB()
		if err != nil {
			return fmt.Errorf("cannot auto-detect system memory: %w (use --build-memory to set explicitly)", err)
		}
		memory = mem
	}

	absContext, err := filepath.Abs(contextDir)
	if err != nil {
		return fmt.Errorf("resolve context dir: %w", err)
	}
	if info, err := os.Stat(absContext); err != nil || !info.IsDir() {
		return fmt.Errorf("build context %q is not a directory", contextDir)
	}

	absDockerfile, err := filepath.Abs(dockerfile)
	if err != nil {
		return fmt.Errorf("resolve Dockerfile: %w", err)
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
		return fmt.Errorf("building BuildKit rootfs: %w", err)
	}

	dockerfileName := filepath.Base(absDockerfile)
	dockerfileInContext := filepath.Join(absContext, dockerfileName)
	dockerfileDir := filepath.Dir(absDockerfile)

	workspaceDir, err := os.MkdirTemp("", "matchlock-build-workspace-*")
	if err != nil {
		return fmt.Errorf("create workspace temp dir: %w", err)
	}
	defer os.RemoveAll(workspaceDir)

	outputDir, err := os.MkdirTemp("", "matchlock-build-output-*")
	if err != nil {
		return fmt.Errorf("create output temp dir: %w", err)
	}
	defer os.RemoveAll(outputDir)

	mounts := map[string]api.MountConfig{
		"/workspace":         {Type: "real_fs", HostPath: workspaceDir},
		"/workspace/context": {Type: "real_fs", HostPath: absContext, Readonly: true},
		"/workspace/output":  {Type: "real_fs", HostPath: outputDir},
	}

	guestDockerfileDir := "/workspace/context"
	if _, err := os.Stat(dockerfileInContext); os.IsNotExist(err) {
		mounts["/workspace/dockerfile"] = api.MountConfig{Type: "real_fs", HostPath: dockerfileDir, Readonly: true}
		guestDockerfileDir = "/workspace/dockerfile"
	}

	var extraDisks []api.DiskMount
	if !noCache {
		cachePath, err := buildCachePath()
		if err != nil {
			return fmt.Errorf("resolve build cache path: %w", err)
		}
		lockFile, err := lockBuildCache(cachePath)
		if err != nil {
			return fmt.Errorf("lock build cache: %w", err)
		}
		defer lockFile.Close()
		if err := ensureBuildCacheImage(cachePath, buildCacheSize); err != nil {
			return fmt.Errorf("prepare build cache: %w", err)
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
		Network:    &api.NetworkConfig{},
		ExtraDisks: extraDisks,
		VFS: &api.VFSConfig{
			Workspace: "/workspace",
			Mounts:    mounts,
		},
	}

	sandboxOpts := &sandbox.Options{RootfsPath: buildResult.RootfsPath}
	sb, err := sandbox.New(ctx, config, sandboxOpts)
	if err != nil {
		return fmt.Errorf("creating BuildKit sandbox: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), api.DefaultGracefulShutdownPeriod)
		defer cancel()
		sb.Close(closeCtx)
	}()

	if err := sb.Start(ctx); err != nil {
		return fmt.Errorf("starting BuildKit sandbox: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Starting BuildKit daemon and building image from %s...\n", dockerfile)

	execOpts := &api.ExecOptions{
		WorkingDir: "/",
		Stdout:     os.Stderr,
		Stderr:     os.Stderr,
	}

	filenameOpt := ""
	if dockerfileName != "Dockerfile" {
		filenameOpt = fmt.Sprintf("  --opt filename=%s \\\n", dockerfileName)
	}

	noCacheOpt := ""
	if noCache {
		noCacheOpt = "  --no-cache \\\n"
	}

	buildScript := fmt.Sprintf(`#!/bin/sh
set -e
export HOME=/root
export TMPDIR=/var/lib/buildkit/tmp
mkdir -p $TMPDIR
SOCK=/tmp/buildkit.sock
buildkitd --root /var/lib/buildkit \
  --addr unix://$SOCK \
  --oci-worker-snapshotter native \
  >/tmp/buildkitd.log 2>&1 &
BKPID=$!
for i in $(seq 1 30); do [ -S $SOCK ] && break; sleep 1; done
if [ ! -S $SOCK ]; then
  echo "BuildKit daemon failed to start" >&2
  cat /tmp/buildkitd.log >&2
  exit 1
fi
echo "BuildKit daemon ready" >&2
buildctl --addr unix://$SOCK build \
  --frontend dockerfile.v0 \
  --local context=/workspace/context \
  --local dockerfile=%s \
%s%s  --output type=docker,dest=/workspace/output/image.tar
RC=$?
[ $RC -ne 0 ] && { echo "=== buildkitd log ===" >&2; cat /tmp/buildkitd.log >&2; }
kill $BKPID 2>/dev/null
exit $RC
`, guestDockerfileDir, filenameOpt, noCacheOpt)

	if err := sb.WriteFile(ctx, "/workspace/buildkit-run.sh", []byte(buildScript), 0755); err != nil {
		return fmt.Errorf("write build script: %w", err)
	}

	result, execErr := sb.Exec(ctx, "/workspace/buildkit-run.sh", execOpts)
	if execErr != nil {
		return fmt.Errorf("BuildKit build: %w", execErr)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("BuildKit build failed (exit %d)", result.ExitCode)
	}

	fmt.Fprintf(os.Stderr, "Importing built image as %s...\n", tag)

	tarballPath := filepath.Join(outputDir, "image.tar")
	importFile, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("open built image tarball: %w", err)
	}
	defer importFile.Close()

	importResult, err := builder.Import(ctx, importFile, tag)
	if err != nil {
		return fmt.Errorf("import built image: %w", err)
	}

	fmt.Printf("Successfully built and tagged %s\n", tag)
	fmt.Printf("Rootfs: %s\n", importResult.RootfsPath)
	fmt.Printf("Size: %.1f MB\n", float64(importResult.Size)/(1024*1024))
	return nil
}
