package kernel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	Version = "6.19.8"

	DefaultRegistry = "ghcr.io/jingkaihe/matchlock"
)

type Architecture string

const (
	ArchX86_64 Architecture = "x86_64"
	ArchARM64  Architecture = "arm64"
)

func CurrentArch() Architecture {
	if runtime.GOARCH == "arm64" {
		return ArchARM64
	}
	return ArchX86_64
}

func (a Architecture) KernelFilename() string {
	if a == ArchARM64 {
		return "kernel-arm64"
	}
	return "kernel"
}

func (a Architecture) OCIPlatform() string {
	if a == ArchARM64 {
		return "linux/arm64"
	}
	return "linux/amd64"
}

type Manager struct {
	cacheDir string
	registry string
	db       *sql.DB
	initErr  error
}

type Option func(*Manager)

func WithCacheDir(dir string) Option {
	return func(m *Manager) {
		m.cacheDir = dir
	}
}

func WithRegistry(registry string) Option {
	return func(m *Manager) {
		m.registry = registry
	}
}

func NewManager(opts ...Option) *Manager {
	home := getUserHome()
	m := &Manager{
		cacheDir: filepath.Join(home, ".cache", "matchlock"),
		registry: DefaultRegistry,
	}
	for _, opt := range opts {
		opt(m)
	}
	db, err := openKernelDB(m.cacheDir)
	m.db = db
	m.initErr = err
	return m
}

func getUserHome() string {
	// When running with sudo, prefer the original user's home
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" && os.Getuid() == 0 {
		return filepath.Join("/home", sudoUser)
	}
	home, _ := os.UserHomeDir()
	return home
}

func (m *Manager) KernelPath(arch Architecture, version string) string {
	if version == "" {
		version = Version
	}
	return filepath.Join(m.cacheDir, "kernels", version, arch.KernelFilename())
}

func (m *Manager) EnsureKernel(ctx context.Context, arch Architecture, version string) (string, error) {
	if version == "" {
		version = Version
	}

	kernelPath := m.KernelPath(arch, version)
	imageRef := fmt.Sprintf("%s/kernel:%s", m.registry, version)

	if _, err := os.Stat(kernelPath); err == nil {
		m.recordVersionCache(version, arch, kernelPath, sizeOnDisk(kernelPath), imageRef, "")
		return kernelPath, nil
	}

	digest, size, err := m.download(ctx, arch, version, kernelPath)
	if err != nil {
		return "", errx.Wrap(ErrDownloadKernel, err)
	}
	m.recordVersionCache(version, arch, kernelPath, size, imageRef, digest)

	return kernelPath, nil
}

func (m *Manager) EnsureKernelRef(ctx context.Context, arch Architecture, ref string) (string, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return m.EnsureKernel(ctx, arch, Version)
	}

	if strings.HasPrefix(trimmed, "file://") {
		path, err := parseFileRef(trimmed)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			return "", errx.With(ErrKernelNotFoundOnDisk, ": %s: %v", path, err)
		}
		return path, nil
	}

	destPath := m.KernelRefPath(arch, trimmed)
	if _, err := os.Stat(destPath); err == nil {
		m.recordRefCache(trimmed, arch, destPath, sizeOnDisk(destPath), "")
		return destPath, nil
	}

	digest, size, err := m.downloadRef(ctx, arch, trimmed, destPath)
	if err != nil {
		return "", errx.Wrap(ErrDownloadKernel, err)
	}
	m.recordRefCache(trimmed, arch, destPath, size, digest)

	return destPath, nil
}

func (m *Manager) KernelRefPath(arch Architecture, ref string) string {
	sum := sha256.Sum256([]byte(ref))
	refHash := hex.EncodeToString(sum[:8])
	return filepath.Join(m.cacheDir, "kernels", "refs", refHash, arch.KernelFilename())
}

func (m *Manager) download(ctx context.Context, arch Architecture, version string, destPath string) (string, int64, error) {
	imageRef := fmt.Sprintf("%s/kernel:%s", m.registry, version)
	return m.downloadRef(ctx, arch, imageRef, destPath)
}

