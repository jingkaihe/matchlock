package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
	"github.com/jingkaihe/matchlock/pkg/image"
	"github.com/jingkaihe/matchlock/pkg/sandbox"
)

// Default build flags. These are tuned so the plain `matchlock build -t <tag> .`
// works for heavy images (e.g. a full Ubuntu-based toolchain like bedlam):
//   - overlayfs snapshotter (native stores full layer copies, which is space-heavy
//     and can hit the ext4 reserved-block floor on big images)
//   - a large cache disk so apt / layer data has room above the reserved-block floor
//   - a large writable build disk
const (
	defaultBuildDisk        = 32768 // MB (32 GiB) writable root disk for the build VM
	defaultBuildCacheSize   = 65536 // MB (64 GiB) BuildKit cache disk (holds /var/lib/buildkit)
	defaultBuildSnapshotter = "overlayfs"

	// defaultMaxBuildMemoryMB caps the *default* BuildKit VM memory
	// (--build-memory 0 = "all available"). Handing the guest the host's entire
	// RAM makes the kernel's early memory initialization dominate boot: on a
	// 1.35 TiB host the guest needed ~33 s just to signal ready, so the build
	// failed with "timeout waiting for VM ready signal". An explicit
	// --build-memory is still honored (validated against host RAM).
	defaultMaxBuildMemoryMB = 32768 // MB (32 GiB)
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
	buildCmd.Flags().Int("build-memory", 0, fmt.Sprintf("Memory in MB for BuildKit VM (0 = all available, capped at %d MB)", defaultMaxBuildMemoryMB))
	buildCmd.Flags().Int("build-disk", defaultBuildDisk, "Disk size in MB for BuildKit VM")
	buildCmd.Flags().Bool("no-cache", false, "Do not use BuildKit build cache")
	buildCmd.Flags().Int("build-cache-size", defaultBuildCacheSize, "BuildKit cache disk size in MB")
	buildCmd.Flags().Int("mtu", api.DefaultNetworkMTU, "Network MTU for BuildKit guest interface")
	buildCmd.Flags().Int("build-timeout", 0, "Build timeout in seconds (0 = no timeout)")
	buildCmd.Flags().String("build-snapshotter", defaultBuildSnapshotter, "OCI worker snapshotter (native or overlayfs)")

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

// buildTimeoutSeconds maps the --build-timeout flag (seconds) to a resource-level
// timeout. A value of 0 means "no timeout"; we pass a generous sentinel so the
// sandbox/RPC never interprets 0 as an immediate deadline.
func buildTimeoutSeconds(sec int) int {
	if sec <= 0 {
		return 86400 // 24h: effectively no timeout
	}
	return sec
}

// buildScript renders the in-guest shell script that starts buildkitd and runs
// buildctl. snapshotter must be one of the accepted --build-snapshotter values.
// dockerfileDir is the guest path to the local dockerfile context; filenameOpt
// and noCacheOpt are pre-built "  --opt filename=... \\" / "  --no-cache \\"
// continuation lines (empty when not applicable).
func buildScript(snapshotter, dockerfileDir, filenameOpt, noCacheOpt string) string {
	return fmt.Sprintf(`#!/bin/sh
set -e
export HOME=/root
export TMPDIR=/var/lib/buildkit/tmp
mkdir -p $TMPDIR
SOCK=/tmp/buildkit.sock
buildkitd --root /var/lib/buildkit \
  --addr unix://$SOCK \
  --oci-worker-snapshotter %s \
  >/tmp/buildkitd.log 2>&1 &
BKPID=$!
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
  --local dockerfile=%s \
%s%s  --output type=docker,dest=/workspace/output/image.tar
RC=$?
if [ $RC -ne 0 ]; then
  echo "=== buildkitd log ===" >&2
  cat /tmp/buildkitd.log >&2
  kill $BKPID 2>/dev/null
  exit $RC
fi
kill $BKPID 2>/dev/null
exit 0
`, snapshotter, dockerfileDir, filenameOpt, noCacheOpt)
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

	if _, err := os.Stat(cachePath); err == nil {
		return ensureExistingCacheGrown(cachePath, targetBytes)
	} else if !os.IsNotExist(err) {
		// A real stat failure (not "absent") must be surfaced, not treated as
		// "create new".
		return errx.Wrap(ErrStatCacheImage, err)
	}

	// Cache does not exist: create it, and only then format.
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

// ensureExistingCacheGrown grows an existing cache to at least targetBytes.
// It only ever enlarges (never shrinks) and refuses to grow a file it cannot
// inspect: an interrupted prior grow (truncate succeeded but resize2fs did
// not) leaves a big file with a small FS, which this repairs by re-running
// the grow when the FS is measurably short.
func ensureExistingCacheGrown(cachePath string, targetBytes int64) error {
	fi, err := os.Stat(cachePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errx.Wrap(ErrStatCacheImage, err)
	}
	if fi.Size() < targetBytes {
		// File is smaller than target: expand it.
		return growExt4Image(cachePath, targetBytes)
	}

	// File is already at/above target. The only case worth repairing is a FS
	// that failed to grow while the file grew (truncate succeeded, resize2fs
	// did not). Inspect once; if we cannot read the FS size, leave the big
	// file untouched — an uninspectable-but-big-enough cache is the normal
	// happy path and must not fail the build, and must not be resized.
	size, err := ext4FSSizeBytes(cachePath)
	if err != nil {
		return nil
	}
	if size < targetBytes {
		return growExt4Image(cachePath, targetBytes)
	}
	return nil
}

// growExt4Image expands an existing ext4 image to targetBytes using truncate +
// e2fsck + resize2fs, then VERIFIES the filesystem actually grew. It never
// shrinks the file: if the backing file is already larger than targetBytes it
// only grows the filesystem to fit the existing (larger) file.
func growExt4Image(path string, targetBytes int64) error {
	fmt.Fprintf(os.Stderr, "Growing build cache to %d MB...\n", targetBytes/(1024*1024))

	// Preflight the tooling before mutating anything, so a missing e2fsprogs
	// fails fast instead of truncating the file and leaving a partial FS.
	resize2fs, err := exec.LookPath("resize2fs")
	if err != nil {
		// resize2fs missing: return an actionable error rather than masking a
		// cache that is effectively empty (the Mac host hit exactly this).
		return fmt.Errorf("resize2fs not found; install e2fsprogs to grow cache")
	}

	if fi, err := os.Stat(path); err == nil {
		current := fi.Size()
		if current < targetBytes {
			// Only expand; never truncate down. A cache can already be larger
			// than the requested target (e.g. grown earlier to 96 GiB), and
			// shrinking it would destroy cached layers.
			if err := os.Truncate(path, targetBytes); err != nil {
				return errx.Wrap(ErrTruncateCacheImage, err)
			}
		}
	}

	if e2fsck, err := exec.LookPath("e2fsck"); err == nil {
		// e2fsck -fy is best-effort; resize2fs below is the source of truth.
		_ = exec.Command(e2fsck, "-fy", path).Run()
	}

	cmd := exec.Command(resize2fs, "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs: %w: %s", err, out)
	}

	// Verify the filesystem (not the backing file) reached the target. A grow
	// can truncate the file and then fail to enlarge the FS (e.g. resize2fs
	// missing or interrupted); without this the next run would trust the file
	// size and never repair it.
	if size, err := ext4FSSizeBytes(path); err != nil || size < targetBytes {
		return fmt.Errorf("resize2fs finished but the filesystem is still below %d bytes; cache not actually grown", targetBytes)
	}

	return nil
}

// fsSizeBytesAtLeast reports whether the ext4 image's total filesystem size
// (block count × block size) is at least targetBytes.
func fsSizeBytesAtLeast(path string, targetBytes int64) bool {
	size, err := ext4FSSizeBytes(path)
	if err != nil {
		return false
	}
	return size >= targetBytes
}

// dumpe2fsBlockField extracts a numeric field from a `dumpe2fs -h` line that is
// shaped like "Label:<padding><number>". Returns (0, false) if the label does
// not match or the value is not a positive integer.
func dumpe2fsBlockField(out []byte, prefix string) (int64, bool) {
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		body, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		body = strings.TrimSpace(body)
		// The body is "12345" only; any extra spaces already trimmed. It must not
		// contain a second token (e.g. a trailing comment).
		if body == "" || strings.ContainsAny(body, " 	") {
			return 0, false
		}
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// ext4FSSizeBytes returns the total filesystem size (block count × block size)
// of an ext4 image. Uses dumpe2fs -h so it works on image files without a
// mount. Returns an error only when dumpe2fs is unavailable or the image is not
// a parseable ext4 filesystem.
func ext4FSSizeBytes(path string) (int64, error) {
	out, err := exec.Command("dumpe2fs", "-h", path).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("dumpe2fs -h %s: %w: %s", path, err, out)
	}
	blockCount, okCount := dumpe2fsBlockField(out, "Block count:")
	blockSize, okSize := dumpe2fsBlockField(out, "Block size:")
	if !okCount || !okSize {
		return 0, fmt.Errorf("dumpe2fs -h %s: could not parse 'Block count'/'Block size'", path)
	}
	return blockCount * blockSize, nil
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

// resolveBuildCPUs validates and resolves the --build-cpus flag for the build
// VM. When the flag is left at its default (or explicitly 0), the value means
// "all available": we use every host CPU but never more than the Firecracker
// backend's maximum, so a default build on a many-core host does not hand the
// VMM a vCPU count it rejects (which surfaces only as a ready timeout). An
// explicit request above the backend maximum fails here, before any VM is
// created, with a message naming the limit.
func resolveBuildCPUs(changed bool, cpus float64, hostCPUs int) (float64, error) {
	if !changed || cpus == 0 {
		cpus = float64(min(hostCPUs, api.MaxFirecrackerVCPUs))
	}
	vcpuCount, ok := api.VCPUCount(cpus)
	if !ok {
		return 0, fmt.Errorf("--build-cpus must be a finite number > 0 (or 0 for all available)")
	}
	if vcpuCount > api.MaxFirecrackerVCPUs {
		return 0, fmt.Errorf("--build-cpus %d exceeds the Firecracker maximum of %d vCPUs (backend limit)", vcpuCount, api.MaxFirecrackerVCPUs)
	}
	if vcpuCount > hostCPUs {
		return 0, fmt.Errorf("--build-cpus must be <= host cpus (%d)", hostCPUs)
	}
	return cpus, nil
}

// resolveBuildMemory validates and resolves the --build-memory flag for the
// build VM. The default (or an explicit 0) means "all available", capped at
// defaultMaxBuildMemoryMB so a very large host does not hand the BuildKit guest
// so much RAM that kernel memory initialization outlasts the ready timeout. An
// explicit request larger than host RAM fails here, before any VM is created.
func resolveBuildMemory(changed bool, memoryMB, hostMB int) (int, error) {
	if memoryMB < 0 {
		return 0, fmt.Errorf("--build-memory must be >= 0 (or 0 for all available)")
	}
	if changed && memoryMB > 0 {
		if hostMB > 0 && memoryMB > hostMB {
			return 0, fmt.Errorf("--build-memory %d exceeds host memory (%d MB)", memoryMB, hostMB)
		}
		return memoryMB, nil
	}
	if hostMB > 0 && hostMB < defaultMaxBuildMemoryMB {
		return hostMB, nil
	}
	return defaultMaxBuildMemoryMB, nil
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
	buildTimeout, _ := cmd.Flags().GetInt("build-timeout")
	buildSnapshotter, _ := cmd.Flags().GetString("build-snapshotter")

	if networkMTU <= 0 {
		return fmt.Errorf("--mtu must be > 0")
	}
	switch buildSnapshotter {
	case "native", "overlayfs":
	default:
		return fmt.Errorf("--build-snapshotter must be \"native\" or \"overlayfs\", got %q", buildSnapshotter)
	}

	hostCPUs := runtime.NumCPU()
	cpus, err := resolveBuildCPUs(cmd.Flags().Changed("build-cpus"), cpus, hostCPUs)
	if err != nil {
		return err
	}
	hostMB, err := totalMemoryMB()
	if err != nil {
		return errx.With(ErrAutoDetectMemory, ": %w (use --build-memory to set explicitly)", err)
	}
	memory, err = resolveBuildMemory(cmd.Flags().Changed("build-memory"), memory, hostMB)
	if err != nil {
		return err
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

	var ctx context.Context
	var cancel context.CancelFunc
	if buildTimeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(buildTimeout)*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
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

	dockerfileName := filepath.Base(absDockerfile)
	dockerfileInContext := filepath.Join(absContext, dockerfileName)
	dockerfileDir := filepath.Dir(absDockerfile)

	workspaceDir, err := os.MkdirTemp("", "matchlock-build-workspace-*")
	if err != nil {
		return errx.Wrap(ErrCreateWorkspaceDir, err)
	}
	defer os.RemoveAll(workspaceDir)

	outputDir, err := os.MkdirTemp("", "matchlock-build-output-*")
	if err != nil {
		return errx.Wrap(ErrCreateOutputDir, err)
	}
	defer os.RemoveAll(outputDir)

	mounts := map[string]api.MountConfig{
		"/workspace":         {Type: api.MountTypeHostFS, HostPath: workspaceDir},
		"/workspace/context": {Type: api.MountTypeHostFS, HostPath: absContext, Readonly: true},
		"/workspace/output":  {Type: api.MountTypeHostFS, HostPath: outputDir},
	}

	guestDockerfileDir := "/workspace/context"
	if _, err := os.Stat(dockerfileInContext); os.IsNotExist(err) {
		mounts["/workspace/dockerfile"] = api.MountConfig{Type: api.MountTypeHostFS, HostPath: dockerfileDir, Readonly: true}
		guestDockerfileDir = "/workspace/dockerfile"
	}

	var extraDisks []api.DiskMount
	// Always attach the BuildKit cache disk. Its placement (/var/lib/buildkit) is
	// independent of cache-reuse policy: the snapshotter (native/overlayfs) needs a
	// real backing filesystem for layer data regardless of whether we reuse it.
	// --no-cache only controls BuildKit's layer-cache reuse via the buildctl flag
	// below; it must NOT detach the cache disk (which would move the builder root
	// onto the small overlay rootfs and break overlayfs snapshotting).
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

	config := &api.Config{
		Image:      buildkitImage,
		Privileged: true,
		Resources: &api.Resources{
			CPUs:           cpus,
			MemoryMB:       memory,
			DiskSizeMB:     disk,
			TimeoutSeconds: buildTimeoutSeconds(buildTimeout),
		},
		Network: &api.NetworkConfig{
			MTU: networkMTU,
		},
		ExtraDisks: extraDisks,
		VFS: &api.VFSConfig{
			Workspace: "/workspace",
			Mounts:    mounts,
		},
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

	buildScriptData := buildScript(buildSnapshotter, guestDockerfileDir, filenameOpt, noCacheOpt)

	if err := sb.WriteFile(ctx, "/workspace/buildkit-run.sh", []byte(buildScriptData), 0755); err != nil {
		return errx.Wrap(ErrWriteBuildScript, err)
	}

	result, execErr := sb.Exec(ctx, "/workspace/buildkit-run.sh", execOpts)
	if execErr != nil {
		return errx.Wrap(ErrBuildKitBuild, execErr)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("BuildKit build failed (exit %d)", result.ExitCode)
	}

	fmt.Fprintf(os.Stderr, "Importing built image as %s...\n", tag)

	tarballPath := filepath.Join(outputDir, "image.tar")
	importFile, err := os.Open(tarballPath)
	if err != nil {
		return errx.Wrap(ErrOpenImageTarball, err)
	}
	defer importFile.Close()

	importResult, err := builder.Import(ctx, importFile, tag)
	if err != nil {
		return errx.Wrap(ErrImportImage, err)
	}

	fmt.Printf("Successfully built and tagged %s\n", tag)
	fmt.Printf("Layers: %d\n", len(importResult.LowerPaths))
	fmt.Printf("Size: %.1f MB\n", float64(importResult.Size)/(1024*1024))
	return nil
}
