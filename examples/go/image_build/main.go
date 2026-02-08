package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jingkaihe/matchlock/pkg/sdk"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := sdk.DefaultConfig()
	if os.Getenv("MATCHLOCK_BIN") == "" {
		cfg.BinaryPath = "./bin/matchlock"
	}

	client, err := sdk.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	defer client.Close()

	// Create a temporary build context with a Dockerfile
	contextDir, err := os.MkdirTemp("", "matchlock-example-build-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(contextDir)

	dockerfile := filepath.Join(contextDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte(`FROM alpine:latest
RUN apk add --no-cache curl jq
RUN echo "Built by matchlock SDK" > /built-by.txt
CMD ["cat", "/built-by.txt"]
`), 0644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}

	// Build the image from Dockerfile
	slog.Info("building image from Dockerfile", "context", contextDir)
	result, err := client.BuildDockerfile(sdk.DockerfileBuildOptions{
		ContextDir: contextDir,
		Dockerfile: dockerfile,
		Tag:        "example-app:latest",
	})
	if err != nil {
		return fmt.Errorf("dockerfile build: %w", err)
	}
	slog.Info("build complete",
		"rootfs", result.RootfsPath,
		"digest", result.Digest,
		"size_mb", result.Size/(1024*1024),
	)

	// List images to confirm it's cached
	images, err := client.ImageList()
	if err != nil {
		return fmt.Errorf("image list: %w", err)
	}
	slog.Info("cached images", "count", len(images))
	for _, img := range images {
		fmt.Printf("  %-30s  %-10s  %.1f MB\n", img.Tag, img.Source, float64(img.Size)/(1024*1024))
	}

	// Launch a sandbox from the freshly built image
	sandbox := sdk.New("example-app:latest")
	vmID, err := client.Launch(sandbox)
	if err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	defer client.Remove()
	slog.Info("sandbox ready", "vm", vmID)

	// Verify the built image contents
	res, err := client.Exec("cat /built-by.txt")
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}
	fmt.Printf("Output: %s", res.Stdout)

	// Check that the tools installed in the Dockerfile are available
	res, err = client.Exec("curl --version | head -1")
	if err != nil {
		return fmt.Errorf("exec curl: %w", err)
	}
	fmt.Printf("curl: %s", res.Stdout)

	res, err = client.Exec("jq --version")
	if err != nil {
		return fmt.Errorf("exec jq: %w", err)
	}
	fmt.Printf("jq: %s", res.Stdout)

	// Clean up the built image
	if err := client.ImageRemove("example-app:latest"); err != nil {
		return fmt.Errorf("image remove: %w", err)
	}
	slog.Info("cleaned up", "tag", "example-app:latest")

	return nil
}
