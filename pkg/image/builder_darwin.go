//go:build darwin

package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
)

func (b *Builder) platformOptions() []remote.Option {
	return []remote.Option{
		remote.WithPlatform(v1.Platform{
			OS:           "linux",
			Architecture: runtime.GOARCH,
		}),
	}
}

// createExt4 creates an ext4 filesystem on macOS using e2fsprogs
// Requires: brew install e2fsprogs && brew link e2fsprogs
func (b *Builder) createExt4(sourceDir, destPath string) error {
	// Check for mke2fs in PATH
	mke2fsPath, err := exec.LookPath("mke2fs")
	if err != nil {
		return fmt.Errorf("mke2fs not found in PATH; install e2fsprogs: brew install e2fsprogs && brew link e2fsprogs")
	}

	debugfsPath, err := exec.LookPath("debugfs")
	if err != nil {
		return fmt.Errorf("debugfs not found in PATH; install e2fsprogs: brew install e2fsprogs && brew link e2fsprogs")
	}

	// Calculate size
	var totalSize int64
	filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			totalSize += info.Size()
		}
		return nil
	})

	sizeMB := (totalSize / (1024 * 1024)) + 64

	tmpPath := destPath + "." + uuid.New().String() + ".tmp"

	cmd := exec.Command("dd", "if=/dev/zero", "of="+tmpPath, "bs=1M", fmt.Sprintf("count=%d", sizeMB), "conv=sparse")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("create sparse file: %w: %s", err, out)
	}

	// Create ext4 filesystem
	cmd = exec.Command(mke2fsPath, "-t", "ext4", "-F", "-q", tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("mke2fs: %w: %s", err, out)
	}

	// Build debugfs commands to copy all files
	var debugfsCommands strings.Builder

	err = filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, _ := filepath.Rel(sourceDir, path)
		if relPath == "." {
			return nil
		}

		// Normalize path for ext4 (use forward slashes, ensure leading /)
		ext4Path := "/" + strings.ReplaceAll(relPath, "\\", "/")

		if info.IsDir() {
			debugfsCommands.WriteString(fmt.Sprintf("mkdir %s\n", ext4Path))
		} else if info.Mode().IsRegular() {
			debugfsCommands.WriteString(fmt.Sprintf("write %s %s\n", path, ext4Path))
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err == nil {
				debugfsCommands.WriteString(fmt.Sprintf("symlink %s %s\n", ext4Path, target))
			}
		}
		return nil
	})
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("walk source dir: %w", err)
	}

	// Run debugfs to populate the filesystem
	cmd = exec.Command(debugfsPath, "-w", "-f", "/dev/stdin", tmpPath)
	cmd.Stdin = strings.NewReader(debugfsCommands.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("debugfs: %w: %s", err, out)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}
