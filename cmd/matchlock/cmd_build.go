package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
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
	if tag == "" {
		return fmt.Errorf("-t/--tag is required when building from a Dockerfile")
	}

	cpus, _ := cmd.Flags().GetInt("build-cpus")
	memory, _ := cmd.Flags().GetInt("build-memory")

	disk, _ := cmd.Flags().GetInt("build-disk")

	if cpus == 0 {
		cpus = runtime.NumCPU()
	}
	if memory == 0 {
		memory = totalMemoryMB()
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

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

	mounts := map[string]api.MountConfig{
		"/workspace":         {Type: "memory"},
		"/workspace/context": {Type: "real_fs", HostPath: absContext, Readonly: true},
		"/workspace/output":  {Type: "memory"},
	}

	guestDockerfileDir := "/workspace/context"
	if _, err := os.Stat(dockerfileInContext); os.IsNotExist(err) {
		mounts["/workspace/dockerfile"] = api.MountConfig{Type: "real_fs", HostPath: dockerfileDir, Readonly: true}
		guestDockerfileDir = "/workspace/dockerfile"
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
		Network: &api.NetworkConfig{},
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
	defer sb.Close()

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

	buildScript := fmt.Sprintf(
		`cat > /tmp/buildkit-run.sh << 'SCRIPT'
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
%s  --output type=docker,dest=/workspace/output/image.tar
RC=$?
[ $RC -ne 0 ] && { echo "=== buildkitd log ===" >&2; cat /tmp/buildkitd.log >&2; }
kill $BKPID 2>/dev/null
exit $RC
SCRIPT
`+`chmod +x /tmp/buildkit-run.sh && /tmp/buildkit-run.sh`,
		guestDockerfileDir,
		filenameOpt,
	)
	result, execErr := sb.Exec(ctx, buildScript, execOpts)
	if execErr != nil {
		return fmt.Errorf("BuildKit build: %w", execErr)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("BuildKit build failed (exit %d)", result.ExitCode)
	}

	fmt.Fprintf(os.Stderr, "Importing built image as %s...\n", tag)

	tarballData, err := sb.ReadFile(ctx, "/workspace/output/image.tar")
	if err != nil {
		return fmt.Errorf("read built image: %w", err)
	}

	tmpFile, err := os.CreateTemp("", "matchlock-build-*.tar")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write(tarballData); err != nil {
		tmpFile.Close()
		return fmt.Errorf("write temp tarball: %w", err)
	}
	tmpFile.Close()

	importFile, err := os.Open(tmpFile.Name())
	if err != nil {
		return fmt.Errorf("open temp tarball: %w", err)
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
