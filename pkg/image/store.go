package image

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ImageMeta struct {
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest,omitempty"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	Source    string    `json:"source,omitempty"`
}

type ImageInfo struct {
	Tag        string
	RootfsPath string
	Meta       ImageMeta
}

type Store struct {
	baseDir string
}

func NewStore(baseDir string) *Store {
	if baseDir == "" {
		home, _ := os.UserHomeDir()
		baseDir = filepath.Join(home, ".cache", "matchlock", "images", "local")
	}
	return &Store{baseDir: baseDir}
}

func (s *Store) Save(tag string, rootfsPath string, meta ImageMeta) error {
	dir := filepath.Join(s.baseDir, sanitizeRef(tag))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create store dir: %w", err)
	}

	destPath := filepath.Join(dir, "rootfs.ext4")

	src, err := os.Open(rootfsPath)
	if err != nil {
		return fmt.Errorf("open source rootfs: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create dest rootfs: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(destPath)
		return fmt.Errorf("copy rootfs: %w", err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(destPath)
		return fmt.Errorf("flush rootfs: %w", err)
	}

	meta.Tag = tag
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now()
	}
	fi, err := os.Stat(destPath)
	if err == nil {
		meta.Size = fi.Size()
	}

	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	metaPath := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(metaPath, metaBytes, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	return nil
}

func (s *Store) Get(tag string) (*BuildResult, error) {
	dir := filepath.Join(s.baseDir, sanitizeRef(tag))

	metaPath := filepath.Join(dir, "metadata.json")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("image %q not found in local store", tag)
	}

	var meta ImageMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}

	rootfsPath := filepath.Join(dir, "rootfs.ext4")
	if _, err := os.Stat(rootfsPath); err != nil {
		return nil, fmt.Errorf("rootfs not found for %q", tag)
	}

	fi, _ := os.Stat(rootfsPath)
	return &BuildResult{
		RootfsPath: rootfsPath,
		Digest:     meta.Digest,
		Size:       fi.Size(),
		Cached:     true,
	}, nil
}

func (s *Store) List() ([]ImageInfo, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read store dir: %w", err)
	}

	var images []ImageInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		metaPath := filepath.Join(s.baseDir, e.Name(), "metadata.json")
		metaBytes, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}

		var meta ImageMeta
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			continue
		}

		rootfsPath := filepath.Join(s.baseDir, e.Name(), "rootfs.ext4")
		images = append(images, ImageInfo{
			Tag:        meta.Tag,
			RootfsPath: rootfsPath,
			Meta:       meta,
		})
	}

	sort.Slice(images, func(i, j int) bool {
		return images[i].Meta.CreatedAt.After(images[j].Meta.CreatedAt)
	})

	return images, nil
}

func (s *Store) Remove(tag string) error {
	dir := filepath.Join(s.baseDir, sanitizeRef(tag))
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("image %q not found", tag)
	}
	return os.RemoveAll(dir)
}

// ListRegistryCache lists images cached from registry pulls (non-local store).
func ListRegistryCache(cacheDir string) ([]ImageInfo, error) {
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".cache", "matchlock", "images")
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cache dir: %w", err)
	}

	var images []ImageInfo
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "local" {
			continue
		}

		subDir := filepath.Join(cacheDir, e.Name())

		if metaBytes, err := os.ReadFile(filepath.Join(subDir, "metadata.json")); err == nil {
			var meta ImageMeta
			if err := json.Unmarshal(metaBytes, &meta); err == nil {
				rootfsPath := ""
				subEntries, _ := os.ReadDir(subDir)
				for _, se := range subEntries {
					if filepath.Ext(se.Name()) == ".ext4" {
						rootfsPath = filepath.Join(subDir, se.Name())
						break
					}
				}
				images = append(images, ImageInfo{
					Tag:        meta.Tag,
					RootfsPath: rootfsPath,
					Meta:       meta,
				})
				continue
			}
		}

		subEntries, err := os.ReadDir(subDir)
		if err != nil {
			continue
		}

		for _, se := range subEntries {
			if filepath.Ext(se.Name()) == ".ext4" {
				rootfsPath := filepath.Join(subDir, se.Name())
				fi, _ := os.Stat(rootfsPath)
				tag := e.Name()
				var size int64
				var modTime time.Time
				if fi != nil {
					size = fi.Size()
					modTime = fi.ModTime()
				}
				images = append(images, ImageInfo{
					Tag:        tag,
					RootfsPath: rootfsPath,
					Meta: ImageMeta{
						Tag:       tag,
						Digest:    strings.TrimSuffix(se.Name(), ".ext4"),
						Size:      size,
						CreatedAt: modTime,
						Source:    "registry",
					},
				})
			}
		}
	}

	return images, nil
}
