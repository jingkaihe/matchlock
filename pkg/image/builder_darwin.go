//go:build darwin

package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
	"github.com/jingkaihe/matchlock/internal/errx"
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
func (b *Builder) createExt4(sourceDir, destPath string, meta map[string]fileMeta, specials map[string]layerSpecial) error {
	// Check for mke2fs in PATH
	mke2fsPath, err := exec.LookPath("mke2fs")
	if err != nil {
		return errx.With(ErrToolNotFound, ": mke2fs not in PATH; install e2fsprogs: brew install e2fsprogs && brew link e2fsprogs")
	}

	debugfsPath, err := exec.LookPath("debugfs")
	if err != nil {
		return errx.With(ErrToolNotFound, ": debugfs not in PATH; install e2fsprogs: brew install e2fsprogs && brew link e2fsprogs")
	}

	// Calculate size (use Lstat-based walk to avoid following symlinks)
	var totalSize int64
	lstatWalk(sourceDir, func(path string, info os.FileInfo) {
		totalSize += info.Size()
	})

	sizeMB := estimateLayerImageSizeMB(totalSize)

	tmpPath := destPath + "." + uuid.New().String() + ".tmp"

	cmd := exec.Command("dd", "if=/dev/zero", "of="+tmpPath, "bs=1M", fmt.Sprintf("count=%d", sizeMB), "conv=sparse")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return errx.With(ErrCreateExt4, ": create sparse file: %w: %s", err, out)
	}

	// Create ext4 filesystem
	cmd = exec.Command(mke2fsPath, "-t", "ext4", "-F", "-q", tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return errx.With(ErrCreateExt4, ": mke2fs: %w: %s", err, out)
	}

	// Build debugfs commands to copy all files
	var debugfsCommands strings.Builder

	err = lstatWalkErr(sourceDir, func(path string, info os.FileInfo) error {
		relPath, _ := filepath.Rel(sourceDir, path)
		if relPath == "." {
			return nil
		}

		ext4Path := "/" + strings.ReplaceAll(relPath, "\\", "/")

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
		return errx.With(ErrCreateExt4, ": walk source dir: %w", err)
	}

	if len(specials) > 0 {
		specialPaths := make([]string, 0, len(specials))
		for ext4Path := range specials {
			specialPaths = append(specialPaths, ext4Path)
		}
		sort.Strings(specialPaths)

		for _, ext4Path := range specialPaths {
			if hasDebugfsUnsafeChars(ext4Path) {
				continue
			}
			special := specials[ext4Path]
			switch special.kind {
			case layerSpecialWhiteout:
				relPath := strings.TrimPrefix(ext4Path, "/")
				if relPath == "" {
					continue
				}
				debugfsCommands.WriteString(fmt.Sprintf("mknod %s c 0 0\n", relPath))
				debugfsCommands.WriteString(fmt.Sprintf("set_inode_field %s uid %d\n", ext4Path, special.uid))
				debugfsCommands.WriteString(fmt.Sprintf("set_inode_field %s gid %d\n", ext4Path, special.gid))
				mode := uint32(0o20000 | (uint32(special.mode) & 0o7777))
				debugfsCommands.WriteString(fmt.Sprintf("set_inode_field %s mode 0%o\n", ext4Path, mode))
			case layerSpecialOpaque:
				target := ext4Path
				if target == "" {
					target = "/"
				}
				debugfsCommands.WriteString(fmt.Sprintf("ea_set %s trusted.overlay.opaque y\n", target))
			}
		}
	}

	// Run debugfs to populate the filesystem
	cmd = exec.Command(debugfsPath, "-w", "-f", "/dev/stdin", tmpPath)
	cmd.Stdin = strings.NewReader(debugfsCommands.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return errx.With(ErrCreateExt4, ": debugfs: %w: %s", err, out)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return errx.With(ErrCreateExt4, ": rename: %w", err)
	}

	return nil
}

func (b *Builder) createEROFSFromTar(tarPath, destPath string) error {
	mkfsPath, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		return errx.With(ErrToolNotFound, ": mkfs.erofs not in PATH; install erofs-utils")
	}

	tmpPath := destPath + "." + uuid.New().String() + ".tmp"
	cmd := exec.Command(mkfsPath, "--quiet", "--tar=f", tmpPath, tarPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmpPath)
		return errx.With(ErrCreateExt4, ": mkfs.erofs: %w: %s", err, out)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return errx.With(ErrCreateExt4, ": rename erofs image: %w", err)
	}
	return nil
}
