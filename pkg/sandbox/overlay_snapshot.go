package sandbox

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jingkaihe/matchlock/internal/errx"
	"github.com/jingkaihe/matchlock/pkg/api"
)

func prepareOverlaySnapshots(config *api.Config, stateDir string) ([]string, error) {
	if config == nil || config.VFS == nil || len(config.VFS.Mounts) == 0 {
		return nil, nil
	}

	var snapshots []string
	snapshotRoot := filepath.Join(stateDir, "overlay")
	index := 0

	for guestPath, mount := range config.VFS.Mounts {
		if mount.Type != api.MountTypeOverlay {
			continue
		}
		if mount.HostPath == "" {
			cleanupSnapshotPaths(snapshots)
			return nil, errx.With(ErrPrepareOverlayMount, ": %s: host_path is required for overlay mounts", guestPath)
		}
		if mount.Upper != nil || mount.Lower != nil {
			cleanupSnapshotPaths(snapshots)
			return nil, errx.With(ErrPrepareOverlayMount, ": %s: upper/lower overlay config is not supported; use host_path", guestPath)
		}

		if err := os.MkdirAll(snapshotRoot, 0700); err != nil {
			cleanupSnapshotPaths(snapshots)
			return nil, errx.Wrap(ErrPrepareOverlayMount, err)
		}

		dstPath := filepath.Join(snapshotRoot, fmt.Sprintf("mount-%03d", index))
		if err := copySnapshotPath(mount.HostPath, dstPath); err != nil {
			cleanupSnapshotPaths(append(snapshots, dstPath))
			return nil, err
		}

		mount.Type = api.MountTypeHostFS
		mount.HostPath = dstPath
		mount.Readonly = false
		mount.Upper = nil
		mount.Lower = nil
		config.VFS.Mounts[guestPath] = mount

		snapshots = append(snapshots, dstPath)
		index++
	}

	return snapshots, nil
}

func copySnapshotPath(srcPath, dstPath string) error {
	info, err := os.Lstat(srcPath)
	if err != nil {
		return errx.Wrap(ErrCopyOverlaySource, err)
	}
	return copySnapshotNode(srcPath, dstPath, info)
}

func copySnapshotNode(srcPath, dstPath string, info os.FileInfo) error {
	mode := info.Mode()

	if mode&os.ModeSymlink != 0 {
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		target, err := os.Readlink(srcPath)
		if err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		if err := os.Symlink(target, dstPath); err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		return nil
	}

	if mode.IsDir() {
		if err := os.MkdirAll(dstPath, mode.Perm()); err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		entries, err := os.ReadDir(srcPath)
		if err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		for _, entry := range entries {
			childSrc := filepath.Join(srcPath, entry.Name())
			childInfo, err := os.Lstat(childSrc)
			if err != nil {
				return errx.Wrap(ErrCopyOverlaySource, err)
			}
			childDst := filepath.Join(dstPath, entry.Name())
			if err := copySnapshotNode(childSrc, childDst, childInfo); err != nil {
				return err
			}
		}
		if err := os.Chmod(dstPath, mode.Perm()); err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		return nil
	}

	if mode.IsRegular() {
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		src, err := os.Open(srcPath)
		if err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		defer src.Close()

		dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
		if err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			_ = dst.Close()
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		if err := dst.Close(); err != nil {
			return errx.Wrap(ErrCopyOverlaySource, err)
		}
		return nil
	}

	return errx.With(ErrCopyOverlaySource, ": unsupported file type: %s", srcPath)
}

func cleanupSnapshotPaths(paths []string) {
	for _, p := range paths {
		_ = os.RemoveAll(p)
	}
}
