package image

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/jingkaihe/matchlock/internal/errx"
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
	if opts == nil {
		opts = &BuildOptions{}
	}
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
	// RootfsPath is retained as a convenience alias to the first lower layer path.
	RootfsPath string
	// LowerPaths are ordered from OCI base layer -> top layer.
	LowerPaths   []string
	LowerFSTypes []string
	Layers       []LayerRef
	// CanonicalLayers are the per-OCI-layer blobs used for dedupe and GC.
	CanonicalLayers []LayerRef

	Digest       string
	LayerDigests []string
	Size         int64
	Cached       bool
	OCI          *OCIConfig
}

type fileMeta struct {
	uid  int
	gid  int
	mode os.FileMode
}

type layerSpecialKind uint8

const (
	layerSpecialWhiteout layerSpecialKind = iota + 1
	layerSpecialOpaque
)

type layerSpecial struct {
	kind layerSpecialKind
	uid  int
	gid  int
	mode os.FileMode
}

func (b *Builder) Build(ctx context.Context, imageRef string) (*BuildResult, error) {
	if !b.forcePull {
		if result, err := b.store.Get(imageRef); err == nil {
			return result, nil
		}
		if result, err := GetRegistryCache(imageRef, b.cacheDir); err == nil {
			return result, nil
		}
	}

	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return nil, errx.Wrap(ErrParseReference, err)
	}

	remoteOpts := []remote.Option{
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithContext(ctx),
	}
	remoteOpts = append(remoteOpts, b.platformOptions()...)

	img, err := remote.Image(ref, remoteOpts...)
	if err != nil {
		return nil, errx.Wrap(ErrPullImage, err)
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
		Tag:               imageRef,
		Digest:            digest.String(),
		Size:              totalSize,
		CreatedAt:         time.Now().UTC(),
		Source:            "registry",
		OCI:               ociConfig,
		RuntimeRootfsPath: primaryRootfsPath(runtimeLayers),
	}
	if err := SaveRegistryCache(imageRef, b.cacheDir, canonicalLayers, meta); err != nil {
		return nil, errx.Wrap(ErrMetadata, err)
	}

	return newBuildResult(runtimeLayers, canonicalLayers, digest.String(), totalSize, false, ociConfig), nil
}

func (b *Builder) ingestImageLayers(img v1.Image) ([]LayerRef, []LayerRef, error) {
	ociLayers, err := img.Layers()
	if err != nil {
		return nil, nil, errx.With(ErrExtract, ": enumerate image layers: %w", err)
	}
	if len(ociLayers) == 0 {
		return nil, nil, errx.With(ErrExtract, ": no layers")
	}

	canonicalLayers, err := b.ingestPerLayerBlobs(ociLayers)
	if err != nil {
		return nil, nil, err
	}
	runtimeLayers, err := b.ingestFullySquashedLayers(ociLayers)
	if err != nil {
		return nil, nil, err
	}
	return runtimeLayers, canonicalLayers, nil
}

func (b *Builder) ingestFullySquashedLayers(ociLayers []v1.Layer) ([]LayerRef, error) {
	squashed, err := b.ensureSquashedPrefixLayerBlob(ociLayers)
	if err != nil {
		return nil, err
	}
	return []LayerRef{squashed}, nil
}

func (b *Builder) ingestPerLayerBlobs(ociLayers []v1.Layer) ([]LayerRef, error) {
	layers := make([]LayerRef, 0, len(ociLayers))
	seen := make(map[string]LayerRef, len(ociLayers))
	for i, layer := range ociLayers {
		diffID, err := layer.DiffID()
		if err != nil {
			return nil, errx.With(ErrImageDigest, ": layer %d diffid: %w", i, err)
		}
		digest := diffID.String()
		if cached, ok := seen[digest]; ok {
			layers = append(layers, cached)
			continue
		}
		ref, err := b.ensureLayerBlob(layer, digest)
		if err != nil {
			return nil, errx.With(ErrCreateExt4, ": layer %d (%s): %w", i, digest, err)
		}
		seen[digest] = ref
		layers = append(layers, ref)
	}
	return layers, nil
}

