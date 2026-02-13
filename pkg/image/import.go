package image

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func (b *Builder) Import(ctx context.Context, reader io.Reader, tag string) (*BuildResult, error) {
	tmpTar, err := os.CreateTemp("", "matchlock-import-*.tar")
	if err != nil {
		return nil, fmt.Errorf("%w: tarball: %w", ErrCreateTemp, err)
	}
	defer os.Remove(tmpTar.Name())

	if _, err := io.Copy(tmpTar, reader); err != nil {
		tmpTar.Close()
		return nil, fmt.Errorf("%w: read: %w", ErrTarball, err)
	}
	tmpTar.Close()

	img, err := tarball.ImageFromPath(tmpTar.Name(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: load image: %w", ErrTarball, err)
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrImageDigest, err)
	}

	extractDir, err := os.MkdirTemp("", "matchlock-extract-*")
	if err != nil {
		return nil, fmt.Errorf("%w: dir: %w", ErrCreateTemp, err)
	}
	defer os.RemoveAll(extractDir)

	fileMetas, err := b.extractImage(img, extractDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrExtract, err)
	}

	rootfsTmp, err := os.CreateTemp("", "matchlock-rootfs-*.ext4")
	if err != nil {
		return nil, fmt.Errorf("%w: rootfs: %w", ErrCreateTemp, err)
	}
	rootfsTmp.Close()
	rootfsPath := rootfsTmp.Name()

	if err := b.createExt4(extractDir, rootfsPath, fileMetas); err != nil {
		os.Remove(rootfsPath)
		return nil, fmt.Errorf("%w: %w", ErrCreateExt4, err)
	}

	ociConfig := extractOCIConfig(img)

	meta := ImageMeta{
		Digest: digest.String(),
		Source: "import",
		OCI:    ociConfig,
	}
	if err := b.store.Save(tag, rootfsPath, meta); err != nil {
		os.Remove(rootfsPath)
		return nil, fmt.Errorf("%w: %w", ErrStoreSave, err)
	}
	os.Remove(rootfsPath)

	return b.store.Get(tag)
}
