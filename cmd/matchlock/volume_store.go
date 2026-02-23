package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const defaultNamedVolumeSizeMB = 10240

var validVolumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

type namedVolume struct {
	Name      string
	Path      string
	SizeBytes int64
}

func ensureVolumeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errx.Wrap(ErrResolveVolumeDir, err)
	}
	dir := filepath.Join(home, ".cache", "matchlock", "volumes")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", errx.Wrap(ErrCreateVolumeDir, err)
	}
	return dir, nil
}

func validateVolumeName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errx.With(ErrInvalidVolumeName, ": name cannot be empty")
	}
	if !validVolumeName.MatchString(name) {
		return errx.With(ErrInvalidVolumeName, ": %q (allowed: alphanumeric, '_', '.', '-', must start with alphanumeric)", name)
	}
	return nil
}

func volumePathForName(name string) (string, error) {
	if err := validateVolumeName(name); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	dir, err := ensureVolumeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".ext4"), nil
}

func findNamedVolume(name string) (string, error) {
	path, err := volumePathForName(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", errx.With(ErrVolumeNotFound, ": %s", name)
		}
		return "", errx.Wrap(ErrResolveVolumeDir, err)
	}
	return path, nil
}

func createNamedVolume(name string, sizeMB int) (string, error) {
	if sizeMB <= 0 {
		return "", errx.With(ErrCreateVolume, ": size must be > 0 MB")
	}

	path, err := volumePathForName(name)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(path); err == nil {
		return "", errx.With(ErrVolumeExists, ": %s", name)
	} else if !os.IsNotExist(err) {
		return "", errx.Wrap(ErrCreateVolume, err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
	if err != nil {
		return "", errx.Wrap(ErrCreateVolume, err)
	}
	targetBytes := int64(sizeMB) * 1024 * 1024
	if err := f.Truncate(targetBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", errx.Wrap(ErrCreateVolume, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", errx.Wrap(ErrCreateVolume, err)
	}

	mkfsPath, err := exec.LookPath("mkfs.ext4")
	useMke2fs := false
	if err != nil {
		mkfsPath, err = exec.LookPath("mke2fs")
		useMke2fs = true
		if err != nil {
			_ = os.Remove(path)
			return "", errx.With(ErrCreateVolume, ": mkfs.ext4 or mke2fs not found; install e2fsprogs")
		}
	}

	var cmd *exec.Cmd
	if useMke2fs {
		cmd = exec.Command(mkfsPath, "-t", "ext4", "-F", "-q", path)
	} else {
		cmd = exec.Command(mkfsPath, "-F", "-q", path)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(path)
		return "", errx.With(ErrCreateVolume, ": format %s: %v: %s", path, err, strings.TrimSpace(string(out)))
	}

	return path, nil
}

func removeNamedVolume(name string) error {
	path, err := volumePathForName(name)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return errx.With(ErrVolumeNotFound, ": %s", name)
		}
		return errx.Wrap(ErrRemoveVolume, err)
	}

	return nil
}

func listNamedVolumes() ([]namedVolume, error) {
	dir, err := ensureVolumeDir()
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errx.Wrap(ErrListVolumes, err)
	}

	vols := make([]namedVolume, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ext4") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return nil, errx.Wrap(ErrListVolumes, err)
		}

		name := strings.TrimSuffix(entry.Name(), ".ext4")
		vols = append(vols, namedVolume{
			Name:      name,
			Path:      filepath.Join(dir, entry.Name()),
			SizeBytes: info.Size(),
		})
	}

	sort.Slice(vols, func(i, j int) bool {
		return vols[i].Name < vols[j].Name
	})

	return vols, nil
}

func humanizeMB(bytes int64) string {
	mb := float64(bytes) / (1024 * 1024)
	return fmt.Sprintf("%.1f MB", mb)
}
