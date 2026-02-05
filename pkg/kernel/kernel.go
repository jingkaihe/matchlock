package kernel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	Version = "6.1.137"

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

	if _, err := os.Stat(kernelPath); err == nil {
		return kernelPath, nil
	}

	if err := m.download(ctx, arch, version, kernelPath); err != nil {
		return "", fmt.Errorf("download kernel: %w", err)
	}

	return kernelPath, nil
}

func (m *Manager) download(ctx context.Context, arch Architecture, version string, destPath string) error {
	imageRef := fmt.Sprintf("%s/kernel:%s", m.registry, version)

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return fmt.Errorf("parse reference %s: %w", imageRef, err)
	}

	platform, err := v1.ParsePlatform(arch.OCIPlatform())
	if err != nil {
		return fmt.Errorf("parse platform: %w", err)
	}

	desc, err := remote.Get(ref,
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
		remote.WithPlatform(*platform),
	)
	if err != nil {
		return fmt.Errorf("get image descriptor: %w", err)
	}

	img, err := desc.Image()
	if err != nil {
		return fmt.Errorf("get image: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return fmt.Errorf("get layers: %w", err)
	}

	if len(layers) == 0 {
		return fmt.Errorf("no layers in image")
	}

	layer := layers[0]
	rc, err := layer.Uncompressed()
	if err != nil {
		return fmt.Errorf("get uncompressed layer: %w", err)
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	// Read layer content into buffer to try multiple formats
	content, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("read layer: %w", err)
	}

	kernelFilename := arch.KernelFilename()

	// Try gzipped tarball first (new format)
	if err := extractKernelFromTarGz(content, destPath, kernelFilename); err == nil {
		return nil
	}

	// Try plain tarball (uncompressed)
	if err := extractKernelFromTar(content, destPath, kernelFilename); err == nil {
		return nil
	}

	// Fallback: treat as raw kernel binary (old format)
	tmpPath := destPath + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0644); err != nil {
		return fmt.Errorf("write kernel: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename kernel: %w", err)
	}

	return nil
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
				return fmt.Errorf("create file: %w", err)
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

	return fmt.Errorf("kernel file %s not found in archive", kernelFilename)
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
		if entry.IsDir() {
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
	if envPath := os.Getenv("MATCHLOCK_KERNEL"); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return envPath, nil
		}
	}

	mgr := NewManager()
	return mgr.EnsureKernel(ctx, CurrentArch(), Version)
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
