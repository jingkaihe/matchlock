package image

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// DockerfileBuildOptions configures a Dockerfile build using BuildKit-in-VM.
type DockerfileBuildOptions struct {
	ContextDir string
	Dockerfile string
	Tag        string

	CPUs           int
	MemoryMB       int
	DiskSizeMB     int
	BuildCacheMB   int
	NoCache        bool

	// Stdout and Stderr receive build output. Defaults to io.Discard.
	Stdout io.Writer
	Stderr io.Writer
}

func (o *DockerfileBuildOptions) setDefaults() error {
	if o.CPUs == 0 {
		o.CPUs = runtime.NumCPU()
	}
	if o.MemoryMB == 0 {
		mem, err := totalMemoryMB()
		if err != nil {
			return fmt.Errorf("cannot auto-detect system memory: %w (use MemoryMB to set explicitly)", err)
		}
		o.MemoryMB = mem
	}
	if o.DiskSizeMB == 0 {
		o.DiskSizeMB = 10240
	}
	if o.BuildCacheMB == 0 {
		o.BuildCacheMB = 10240
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	return nil
}

// SandboxRunner abstracts the sandbox lifecycle needed for Dockerfile builds.
// This avoids a circular import between pkg/image and pkg/sandbox.
type SandboxRunner interface {
	Start(ctx context.Context) error
	Exec(ctx context.Context, command string, stdout, stderr io.Writer, workingDir string) (exitCode int, err error)
	WriteFile(ctx context.Context, path string, content []byte, mode uint32) error
	Close() error
}

// SandboxFactory creates a sandbox from a rootfs path and build configuration.
type SandboxFactory func(ctx context.Context, rootfsPath string, opts *DockerfileBuildOptions, extraDisks []DiskMountInfo, mounts map[string]MountInfo) (SandboxRunner, error)

// DiskMountInfo describes a disk to attach to the build VM.
type DiskMountInfo struct {
	HostPath   string
	GuestMount string
}

// MountInfo describes a VFS mount for the build VM.
type MountInfo struct {
	HostPath string
	Readonly bool
}

// BuildDockerfile orchestrates a Dockerfile build using BuildKit-in-VM.
// It requires a SandboxFactory to create the build VM.
func (b *Builder) BuildDockerfile(ctx context.Context, opts DockerfileBuildOptions, factory SandboxFactory) (*BuildResult, error) {
	if opts.Tag == "" {
		return nil, fmt.Errorf("Tag is required when building from a Dockerfile")
	}

	if err := opts.setDefaults(); err != nil {
		return nil, err
	}

	absContext, err := filepath.Abs(opts.ContextDir)
	if err != nil {
		return nil, fmt.Errorf("resolve context dir: %w", err)
	}
	if info, err := os.Stat(absContext); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("build context %q is not a directory", opts.ContextDir)
	}

	absDockerfile, err := filepath.Abs(opts.Dockerfile)
	if err != nil {
		return nil, fmt.Errorf("resolve Dockerfile: %w", err)
	}
	if _, err := os.Stat(absDockerfile); err != nil {
		return nil, fmt.Errorf("Dockerfile not found: %s", opts.Dockerfile)
	}

	buildkitImage := "moby/buildkit:rootless"
	fmt.Fprintf(opts.Stderr, "Preparing BuildKit image (%s)...\n", buildkitImage)
	buildResult, err := b.Build(ctx, buildkitImage)
	if err != nil {
		return nil, fmt.Errorf("building BuildKit rootfs: %w", err)
	}

	dockerfileName := filepath.Base(absDockerfile)
	dockerfileInContext := filepath.Join(absContext, dockerfileName)
	dockerfileDir := filepath.Dir(absDockerfile)

	workspaceDir, err := os.MkdirTemp("", "matchlock-build-workspace-*")
	if err != nil {
		return nil, fmt.Errorf("create workspace temp dir: %w", err)
	}
	defer os.RemoveAll(workspaceDir)

	outputDir, err := os.MkdirTemp("", "matchlock-build-output-*")
	if err != nil {
		return nil, fmt.Errorf("create output temp dir: %w", err)
	}
	defer os.RemoveAll(outputDir)

	mounts := map[string]MountInfo{
		"/workspace":         {HostPath: workspaceDir},
		"/workspace/context": {HostPath: absContext, Readonly: true},
		"/workspace/output":  {HostPath: outputDir},
	}

	guestDockerfileDir := "/workspace/context"
	if _, err := os.Stat(dockerfileInContext); os.IsNotExist(err) {
		mounts["/workspace/dockerfile"] = MountInfo{HostPath: dockerfileDir, Readonly: true}
		guestDockerfileDir = "/workspace/dockerfile"
	}

	var extraDisks []DiskMountInfo
	if !opts.NoCache {
		cachePath, err := BuildCachePath()
		if err != nil {
			return nil, fmt.Errorf("resolve build cache path: %w", err)
		}
		lockFile, err := LockBuildCache(cachePath)
		if err != nil {
			return nil, fmt.Errorf("lock build cache: %w", err)
		}
		defer lockFile.Close()
		if err := EnsureBuildCacheImage(cachePath, opts.BuildCacheMB); err != nil {
			return nil, fmt.Errorf("prepare build cache: %w", err)
		}
		extraDisks = append(extraDisks, DiskMountInfo{
			HostPath:   cachePath,
			GuestMount: "/var/lib/buildkit",
		})
		fmt.Fprintf(opts.Stderr, "Using build cache at %s\n", cachePath)
	}

	sb, err := factory(ctx, buildResult.RootfsPath, &opts, extraDisks, mounts)
	if err != nil {
		return nil, fmt.Errorf("creating BuildKit sandbox: %w", err)
	}
	defer sb.Close()

	if err := sb.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting BuildKit sandbox: %w", err)
	}

	fmt.Fprintf(opts.Stderr, "Starting BuildKit daemon and building image from %s...\n", opts.Dockerfile)

	filenameOpt := ""
	if dockerfileName != "Dockerfile" {
		filenameOpt = fmt.Sprintf("  --opt filename=%s \\\n", dockerfileName)
	}

	noCacheOpt := ""
	if opts.NoCache {
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
		return nil, fmt.Errorf("write build script: %w", err)
	}

	exitCode, execErr := sb.Exec(ctx, "/workspace/buildkit-run.sh", opts.Stderr, opts.Stderr, "/")
	if execErr != nil {
		return nil, fmt.Errorf("BuildKit build: %w", execErr)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("BuildKit build failed (exit %d)", exitCode)
	}

	fmt.Fprintf(opts.Stderr, "Importing built image as %s...\n", opts.Tag)

	tarballPath := filepath.Join(outputDir, "image.tar")
	importFile, err := os.Open(tarballPath)
	if err != nil {
		return nil, fmt.Errorf("open built image tarball: %w", err)
	}
	defer importFile.Close()

	importResult, err := b.Import(ctx, importFile, opts.Tag)
	if err != nil {
		return nil, fmt.Errorf("import built image: %w", err)
	}

	return importResult, nil
}

// BuildCachePath returns the path to the persistent BuildKit cache ext4 image.
func BuildCachePath() (string, error) {
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

// EnsureBuildCacheImage creates an ext4 image at cachePath if it doesn't already exist.
// If the image exists but is smaller than sizeMB, it is grown in-place.
// Must be called while holding the build cache lock.
func EnsureBuildCacheImage(cachePath string, sizeMB int) error {
	if sizeMB <= 0 {
		return fmt.Errorf("build-cache-size must be positive, got %d", sizeMB)
	}

	targetBytes := int64(sizeMB) * 1024 * 1024

	if fi, err := os.Stat(cachePath); err == nil {
		if fi.Size() >= targetBytes {
			return nil
		}
		return GrowExt4Image(cachePath, targetBytes)
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

// GrowExt4Image expands an existing ext4 image to targetBytes using truncate + resize2fs.
func GrowExt4Image(path string, targetBytes int64) error {
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

// LockBuildCache acquires an exclusive file lock on the build cache.
// Returns the lock file which must be closed to release the lock.
func LockBuildCache(cachePath string) (*os.File, error) {
	lockPath := cachePath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
			f.Close()
			return nil, fmt.Errorf("acquire lock: %w", err)
		}
	}

	return f, nil
}
