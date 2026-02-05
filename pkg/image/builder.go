package image

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Builder struct {
	cacheDir       string
	guestAgentPath string
	guestFusedPath string
}

type BuildOptions struct {
	CacheDir       string
	GuestAgentPath string
	GuestFusedPath string
}

func NewBuilder(opts *BuildOptions) *Builder {
	cacheDir := opts.CacheDir
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache", "matchlock", "images")
	}
	return &Builder{
		cacheDir:       cacheDir,
		guestAgentPath: opts.GuestAgentPath,
		guestFusedPath: opts.GuestFusedPath,
	}
}

type BuildResult struct {
	RootfsPath string
	Digest     string
	Size       int64
	Cached     bool
}

func (b *Builder) Build(ctx context.Context, imageRef string) (*BuildResult, error) {
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, fmt.Errorf("parse image reference: %w", err)
	}

	img, err := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("get image digest: %w", err)
	}

	rootfsPath := filepath.Join(b.cacheDir, sanitizeRef(imageRef), digest.Hex[:12]+".ext4")

	if _, err := os.Stat(rootfsPath); err == nil {
		fi, _ := os.Stat(rootfsPath)
		return &BuildResult{
			RootfsPath: rootfsPath,
			Digest:     digest.String(),
			Size:       fi.Size(),
			Cached:     true,
		}, nil
	}

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

	if err := b.injectMatchlockComponents(extractDir); err != nil {
		return nil, fmt.Errorf("inject components: %w", err)
	}

	if err := b.createExt4(extractDir, rootfsPath); err != nil {
		os.Remove(rootfsPath)
		return nil, fmt.Errorf("create ext4: %w", err)
	}

	fi, _ := os.Stat(rootfsPath)
	return &BuildResult{
		RootfsPath: rootfsPath,
		Digest:     digest.String(),
		Size:       fi.Size(),
	}, nil
}

func (b *Builder) extractImage(img v1.Image, destDir string) error {
	reader := mutate.Extract(img)
	defer reader.Close()

	// Use --numeric-owner to preserve UIDs/GIDs from the container image
	cmd := exec.Command("tar", "-xf", "-", "-C", destDir, "--numeric-owner")
	cmd.Stdin = reader
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("extract tar: %w", err)
	}

	return nil
}

func (b *Builder) injectMatchlockComponents(rootDir string) error {
	dirs := []string{
		filepath.Join(rootDir, "opt", "matchlock"),
		filepath.Join(rootDir, "sbin"),
		filepath.Join(rootDir, "run"),
		filepath.Join(rootDir, "proc"),
		filepath.Join(rootDir, "sys"),
		filepath.Join(rootDir, "dev"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}

	if b.guestAgentPath != "" {
		if err := copyFile(b.guestAgentPath, filepath.Join(rootDir, "opt", "matchlock", "guest-agent"), 0755); err != nil {
			return fmt.Errorf("copy guest-agent: %w", err)
		}
	}

	if b.guestFusedPath != "" {
		if err := copyFile(b.guestFusedPath, filepath.Join(rootDir, "opt", "matchlock", "guest-fused"), 0755); err != nil {
			return fmt.Errorf("copy guest-fused: %w", err)
		}
	}

	// Configure DNS - container images often have broken/empty resolv.conf
	resolvConf := filepath.Join(rootDir, "etc", "resolv.conf")
	os.Remove(resolvConf) // Remove if it's a symlink
	if err := os.WriteFile(resolvConf, []byte("nameserver 8.8.8.8\nnameserver 8.8.4.4\n"), 0644); err != nil {
		return fmt.Errorf("write resolv.conf: %w", err)
	}

	initScript := `#!/bin/sh
# Matchlock minimal init - runs as PID 1
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev 2>/dev/null || true

mkdir -p /dev/pts /dev/shm
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /dev/shm
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /tmp

hostname matchlock

# Ensure network interface is up
ip link set eth0 up 2>/dev/null || ifconfig eth0 up 2>/dev/null

/opt/matchlock/guest-fused &
exec /opt/matchlock/guest-agent
`
	initPath := filepath.Join(rootDir, "sbin", "matchlock-init")
	if err := os.WriteFile(initPath, []byte(initScript), 0755); err != nil {
		return fmt.Errorf("write init script: %w", err)
	}

	originalInit := filepath.Join(rootDir, "sbin", "init")
	if err := os.Remove(originalInit); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing init: %w", err)
	}
	if err := os.Symlink("/sbin/matchlock-init", originalInit); err != nil {
		return fmt.Errorf("symlink init: %w", err)
	}

	return nil
}

func (b *Builder) createExt4(sourceDir, destPath string) error {
	var totalSize int64
	filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			totalSize += info.Size()
		}
		return nil
	})

	sizeMB := (totalSize / (1024 * 1024)) + 512
	if sizeMB < 256 {
		sizeMB = 256
	}

	tmpPath := destPath + ".tmp"

	cmd := exec.Command("dd", "if=/dev/zero", "of="+tmpPath, "bs=1M", fmt.Sprintf("count=%d", sizeMB), "conv=sparse")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create sparse file: %w: %s", err, out)
	}

	cmd = exec.Command("mkfs.ext4", "-F", "-q", tmpPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("mkfs.ext4: %w: %s", err, out)
	}

	mountDir, err := os.MkdirTemp("", "matchlock-mount-*")
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("create mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	cmd = exec.Command("mount", "-o", "loop", tmpPath, mountDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("mount: %w: %s", err, out)
	}
	defer exec.Command("umount", mountDir).Run()

	cmd = exec.Command("cp", "-a", sourceDir+"/.", mountDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy files: %w: %s", err, out)
	}

	cmd = exec.Command("umount", mountDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("umount: %w: %s", err, out)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

func sanitizeRef(ref string) string {
	ref = strings.ReplaceAll(ref, "/", "_")
	ref = strings.ReplaceAll(ref, ":", "_")
	return ref
}