func (b *Builder) ensureLayerBlob(layer v1.Layer, digest string) (LayerRef, error) {
	blobPath := blobPathForLayer(b.cacheDir, digest, layerFSTypeEROFS)
	if fi, err := os.Stat(blobPath); err == nil && fi.Size() > 0 {
		return LayerRef{Digest: digest, FSType: layerFSTypeEROFS, Path: blobPath, Size: fileStoredBytes(fi)}, nil
	}
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		return LayerRef{}, errx.Wrap(ErrCreateDir, err)
	}

	tarPath, err := b.exportLayerAsTar(layer)
	if err != nil {
		return LayerRef{}, err
	}
	defer os.Remove(tarPath)

	if err := b.createEROFSFromTar(tarPath, blobPath); err != nil {
		return LayerRef{}, err
	}
	fi, err := os.Stat(blobPath)
	if err != nil {
		return LayerRef{}, errx.With(ErrCreateExt4, ": stat layer blob %s: %w", blobPath, err)
	}
	return LayerRef{Digest: digest, FSType: layerFSTypeEROFS, Path: blobPath, Size: fileStoredBytes(fi)}, nil
}

func (b *Builder) ensureSquashedPrefixLayerBlob(layers []v1.Layer) (LayerRef, error) {
	digest, err := squashLayerDigest(layers)
	if err != nil {
		return LayerRef{}, errx.With(ErrImageDigest, ": squash digest: %w", err)
	}
	blobPath := blobPathForLayer(b.cacheDir, digest, layerFSTypeEROFS)
	if fi, err := os.Stat(blobPath); err == nil && fi.Size() > 0 {
		return LayerRef{Digest: digest, FSType: layerFSTypeEROFS, Path: blobPath, Size: fileStoredBytes(fi)}, nil
	}
	if err := os.MkdirAll(filepath.Dir(blobPath), 0755); err != nil {
		return LayerRef{}, errx.Wrap(ErrCreateDir, err)
	}

	img := empty.Image
	img, err = mutate.AppendLayers(img, layers...)
	if err != nil {
		return LayerRef{}, errx.With(ErrExtract, ": append layers for squash: %w", err)
	}

	tarPath, err := b.exportImageAsTar(img)
	if err != nil {
		return LayerRef{}, err
	}
	defer os.Remove(tarPath)

	if err := b.createEROFSFromTar(tarPath, blobPath); err != nil {
		return LayerRef{}, err
	}

	fi, err := os.Stat(blobPath)
	if err != nil {
		return LayerRef{}, errx.With(ErrCreateExt4, ": stat squashed blob %s: %w", blobPath, err)
	}
	return LayerRef{Digest: digest, FSType: layerFSTypeEROFS, Path: blobPath, Size: fileStoredBytes(fi)}, nil
}

func (b *Builder) exportImageAsTar(img v1.Image) (string, error) {
	reader := mutate.Extract(img)
	defer reader.Close()

	tmpTar, err := os.CreateTemp("", "matchlock-layer-squash-*.tar")
	if err != nil {
		return "", errx.Wrap(ErrCreateTemp, err)
	}

	if _, err := io.Copy(tmpTar, reader); err != nil {
		_ = tmpTar.Close()
		_ = os.Remove(tmpTar.Name())
		return "", errx.With(ErrExtract, ": write tar stream: %w", err)
	}
	if err := tmpTar.Close(); err != nil {
		_ = os.Remove(tmpTar.Name())
		return "", errx.With(ErrExtract, ": close tar stream: %w", err)
	}
	return tmpTar.Name(), nil
}

func (b *Builder) exportLayerAsTar(layer v1.Layer) (string, error) {
	reader, err := layer.Uncompressed()
	if err != nil {
		return "", errx.With(ErrExtract, ": layer uncompressed stream: %w", err)
	}
	defer reader.Close()

	tmpTar, err := os.CreateTemp("", "matchlock-layer-*.tar")
	if err != nil {
		return "", errx.Wrap(ErrCreateTemp, err)
	}
	if _, err := io.Copy(tmpTar, reader); err != nil {
		_ = tmpTar.Close()
		_ = os.Remove(tmpTar.Name())
		return "", errx.With(ErrExtract, ": write layer tar: %w", err)
	}
	if err := tmpTar.Close(); err != nil {
		_ = os.Remove(tmpTar.Name())
		return "", errx.With(ErrExtract, ": close layer tar: %w", err)
	}
	return tmpTar.Name(), nil
}

