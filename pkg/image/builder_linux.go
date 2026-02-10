//go:build linux

package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
)

// platformOptions returns remote options for linux (uses default platform detection)
func (b *Builder) platformOptions() []remote.Option {
	return nil
}

// createExt4 creates an ext4 filesystem using debugfs (no root required)
func (b *Builder) createExt4(sourceDir, destPath string, meta map[string]fileMeta) error {
	mke2fsPath, err := exec.LookPath("mke2fs")
	if err != nil {
		mke2fsPath, err = exec.LookPath("mkfs.ext4")
		if err != nil {
			return fmt.Errorf("mke2fs/mkfs.ext4 not found; install e2fsprogs")
		}
	}

	debugfsPath, err := exec.LookPath("debugfs")
	if err != nil {
		return fmt.Errorf("debugfs not found; install e2fsprogs")
	}

	var totalSize int64
	lstatWalk(sourceDir, func(path string, info os.FileInfo) {
		totalSize += info.Size()
	})

	sizeMB := (totalSize / (1024 * 1024)) + 64

	tmpPath := destPath + "." + uuid.New().String() + ".tmp"

	cmd := exec.Command("dd", "if=/dev/zero", "of="+tmpPath, "bs=1M", fmt.Sprintf("count=%d", sizeMB), "conv=sparse")
	cmd.Stderr = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("create sparse file: %w: %s", err, out)
	}

	cmd = exec.Command(mke2fsPath, "-t", "ext4", "-F", "-q", tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("mke2fs: %w: %s", err, out)
	}

	var debugfsCommands strings.Builder

	err = lstatWalkErr(sourceDir, func(path string, info os.FileInfo) error {
		relPath, _ := filepath.Rel(sourceDir, path)
		if relPath == "." {
			return nil
		}

		ext4Path := "/" + relPath

		if hasDebugfsUnsafeChars(ext4Path) {
			return nil
		}

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

		if fm, ok := meta[ext4Path]; ok {
			debugfsCommands.WriteString(fmt.Sprintf("set_inode_field %s uid %d\n", ext4Path, fm.uid))
			debugfsCommands.WriteString(fmt.Sprintf("set_inode_field %s gid %d\n", ext4Path, fm.gid))
			var typeBits uint32
			if info.IsDir() {
				typeBits = 0o040000
			} else if info.Mode()&os.ModeSymlink != 0 {
				typeBits = 0o120000
			} else {
				typeBits = 0o100000
			}
			debugfsCommands.WriteString(fmt.Sprintf("set_inode_field %s mode 0%o\n", ext4Path, typeBits|uint32(fm.mode)))
		}
		return nil
	})
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("walk source dir: %w", err)
	}

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
