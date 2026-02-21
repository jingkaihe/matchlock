//go:build linux

package image

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/uuid"
	"github.com/jingkaihe/matchlock/internal/errx"
)

// platformOptions returns remote options for linux (uses default platform detection)
func (b *Builder) platformOptions() []remote.Option {
	return nil
}

func (b *Builder) createEROFS(sourceDir, destPath string, meta map[string]fileMeta) error {
	mkfsPath, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		return errx.With(ErrToolNotFound, ": mkfs.erofs; install erofs-utils")
	}

	// Best-effort mode sync so mkfs.erofs captures image file modes.
	for relPath, fm := range meta {
		if relPath == "" || relPath == "/" {
			continue
		}
		hostPath := filepath.Join(sourceDir, strings.TrimPrefix(relPath, "/"))
		_ = os.Chmod(hostPath, fm.mode)
	}

	tmpPath := destPath + "." + uuid.New().String() + ".tmp"
	cmd := exec.Command(mkfsPath, "--quiet", tmpPath, sourceDir)
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

// createExt4 creates an ext4 filesystem using debugfs (no root required)
func (b *Builder) createExt4(sourceDir, destPath string, meta map[string]fileMeta, specials map[string]layerSpecial) error {
	mke2fsPath, err := exec.LookPath("mke2fs")
	if err != nil {
		mke2fsPath, err = exec.LookPath("mkfs.ext4")
		if err != nil {
			return errx.With(ErrToolNotFound, ": mke2fs/mkfs.ext4; install e2fsprogs")
		}
	}

	debugfsPath, err := exec.LookPath("debugfs")
	if err != nil {
		return errx.With(ErrToolNotFound, ": debugfs; install e2fsprogs")
	}

	var totalSize int64
	lstatWalk(sourceDir, func(path string, info os.FileInfo) {
		totalSize += info.Size()
	})

	sizeMB := estimateLayerImageSizeMB(totalSize)

	tmpPath := destPath + "." + uuid.New().String() + ".tmp"

	cmd := exec.Command("dd", "if=/dev/zero", "of="+tmpPath, "bs=1M", fmt.Sprintf("count=%d", sizeMB), "conv=sparse")
	cmd.Stderr = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return errx.With(ErrCreateExt4, ": create sparse file: %w: %s", err, out)
	}

	cmd = exec.Command(mke2fsPath, "-t", "ext4", "-F", "-q", tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return errx.With(ErrCreateExt4, ": mke2fs: %w: %s", err, out)
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