func newBuildResult(runtimeLayers, canonicalLayers []LayerRef, digest string, size int64, cached bool, oci *OCIConfig) *BuildResult {
	if len(canonicalLayers) == 0 {
		canonicalLayers = runtimeLayers
	}
	lowerPaths := layerPaths(runtimeLayers)
	if size <= 0 {
		size = imageSizeFromLayers(runtimeLayers)
	}
	return &BuildResult{
		RootfsPath:      primaryLowerPath(lowerPaths),
		LowerPaths:      lowerPaths,
		LowerFSTypes:    layerFSTypes(runtimeLayers),
		Layers:          runtimeLayers,
		CanonicalLayers: canonicalLayers,
		Digest:          digest,
		LayerDigests:    layerDigests(runtimeLayers),
		Size:            size,
		Cached:          cached,
		OCI:             oci,
	}
}

func (b *Builder) SaveTag(tag string, result *BuildResult) error {
	layers, err := b.layersFromResult(result)
	if err != nil {
		return err
	}
	meta := ImageMeta{
		Digest:            result.Digest,
		Size:              result.Size,
		Source:            "tag",
		OCI:               result.OCI,
		RuntimeRootfsPath: result.RootfsPath,
	}
	return b.store.Save(tag, layers, meta)
}

func (b *Builder) Store() *Store {
	return b.store
}

func (b *Builder) layersFromResult(result *BuildResult) ([]LayerRef, error) {
	if result == nil {
		return nil, errx.With(ErrStoreSave, ": nil build result")
	}
	if len(result.CanonicalLayers) > 0 {
		return normalizeLayerRefs(result.CanonicalLayers, b.cacheDir)
	}
	if len(result.Layers) > 0 {
		return normalizeLayerRefs(result.Layers, b.cacheDir)
	}
	if len(result.LayerDigests) > 0 {
		layers := make([]LayerRef, 0, len(result.LayerDigests))
		for _, digest := range result.LayerDigests {
			fsType := layerFSTypeEROFS
			layers = append(layers, LayerRef{
				Digest: digest,
				FSType: fsType,
				Path:   blobPathForLayer(b.cacheDir, digest, fsType),
			})
		}
		return normalizeLayerRefs(layers, b.cacheDir)
	}
	if len(result.LowerPaths) > 0 {
		layers := make([]LayerRef, 0, len(result.LowerPaths))
		for _, p := range result.LowerPaths {
			digest, fsType, ok := digestAndFSTypeFromBlobPath(p)
			if !ok {
				return nil, errx.With(ErrStoreSave, ": cannot infer layer digest from path %q", p)
			}
			layers = append(layers, LayerRef{Digest: digest, FSType: fsType, Path: p})
		}
		return normalizeLayerRefs(layers, b.cacheDir)
	}
	return nil, errx.With(ErrStoreSave, ": no layers in build result")
}

