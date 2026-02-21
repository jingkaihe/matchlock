package image

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/jingkaihe/matchlock/internal/errx"
)

func (b *Builder) Import(ctx context.Context, reader io.Reader, tag string) (*BuildResult, error) {
	_ = ctx

	tmpTar, err := os.CreateTemp("", "matchlock-import-*.tar")
	if err != nil {
		return nil, errx.With(ErrCreateTemp, ": tarball: %w", err)
	}
	defer os.Remove(tmpTar.Name())

	if _, err := io.Copy(tmpTar, reader); err != nil {
		tmpTar.Close()
		return nil, errx.With(ErrTarball, ": read: %w", err)
	}
	if err := tmpTar.Close(); err != nil {
		return nil, errx.With(ErrTarball, ": close: %w", err)
	}

	img, err := tarball.ImageFromPath(tmpTar.Name(), nil)
	if err != nil {
		return nil, errx.With(ErrTarball, ": load image: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, errx.Wrap(ErrImageDigest, err)
	}

	runtimeLayers, canonicalLayers, err := b.ingestImageLayers(img)
	if err != nil {
		return nil, err
	}

	ociConfig := extractOCIConfig(img)
	totalSize := imageSizeFromLayers(runtimeLayers)
	meta := ImageMeta{
		Digest:            digest.String(),
		Size:              totalSize,
		Source:            "import",
		CreatedAt:         time.Now().UTC(),
		OCI:               ociConfig,
		RuntimeRootfsPath: primaryRootfsPath(runtimeLayers),
	}
	if err := b.store.Save(tag, canonicalLayers, meta); err != nil {
		return nil, errx.Wrap(ErrStoreSave, err)
	}

	result, err := b.store.Get(tag)
	if err != nil {
		return nil, err
	}
	return result, nil
}
