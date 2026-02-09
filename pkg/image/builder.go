package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Builder struct {
	cacheDir  string
	forcePull bool
	store     *Store
}

type BuildOptions struct {
	CacheDir  string
	ForcePull bool
}

func NewBuilder(opts *BuildOptions) *Builder {
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache", "matchlock", "images")
	}
	return &Builder{
		cacheDir:  cacheDir,
		forcePull: opts.ForcePull,
		store:     NewStore(""),
	}
}

type BuildResult struct {
	RootfsPath string
	Digest     string
	Size       int64
	Cached     bool
}

func (b *Builder) Build(ctx context.Context, imageRef string) (*BuildResult, error) {
	if !b.forcePull {
		if result, err := b.store.Get(imageRef); err == nil {
			return result, nil
		}
	}

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parse image reference: %w", err)
	}

	cacheDir := filepath.Join(b.cacheDir, sanitizeRef(imageRef))
	if !b.forcePull {
		if entries, err := os.ReadDir(cacheDir); err == nil {
			for _, e := range entries {
				if filepath.Ext(e.Name()) == ".ext4" {
					rootfsPath := filepath.Join(cacheDir, e.Name())
					fi, _ := os.Stat(rootfsPath)
					return &BuildResult{
						RootfsPath: rootfsPath,
						Digest:     strings.TrimSuffix(e.Name(), ".ext4"),
						Size:       fi.Size(),
						Cached:     true,
					}, nil
				}
			}
		}
	}

	remoteOpts := []remote.Option{
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
	}
	remoteOpts = append(remoteOpts, b.platformOptions()...)

	img, err := remote.Image(ref, remoteOpts...)
	if err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("get image digest: %w", err)
	}

	rootfsPath := filepath.Join(cacheDir, digest.Hex[:12]+".ext4")

	if err := os.MkdirAll(filepath.Dir(rootfsPath), 0755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	extractDir, err := os.MkdirTemp("", "matchlock-extract-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(extractDir)

	if err := b.extractImage(img, extractDir); err != nil {
		return nil, fmt.Errorf("extract image: %w", err)
	}

	if err := b.createExt4(extractDir, rootfsPath); err != nil {
		os.Remove(rootfsPath)
		return nil, fmt.Errorf("create ext4: %w", err)
	}

	fi, _ := os.Stat(rootfsPath)

	meta := ImageMeta{
		Tag:       imageRef,
		Digest:    digest.String(),
		Size:      fi.Size(),
		CreatedAt: time.Now(),
		Source:    "registry",
	}
	if metaBytes, err := json.MarshalIndent(meta, "", "  "); err == nil {
		os.WriteFile(filepath.Join(cacheDir, "metadata.json"), metaBytes, 0644)
	}

	return &BuildResult{
		RootfsPath: rootfsPath,
		Digest:     digest.String(),
		Size:       fi.Size(),
	}, nil
}

func (b *Builder) extractImage(img v1.Image, destDir string) error {
	reader := mutate.Extract(img)
	defer reader.Close()

	cmd := exec.Command("tar", "-xf", "-", "-C", destDir, "--numeric-owner")
	cmd.Stdin = reader
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract tar: %w", err)
	}

	return nil
}

func (b *Builder) SaveTag(tag string, result *BuildResult) error {
	meta := ImageMeta{
		Digest: result.Digest,
		Source: "tag",
	}
	return b.store.Save(tag, result.RootfsPath, meta)
}

func (b *Builder) Store() *Store {
	return b.store
}

func sanitizeRef(ref string) string {
	ref = strings.ReplaceAll(ref, "/", "_")
	ref = strings.ReplaceAll(ref, ":", "_")
	return ref
}