func (m *Manager) downloadRef(ctx context.Context, arch Architecture, imageRef, destPath string) (string, int64, error) {

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return "", 0, errx.With(ErrParseReference, " %s: %w", imageRef, err)
	}

	platform, err := v1.ParsePlatform(arch.OCIPlatform())
	if err != nil {
		return "", 0, errx.Wrap(ErrParsePlatform, err)
	}

	desc, err := remote.Get(ref,
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
		remote.WithPlatform(*platform),
	)
	if err != nil {
		return "", 0, errx.Wrap(ErrGetDescriptor, err)
	}
	sourceDigest := desc.Digest.String()

	img, err := desc.Image()
	if err != nil {
		return "", 0, errx.Wrap(ErrGetImage, err)
	}

	layers, err := img.Layers()
	if err != nil {
		return "", 0, errx.Wrap(ErrGetLayers, err)
	}

	if len(layers) == 0 {
		return "", 0, ErrNoLayers
	}

	layer := layers[0]
	rc, err := layer.Uncompressed()
	if err != nil {
		return "", 0, errx.Wrap(ErrUncompressLayer, err)
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return "", 0, errx.Wrap(ErrCreateDirectory, err)
	}

	// Read layer content into buffer to try multiple formats
	content, err := io.ReadAll(rc)
	if err != nil {
		return "", 0, errx.Wrap(ErrReadLayer, err)
	}

	kernelFilename := arch.KernelFilename()

	// Try gzipped tarball first (new format)
	if err := extractKernelFromTarGz(content, destPath, kernelFilename); err == nil {
		return sourceDigest, sizeOnDisk(destPath), nil
	}

	// Try plain tarball (uncompressed)
	if err := extractKernelFromTar(content, destPath, kernelFilename); err == nil {
		return sourceDigest, sizeOnDisk(destPath), nil
	}

	// Fallback: treat as raw kernel binary (old format)
	tmpPath := destPath + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0644); err != nil {
		return "", 0, errx.Wrap(ErrWriteKernel, err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return "", 0, errx.Wrap(ErrRenameKernel, err)
	}

	return sourceDigest, sizeOnDisk(destPath), nil
}

func extractKernelFromTarGz(data []byte, destPath, kernelFilename string) error {
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gr.Close()

	return extractFromTarReader(tar.NewReader(gr), destPath, kernelFilename)
}

func extractKernelFromTar(data []byte, destPath, kernelFilename string) error {
	return extractFromTarReader(tar.NewReader(bytes.NewReader(data)), destPath, kernelFilename)
}

func extractFromTarReader(tr *tar.Reader, destPath, kernelFilename string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		if filepath.Base(hdr.Name) == kernelFilename {
			tmpPath := destPath + ".tmp"
			f, err := os.Create(tmpPath)
			if err != nil {
				return errx.Wrap(ErrCreateFile, err)
			}

			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				os.Remove(tmpPath)
				return err
			}

			if err := os.Rename(tmpPath, destPath); err != nil {
				os.Remove(tmpPath)
				return err
			}
			return nil
		}
	}

	return errx.With(ErrKernelNotFound, ": %s", kernelFilename)
}

func (m *Manager) ListCachedVersions() ([]string, error) {
	kernelsDir := filepath.Join(m.cacheDir, "kernels")
	entries, err := os.ReadDir(kernelsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var versions []string
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != "refs" {
			versions = append(versions, entry.Name())
		}
	}
	return versions, nil
}

func (m *Manager) CleanCache(version string) error {
	if version == "" {
		return os.RemoveAll(filepath.Join(m.cacheDir, "kernels"))
	}
	return os.RemoveAll(filepath.Join(m.cacheDir, "kernels", version))
}

func ResolveKernelPath(ctx context.Context) (string, error) {
	mgr := NewManager()
	return mgr.EnsureKernel(ctx, CurrentArch(), Version)
}

func ResolveKernelRef(ctx context.Context, ref string) (string, error) {
	return NewManager().EnsureKernelRef(ctx, CurrentArch(), ref)
}

func ImageReference(version string) string {
	if version == "" {
		version = Version
	}
	return fmt.Sprintf("%s/kernel:%s", DefaultRegistry, version)
}

func ParseVersion(ref string) string {
	if idx := strings.LastIndex(ref, ":"); idx != -1 {
		return ref[idx+1:]
	}
	return Version
}

func parseFileRef(ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil {
		return "", errx.With(ErrInvalidKernelRef, ": %v", err)
	}
	if u.Scheme != "file" {
		return "", errx.With(ErrInvalidKernelRef, ": unsupported scheme %q", u.Scheme)
	}
	if u.Host != "" {
		return "", errx.With(ErrInvalidKernelRef, ": file ref must not include host")
	}
	path := u.Path
	if path == "" {
		return "", errx.With(ErrInvalidKernelRef, ": empty file path")
	}
	if !filepath.IsAbs(path) {
		return "", errx.With(ErrInvalidKernelRef, ": file path must be absolute")
	}
	return path, nil
}

func sizeOnDisk(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