func squashLayerDigest(layers []v1.Layer) (string, error) {
	h := sha256.New()
	_, _ = h.Write([]byte("matchlock-squash-v1\n"))
	for i, layer := range layers {
		diffID, err := layer.DiffID()
		if err != nil {
			return "", errx.With(ErrImageDigest, ": layer %d diffid: %w", i, err)
		}
		_, _ = h.Write([]byte(diffID.String()))
		_, _ = h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func squashPrefixCount(totalLayers, threshold int) int {
	if threshold <= 1 {
		return totalLayers
	}
	if totalLayers <= threshold {
		return 0
	}
	return totalLayers - (threshold - 1)
}

// ensureRealDir ensures that every component of path under root is a real
// directory, not a symlink. If an intermediate component is a symlink it is
// replaced with a real directory so that later file creation won't chase
// symlink chains (which can cause ELOOP on images with deep/circular symlinks).
func ensureRealDir(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for _, p := range parts {
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			if err := os.Mkdir(cur, 0755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(cur); err != nil {
				return err
			}
			if err := os.Mkdir(cur, 0755); err != nil {
				return err
			}
		} else if !fi.IsDir() {
			if err := os.Remove(cur); err != nil {
				return err
			}
			if err := os.Mkdir(cur, 0755); err != nil {
				return err
			}
		}
	}
	return nil
}

// safeCreate creates a file at target ensuring no intermediate symlinks are
// followed. It uses Lstat + O_NOFOLLOW-style semantics by removing any
// existing symlink at the final component before creating the file.
func safeCreate(root, target string, mode os.FileMode) (*os.File, error) {
	if err := ensureRealDir(root, filepath.Dir(target)); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(target)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		os.Remove(target)
	}
	return os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
}

func (b *Builder) extractImage(img v1.Image, destDir string) (map[string]fileMeta, error) {
	reader := mutate.Extract(img)
	defer reader.Close()

	meta := make(map[string]fileMeta)
	tr := tar.NewReader(reader)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errx.With(ErrExtract, ": read tar: %w", err)
		}

		clean, ok := normalizeTarPath(hdr.Name)
		if !ok {
			continue
		}
		target := filepath.Join(destDir, filepath.FromSlash(clean))

		skipMeta, err := applyTarHeader(destDir, target, clean, hdr, tr)
		if err != nil {
			return nil, err
		}
		if skipMeta {
			continue
		}

		relPath := "/" + clean
		meta[relPath] = fileMeta{
			uid:  hdr.Uid,
			gid:  hdr.Gid,
			mode: os.FileMode(hdr.Mode) & 0o7777,
		}
	}

	return meta, nil
}

func (b *Builder) extractLayer(layer v1.Layer, destDir string) (map[string]fileMeta, map[string]layerSpecial, error) {
	r, err := layer.Uncompressed()
	if err != nil {
		return nil, nil, errx.With(ErrExtract, ": layer uncompressed stream: %w", err)
	}
	defer r.Close()

	meta := make(map[string]fileMeta)
	specials := make(map[string]layerSpecial)
	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, errx.With(ErrExtract, ": read layer tar: %w", err)
		}

		clean, ok := normalizeTarPath(hdr.Name)
		if !ok {
			continue
		}

		base := path.Base(clean)
		dir := path.Dir(clean)
		if dir == "." {
			dir = ""
		}

		if base == ".wh..wh..opq" {
			if dir != "" {
				if err := ensureRealDir(destDir, filepath.Join(destDir, filepath.FromSlash(dir))); err != nil {
					return nil, nil, errx.With(ErrExtract, ": mkdir opaque parent %s: %w", dir, err)
				}
			}
			ext4Path := "/"
			if dir != "" {
				ext4Path = "/" + dir
			}
			specials[ext4Path] = layerSpecial{
				kind: layerSpecialOpaque,
				uid:  hdr.Uid,
				gid:  hdr.Gid,
				mode: os.FileMode(hdr.Mode) & 0o7777,
			}
			continue
		}

		if strings.HasPrefix(base, ".wh.") {
			name := strings.TrimPrefix(base, ".wh.")
			if name == "" {
				continue
			}
			if dir != "" {
				if err := ensureRealDir(destDir, filepath.Join(destDir, filepath.FromSlash(dir))); err != nil {
					return nil, nil, errx.With(ErrExtract, ": mkdir whiteout parent %s: %w", dir, err)
				}
			}
			whRel := name
			if dir != "" {
				whRel = path.Join(dir, name)
			}
			ext4Path := "/" + whRel
			specials[ext4Path] = layerSpecial{
				kind: layerSpecialWhiteout,
				uid:  hdr.Uid,
				gid:  hdr.Gid,
			}
			continue
		}

		target := filepath.Join(destDir, filepath.FromSlash(clean))
		skipMeta, err := applyTarHeader(destDir, target, clean, hdr, tr)
		if err != nil {
			return nil, nil, err
		}
		if skipMeta {
			continue
		}

		relPath := "/" + clean
		meta[relPath] = fileMeta{
			uid:  hdr.Uid,
			gid:  hdr.Gid,
			mode: os.FileMode(hdr.Mode) & 0o7777,
		}
	}

	return meta, specials, nil
}

