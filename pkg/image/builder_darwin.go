//go:build darwin

package image

import (
	"os"
	"os/exec"
	"runtime"

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

func (b *Builder) createEROFSFromTar(tarPath, destPath string) error {
	mkfsPath, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		return errx.With(ErrToolNotFound, ": mkfs.erofs not in PATH; install erofs-utils")
	}

	tmpPath := destPath + "." + uuid.New().String() + ".tmp"
	// Force 4K EROFS blocks so guest kernels with 4K page support can mount layers.
	cmd := exec.Command(mkfsPath, "--quiet", "-b", "4096", "--aufs", "--tar=f", tmpPath, tarPath)
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