func normalizeTarPath(name string) (string, bool) {
	clean := path.Clean(strings.TrimPrefix(name, "/"))
	if clean == "." || clean == "" {
		return "", false
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// applyTarHeader applies a single tar header entry to destDir/target.
// It returns skipMetadata=true for hardlinks, which share target inode metadata.
func applyTarHeader(destDir, target, clean string, hdr *tar.Header, tr *tar.Reader) (skipMetadata bool, err error) {
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := ensureRealDir(destDir, target); err != nil {
			return false, errx.With(ErrExtract, ": mkdir %s: %w", clean, err)
		}
	case tar.TypeReg:
		f, err := safeCreate(destDir, target, os.FileMode(hdr.Mode)&0o777)
		if err != nil {
			return false, errx.With(ErrExtract, ": create %s: %w", clean, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return false, errx.With(ErrExtract, ": write %s: %w", clean, err)
		}
		if err := f.Close(); err != nil {
			return false, errx.With(ErrExtract, ": close %s: %w", clean, err)
		}
	case tar.TypeSymlink:
		if err := ensureRealDir(destDir, filepath.Dir(target)); err != nil {
			return false, errx.With(ErrExtract, ": mkdir parent %s: %w", clean, err)
		}
		if err := os.RemoveAll(target); err != nil {
			return false, errx.With(ErrExtract, ": remove existing %s: %w", clean, err)
		}
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return false, errx.With(ErrExtract, ": symlink %s: %w", clean, err)
		}
	case tar.TypeLink:
		linkClean, ok := normalizeTarPath(hdr.Linkname)
		if !ok {
			return true, nil
		}
		linkTarget := filepath.Join(destDir, filepath.FromSlash(linkClean))
		if err := ensureRealDir(destDir, filepath.Dir(target)); err != nil {
			return false, errx.With(ErrExtract, ": mkdir parent %s: %w", clean, err)
		}
		if err := os.RemoveAll(target); err != nil {
			return false, errx.With(ErrExtract, ": remove existing %s: %w", clean, err)
		}
		if err := os.Link(linkTarget, target); err != nil {
			return false, errx.With(ErrExtract, ": hardlink %s: %w", clean, err)
		}
		return true, nil
	default:
		return true, nil
	}

	return false, nil
}

func extractOCIConfig(img v1.Image) *OCIConfig {
	cf, err := img.ConfigFile()
	if err != nil || cf == nil {
		return nil
	}
	c := cf.Config

	oci := &OCIConfig{
		User:       c.User,
		WorkingDir: c.WorkingDir,
		Entrypoint: c.Entrypoint,
		Cmd:        c.Cmd,
	}

	if len(c.Env) > 0 {
		oci.Env = make(map[string]string, len(c.Env))
		for _, e := range c.Env {
			if k, v, ok := strings.Cut(e, "="); ok {
				oci.Env[k] = v
			}
		}
	}

	return oci
}

// lstatWalk walks a directory tree using Lstat (not following symlinks) and
// calls fn for every entry. Errors are silently ignored.
func lstatWalk(root string, fn func(path string, info os.FileInfo)) {
	_ = lstatWalkErr(root, func(path string, info os.FileInfo) error {
		fn(path, info)
		return nil
	})
}

// lstatWalkErr walks a directory tree using Lstat so that symlinks are not
// followed. This avoids ELOOP when the tree contains circular symlinks.
func lstatWalkErr(root string, fn func(path string, info os.FileInfo) error) error {
	return lstatWalkDir(root, fn)
}

func lstatWalkDir(dir string, fn func(string, os.FileInfo) error) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := fn(dir, info); err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := filepath.Join(dir, e.Name())
		ci, err := os.Lstat(child)
		if err != nil {
			return err
		}
		if ci.IsDir() {
			if err := lstatWalkDir(child, fn); err != nil {
				return err
			}
		} else {
			if err := fn(child, ci); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasDebugfsUnsafeChars returns true if the path contains characters that
// would break debugfs command parsing (newlines, null bytes, or unbalanced quotes).
func hasDebugfsUnsafeChars(path string) bool {
	return strings.ContainsAny(path, "\n\r\x00")
}

func sanitizeRef(ref string) string {
	ref = strings.ReplaceAll(ref, "/", "_")
	ref = strings.ReplaceAll(ref, ":", "_")
	return ref
}
