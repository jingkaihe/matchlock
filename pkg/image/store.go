package image

import (
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

const (
	imageScopeLocal    = "local"
	imageScopeRegistry = "registry"
	blobsDirName       = "blobs"
	layerFSTypeEROFS   = "erofs"
)

type OCIConfig struct {
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

type ImageMeta struct {
	Tag       string     `json:"tag"`
	Digest    string     `json:"digest,omitempty"`
	Size      int64      `json:"size"`
	CreatedAt time.Time  `json:"created_at"`
	Source    string     `json:"source,omitempty"`
	OCI       *OCIConfig `json:"oci,omitempty"`
	// RuntimeRootfsPath optionally pins the preferred runtime lower blob (hot squash cache).
	RuntimeRootfsPath string `json:"-"`
}

type LayerRef struct {
	Digest string
	FSType string
	Size   int64
	Path   string
}

type ImageInfo struct {
	Tag        string
	RootfsPath string
	Meta       ImageMeta
}

type Store struct {
	baseDir   string
	cacheRoot string
	blobsDir  string
	db        *sql.DB
	initErr   error
}

func NewStore(baseDir string) *Store {
	if baseDir == "" {
		baseDir = filepath.Join(defaultImageCacheDir(), "local")
	}

	cacheRoot := filepath.Dir(baseDir)
	blobsDir := filepath.Join(cacheRoot, blobsDirName)

	_ = os.MkdirAll(baseDir, 0755)
	_ = os.MkdirAll(blobsDir, 0755)

	db, err := openImageDBForLocalBase(baseDir)
	return &Store{
		baseDir:   baseDir,
		cacheRoot: cacheRoot,
		blobsDir:  blobsDir,
		db:        db,
		initErr:   err,
	}
}

func (s *Store) CacheRoot() string {
	return s.cacheRoot
}

func (s *Store) BlobPath(digest string) string {
	return blobPathForLayer(s.cacheRoot, digest, layerFSTypeEROFS)
}

func (s *Store) BlobPathWithFS(digest, fsType string) string {
	return blobPathForLayer(s.cacheRoot, digest, fsType)
}

func (s *Store) ready() error {
	if s.initErr != nil {
		return errx.Wrap(ErrStoreRead, s.initErr)
	}
	if s.db == nil {
		return ErrStoreRead
	}
	return nil
}

func (s *Store) Save(tag string, layers []LayerRef, meta ImageMeta) error {
	if err := s.ready(); err != nil {
		return err
	}
	normLayers, err := normalizeLayerRefs(layers, s.cacheRoot)
	if err != nil {
		return err
	}
	normLayers, err = ensureLayersInCache(normLayers, s.cacheRoot)
	if err != nil {
		return err
	}
	if len(normLayers) == 0 {
		return errx.With(ErrStoreSave, ": no layers for %q", tag)
	}
	if err := ensureLayerFilesExist(normLayers); err != nil {
		return err
	}

	meta.Tag = tag
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	if meta.Size <= 0 {
		meta.Size = imageSizeFromLayers(normLayers)
	}
	rootfsPath := meta.RuntimeRootfsPath
	if err := upsertImageMetaWithLayers(s.db, imageScopeLocal, tag, meta, normLayers, rootfsPath); err != nil {
		return err
	}
	if _, err := pruneUnreferencedBlobs(s.db, s.cacheRoot); err != nil {
		return err
	}
	return nil
}

func (s *Store) Get(tag string) (*BuildResult, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	return getImageBuildResult(s.db, imageScopeLocal, tag, s.cacheRoot)
}

func (s *Store) List() ([]ImageInfo, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	return listImageMeta(s.db, imageScopeLocal)
}

func (s *Store) Remove(tag string) error {
	if err := s.ready(); err != nil {
		return err
	}
	deleted, err := deleteImageWithLayers(s.db, imageScopeLocal, tag)
	if err != nil {
		return err
	}
	if !deleted {
		return errx.With(ErrImageNotFound, ": %q", tag)
	}
	if _, err := pruneUnreferencedBlobs(s.db, s.cacheRoot); err != nil {
		return err
	}

	// Cleanup legacy per-tag local directory if present.
	dir := filepath.Join(s.baseDir, sanitizeRef(tag))
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return errx.With(ErrStoreSave, ": remove legacy local rootfs dir: %w", err)
	}
	return nil
}

func (s *Store) GC() (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	return pruneUnreferencedBlobs(s.db, s.cacheRoot)
}

// SaveRegistryCache records metadata for a registry-cached image.
func SaveRegistryCache(tag string, cacheDir string, layers []LayerRef, meta ImageMeta) error {
	if cacheDir == "" {
		cacheDir = defaultImageCacheDir()
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return errx.Wrap(ErrCreateDir, err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, blobsDirName), 0755); err != nil {
		return errx.Wrap(ErrCreateDir, err)
	}

	normLayers, err := normalizeLayerRefs(layers, cacheDir)
	if err != nil {
		return err
	}
	normLayers, err = ensureLayersInCache(normLayers, cacheDir)
	if err != nil {
		return err
	}
	if len(normLayers) == 0 {
		return errx.With(ErrStoreSave, ": no layers for %q", tag)
	}
	if err := ensureLayerFilesExist(normLayers); err != nil {
		return err
	}

	db, err := openImageDBForCacheDir(cacheDir)
	if err != nil {
		return errx.With(ErrStoreSave, ": open registry metadata DB: %w", err)
	}
	defer db.Close()

	meta.Tag = tag
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	if meta.Size <= 0 {
		meta.Size = imageSizeFromLayers(normLayers)
	}

	rootfsPath := meta.RuntimeRootfsPath
	if err := upsertImageMetaWithLayers(db, imageScopeRegistry, tag, meta, normLayers, rootfsPath); err != nil {
		return err
	}
	if _, err := pruneUnreferencedBlobs(db, cacheDir); err != nil {
		return err
	}
	return nil
}

// GetRegistryCache returns a registry-cached image metadata entry as a BuildResult.
func GetRegistryCache(tag string, cacheDir string) (*BuildResult, error) {
	if cacheDir == "" {
		cacheDir = defaultImageCacheDir()
	}
	db, err := openImageDBForCacheDir(cacheDir)
	if err != nil {
		return nil, errx.With(ErrStoreRead, ": open registry metadata DB: %w", err)
	}
	defer db.Close()

	return getImageBuildResult(db, imageScopeRegistry, tag, cacheDir)
}

// RemoveRegistryCache removes a registry-cached image by tag.
func RemoveRegistryCache(tag string, cacheDir string) error {
	if cacheDir == "" {
		cacheDir = defaultImageCacheDir()
	}
	db, err := openImageDBForCacheDir(cacheDir)
	if err != nil {
		return errx.With(ErrStoreSave, ": open registry metadata DB: %w", err)
	}
	defer db.Close()

	deleted, err := deleteImageWithLayers(db, imageScopeRegistry, tag)
	if err != nil {
		return err
	}
	if !deleted {
		return errx.With(ErrImageNotFound, ": %q", tag)
	}
	if _, err := pruneUnreferencedBlobs(db, cacheDir); err != nil {
		return err
	}

	// Cleanup legacy per-tag registry directory if present.
	dir := filepath.Join(cacheDir, sanitizeRef(tag))
	if dir != filepath.Clean(cacheDir) && dir != filepath.Join(cacheDir, "local") {
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			return errx.With(ErrStoreSave, ": remove legacy registry cache dir: %w", err)
		}
	}
	return nil
}

func GC(cacheDir string) (int, error) {
	if cacheDir == "" {
		cacheDir = defaultImageCacheDir()
	}
	db, err := openImageDBForCacheDir(cacheDir)
	if err != nil {
		return 0, errx.With(ErrStoreRead, ": open registry metadata DB: %w", err)
	}
	defer db.Close()
	return pruneUnreferencedBlobs(db, cacheDir)
}

// ListRegistryCache lists images cached from registry pulls (non-local store).
func ListRegistryCache(cacheDir string) ([]ImageInfo, error) {
	if cacheDir == "" {
		cacheDir = defaultImageCacheDir()
	}
	db, err := openImageDBForCacheDir(cacheDir)
	if err != nil {
		return nil, errx.With(ErrStoreRead, ": open registry metadata DB: %w", err)
	}
	defer db.Close()

	return listImageMeta(db, imageScopeRegistry)
}

func upsertImageMetaWithLayers(db *sql.DB, scope, tag string, meta ImageMeta, layers []LayerRef, rootfsPath string) error {
	var ociJSON []byte
	if meta.OCI != nil {
		data, err := json.Marshal(meta.OCI)
		if err != nil {
			return errx.With(ErrMetadata, ": marshal OCI config: %w", err)
		}
		ociJSON = data
	}
	createdAt := meta.CreatedAt.UTC().Format(time.RFC3339Nano)
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := db.Begin()
	if err != nil {
		return errx.With(ErrStoreSave, ": begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO images(scope, tag, digest, size, created_at, source, rootfs_path, oci_json, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(scope, tag) DO UPDATE SET
		   digest = excluded.digest,
		   size = excluded.size,
		   created_at = excluded.created_at,
		   source = excluded.source,
		   rootfs_path = excluded.rootfs_path,
		   oci_json = excluded.oci_json,
		   updated_at = excluded.updated_at`,
		scope,
		tag,
		meta.Digest,
		meta.Size,
		createdAt,
		meta.Source,
		rootfsPath,
		ociJSON,
		updatedAt,
	)
	if err != nil {
		return errx.With(ErrStoreSave, ": upsert image metadata: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM image_layers WHERE scope = ? AND tag = ?`, scope, tag); err != nil {
		return errx.With(ErrStoreSave, ": clear image layers: %w", err)
	}

	stmt, err := tx.Prepare(`INSERT INTO image_layers(scope, tag, ordinal, digest, fs_type, size) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return errx.With(ErrStoreSave, ": prepare layer insert: %w", err)
	}
	defer stmt.Close()

	for ordinal, layer := range layers {
		if _, err := stmt.Exec(scope, tag, ordinal, layer.Digest, layer.FSType, layer.Size); err != nil {
			return errx.With(ErrStoreSave, ": insert layer %d: %w", ordinal, err)
		}
	}
	if err := syncLayerRefCounts(tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return errx.With(ErrStoreSave, ": commit image metadata: %w", err)
	}
	return nil
}

func getImageBuildResult(db *sql.DB, scope, tag, cacheRoot string) (*BuildResult, error) {
	info, err := getImageMeta(db, scope, tag)
	if err != nil {
		return nil, err
	}
	layers, err := getImageLayers(db, scope, tag, cacheRoot)
	if err != nil {
		return nil, err
	}
	if len(layers) == 0 {
		return nil, errx.With(ErrImageNotFound, ": layers for %q", tag)
	}
	if err := ensureLayerFilesExist(layers); err != nil {
		return nil, err
	}

	size := info.Meta.Size
	if size <= 0 {
		size = imageSizeFromLayers(layers)
	}
	runtimeLayers := layers
	if runtimeLayer, ok := runtimeLayerFromRootfsPath(info.RootfsPath); ok &&
		!containsLayerDigest(layers, runtimeLayer.Digest, runtimeLayer.FSType) {
		runtimeLayers = []LayerRef{runtimeLayer}
	}
	lowerPaths := layerPaths(runtimeLayers)
	return &BuildResult{
		RootfsPath:      primaryLowerPath(lowerPaths),
		LowerPaths:      lowerPaths,
		LowerFSTypes:    layerFSTypes(runtimeLayers),
		Layers:          runtimeLayers,
		CanonicalLayers: layers,
		Digest:          info.Meta.Digest,
		Size:            size,
		Cached:          true,
		OCI:             info.Meta.OCI,
		LayerDigests:    layerDigests(runtimeLayers),
	}, nil
}

func runtimeLayerFromRootfsPath(rootfsPath string) (LayerRef, bool) {
	if rootfsPath == "" {
		return LayerRef{}, false
	}
	digest, fsType, ok := digestAndFSTypeFromBlobPath(rootfsPath)
	if !ok {
		return LayerRef{}, false
	}
	fi, err := os.Stat(rootfsPath)
	if err != nil || fi.Size() <= 0 {
		return LayerRef{}, false
	}
	return LayerRef{
		Digest: digest,
		FSType: fsType,
		Size:   fileStoredBytes(fi),
		Path:   rootfsPath,
	}, true
}

func containsLayerDigest(layers []LayerRef, digest, fsType string) bool {
	for _, layer := range layers {
		if layer.Digest == digest && layer.FSType == fsType {
			return true
		}
	}
	return false
}

func getImageLayers(db *sql.DB, scope, tag, cacheRoot string) ([]LayerRef, error) {
	rows, err := db.Query(
		`SELECT digest, fs_type, size
		   FROM image_layers
		  WHERE scope = ? AND tag = ?
		  ORDER BY ordinal ASC`,
		scope,
		tag,
	)
	if err != nil {
		return nil, errx.With(ErrStoreRead, ": query image layers: %w", err)
	}
	defer rows.Close()

	var layers []LayerRef
	for rows.Next() {
		var layer LayerRef
		if err := rows.Scan(&layer.Digest, &layer.FSType, &layer.Size); err != nil {
			return nil, errx.With(ErrStoreRead, ": scan image layer: %w", err)
		}
		fsType, err := normalizeLayerFSType(layer.FSType)
		if err != nil {
			return nil, err
		}
		layer.FSType = fsType
		layer.Path = blobPathForLayer(cacheRoot, layer.Digest, layer.FSType)
		layers = append(layers, layer)
	}
	if err := rows.Err(); err != nil {
		return nil, errx.With(ErrStoreRead, ": iterate image layers: %w", err)
	}
	return layers, nil
}

func getImageMeta(db *sql.DB, scope, tag string) (*ImageInfo, error) {
	row := db.QueryRow(
		`SELECT tag, digest, size, created_at, source, rootfs_path, oci_json
		   FROM images
		  WHERE scope = ? AND tag = ?`,
		scope,
		tag,
	)

	var (
		info      ImageInfo
		createdAt string
		ociJSON   []byte
	)
	if err := row.Scan(&info.Tag, &info.Meta.Digest, &info.Meta.Size, &createdAt, &info.Meta.Source, &info.RootfsPath, &ociJSON); err != nil {
		if err == sql.ErrNoRows {
			return nil, errx.With(ErrImageNotFound, ": %q", tag)
		}
		return nil, errx.With(ErrStoreRead, ": get image metadata: %w", err)
	}
	if createdAt != "" {
		t, err := time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			t, err = time.Parse(time.RFC3339, createdAt)
			if err != nil {
				return nil, errx.With(ErrStoreRead, ": parse created_at: %w", err)
			}
		}
		info.Meta.CreatedAt = t
	}
	info.Meta.Tag = info.Tag
	if len(ociJSON) > 0 {
		var oci OCIConfig
		if err := json.Unmarshal(ociJSON, &oci); err != nil {
			return nil, errx.With(ErrStoreRead, ": decode OCI config: %w", err)
		}
		info.Meta.OCI = &oci
	}
	return &info, nil
}

func listImageMeta(db *sql.DB, scope string) ([]ImageInfo, error) {
	rows, err := db.Query(
		`SELECT tag, digest, size, created_at, source, rootfs_path, oci_json
		   FROM images
		  WHERE scope = ?
		  ORDER BY created_at DESC`,
		scope,
	)
	if err != nil {
		return nil, errx.With(ErrStoreRead, ": list image metadata: %w", err)
	}
	defer rows.Close()

	var images []ImageInfo
	for rows.Next() {
		var (
			info      ImageInfo
			createdAt string
			ociJSON   []byte
		)
		if err := rows.Scan(&info.Tag, &info.Meta.Digest, &info.Meta.Size, &createdAt, &info.Meta.Source, &info.RootfsPath, &ociJSON); err != nil {
			return nil, errx.With(ErrStoreRead, ": scan image metadata: %w", err)
		}
		if createdAt != "" {
			t, err := time.Parse(time.RFC3339Nano, createdAt)
			if err != nil {
				t, err = time.Parse(time.RFC3339, createdAt)
				if err != nil {
					return nil, errx.With(ErrStoreRead, ": parse created_at: %w", err)
				}
			}
			info.Meta.CreatedAt = t
		}
		info.Meta.Tag = info.Tag
		if len(ociJSON) > 0 {
			var oci OCIConfig
			if err := json.Unmarshal(ociJSON, &oci); err != nil {
				return nil, errx.With(ErrStoreRead, ": decode OCI config: %w", err)
			}
			info.Meta.OCI = &oci
		}
		images = append(images, info)
	}
	if err := rows.Err(); err != nil {
		return nil, errx.With(ErrStoreRead, ": iterate image metadata: %w", err)
	}
	return images, nil
}

func deleteImageWithLayers(db *sql.DB, scope, tag string) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, errx.With(ErrStoreSave, ": begin delete transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM images WHERE scope = ? AND tag = ?`, scope, tag)
	if err != nil {
		return false, errx.With(ErrStoreSave, ": remove image metadata: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, errx.With(ErrStoreSave, ": check rows affected: %w", err)
	}
	if rows == 0 {
		_ = tx.Rollback()
		return false, nil
	}

	if _, err := tx.Exec(`DELETE FROM image_layers WHERE scope = ? AND tag = ?`, scope, tag); err != nil {
		return false, errx.With(ErrStoreSave, ": remove image layers: %w", err)
	}
	if err := syncLayerRefCounts(tx); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, errx.With(ErrStoreSave, ": commit delete transaction: %w", err)
	}
	return true, nil
}

func normalizeLayerRefs(layers []LayerRef, cacheRoot string) ([]LayerRef, error) {
	norm := make([]LayerRef, 0, len(layers))
	for i, layer := range layers {
		if layer.Digest == "" {
			if d, fsType, ok := digestAndFSTypeFromBlobPath(layer.Path); ok {
				layer.Digest = d
				if layer.FSType == "" {
					layer.FSType = fsType
				}
			}
		}
		if layer.Digest == "" {
			return nil, errx.With(ErrStoreSave, ": missing digest for layer %d", i)
		}
		fsType, err := normalizeLayerFSType(layer.FSType)
		if err != nil {
			return nil, err
		}
		layer.FSType = fsType
		if layer.Path == "" {
			layer.Path = blobPathForLayer(cacheRoot, layer.Digest, layer.FSType)
		}
		if layer.Size <= 0 {
			if fi, err := os.Stat(layer.Path); err == nil {
				layer.Size = fileStoredBytes(fi)
			}
		}
		norm = append(norm, layer)
	}
	return norm, nil
}

func ensureLayersInCache(layers []LayerRef, cacheRoot string) ([]LayerRef, error) {
	out := make([]LayerRef, 0, len(layers))
	for _, layer := range layers {
		targetPath := blobPathForLayer(cacheRoot, layer.Digest, layer.FSType)
		if layer.Path != targetPath {
			if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
				return nil, errx.Wrap(ErrCreateDir, err)
			}
			if _, err := os.Stat(targetPath); os.IsNotExist(err) {
				if err := copyLayerBlob(layer.Path, targetPath); err != nil {
					return nil, err
				}
			}
			layer.Path = targetPath
		}
		if fi, err := os.Stat(layer.Path); err == nil {
			layer.Size = fileStoredBytes(fi)
		}
		out = append(out, layer)
	}
	return out, nil
}

func copyLayerBlob(srcPath, dstPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return errx.With(ErrStoreSave, ": open layer blob %s: %w", srcPath, err)
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return errx.With(ErrStoreSave, ": create layer blob %s: %w", dstPath, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(dstPath)
		return errx.With(ErrStoreSave, ": copy layer blob %s -> %s: %w", srcPath, dstPath, err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(dstPath)
		return errx.With(ErrStoreSave, ": flush layer blob %s: %w", dstPath, err)
	}
	return nil
}

func ensureLayerFilesExist(layers []LayerRef) error {
	for _, layer := range layers {
		fi, err := os.Stat(layer.Path)
		if err != nil || fi.Size() <= 0 {
			return errx.With(ErrImageNotFound, ": missing layer %q", layer.Digest)
		}
	}
	return nil
}

func imageSizeFromLayers(layers []LayerRef) int64 {
	var total int64
	for _, layer := range layers {
		if layer.Size > 0 {
			total += layer.Size
		}
	}
	return total
}

func layerPaths(layers []LayerRef) []string {
	paths := make([]string, 0, len(layers))
	for _, layer := range layers {
		paths = append(paths, layer.Path)
	}
	return paths
}

func layerDigests(layers []LayerRef) []string {
	digests := make([]string, 0, len(layers))
	for _, layer := range layers {
		digests = append(digests, layer.Digest)
	}
	return digests
}

func layerFSTypes(layers []LayerRef) []string {
	fsTypes := make([]string, 0, len(layers))
	for _, layer := range layers {
		fsType, err := normalizeLayerFSType(layer.FSType)
		if err != nil {
			fsType = layerFSTypeEROFS
		}
		fsTypes = append(fsTypes, fsType)
	}
	return fsTypes
}

func primaryRootfsPath(layers []LayerRef) string {
	if len(layers) == 0 {
		return ""
	}
	return layers[len(layers)-1].Path
}

func primaryLowerPath(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

func blobPathForDigest(cacheRoot, digest string) string {
	return blobPathForLayer(cacheRoot, digest, layerFSTypeEROFS)
}

func blobPathForLayer(cacheRoot, digest, fsType string) string {
	if cacheRoot == "" {
		cacheRoot = defaultImageCacheDir()
	}
	return filepath.Join(cacheRoot, blobsDirName, blobFileNameForLayer(digest, fsType))
}

func blobFileNameForLayer(digest, fsType string) string {
	normalized, err := normalizeLayerFSType(fsType)
	if err != nil {
		normalized = layerFSTypeEROFS
	}
	if digest == "" {
		return "unknown." + normalized
	}
	safe := strings.ReplaceAll(digest, ":", "_")
	safe = strings.ReplaceAll(safe, "/", "_")
	return safe + "." + normalized
}

func digestAndFSTypeFromBlobPath(path string) (string, string, bool) {
	name := filepath.Base(path)
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 {
		return "", "", false
	}
	stem := name[:i]
	fsType := name[i+1:]
	normalizedFS, err := normalizeLayerFSType(fsType)
	if err != nil {
		return "", "", false
	}
	idx := strings.IndexByte(stem, '_')
	if idx <= 0 || idx >= len(stem)-1 {
		return "", "", false
	}
	return stem[:idx] + ":" + stem[idx+1:], normalizedFS, true
}

func normalizeLayerFSType(fsType string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(fsType))
	if v == "" {
		return layerFSTypeEROFS, nil
	}
	switch v {
	case layerFSTypeEROFS:
		return v, nil
	default:
		return "", errx.With(ErrLayerFS, ": %q (supported: %s)", fsType, layerFSTypeEROFS)
	}
}

func syncLayerRefCounts(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM layer_refs`); err != nil {
		return errx.With(ErrStoreSave, ": clear layer refs: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO layer_refs(digest, fs_type, ref_count, size, updated_at)
		 SELECT digest, COALESCE(fs_type, 'erofs'), COUNT(*), MAX(size), ?
		   FROM image_layers
		  GROUP BY digest, COALESCE(fs_type, 'erofs')`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return errx.With(ErrStoreSave, ": rebuild layer refs: %w", err)
	}
	return nil
}

func pruneUnreferencedBlobs(db *sql.DB, cacheRoot string) (int, error) {
	rows, err := db.Query(`SELECT digest, fs_type FROM layer_refs WHERE ref_count > 0`)
	if err != nil {
		return 0, errx.With(ErrStoreRead, ": query referenced blobs: %w", err)
	}
	defer rows.Close()

	referenced := make(map[string]struct{})
	for rows.Next() {
		var (
			digest string
			fsType string
		)
		if err := rows.Scan(&digest, &fsType); err != nil {
			return 0, errx.With(ErrStoreRead, ": scan referenced blob: %w", err)
		}
		if digest == "" {
			continue
		}
		normalizedFS, err := normalizeLayerFSType(fsType)
		if err != nil {
			continue
		}
		referenced[digest+"|"+normalizedFS] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, errx.With(ErrStoreRead, ": iterate referenced blobs: %w", err)
	}

	rootRows, err := db.Query(`SELECT rootfs_path FROM images WHERE rootfs_path <> ''`)
	if err != nil {
		return 0, errx.With(ErrStoreRead, ": query runtime rootfs paths: %w", err)
	}
	defer rootRows.Close()
	for rootRows.Next() {
		var path string
		if err := rootRows.Scan(&path); err != nil {
			return 0, errx.With(ErrStoreRead, ": scan runtime rootfs path: %w", err)
		}
		digest, fsType, ok := digestAndFSTypeFromBlobPath(path)
		if !ok {
			continue
		}
		referenced[digest+"|"+fsType] = struct{}{}
	}
	if err := rootRows.Err(); err != nil {
		return 0, errx.With(ErrStoreRead, ": iterate runtime rootfs paths: %w", err)
	}

	blobsDir := filepath.Join(cacheRoot, blobsDirName)
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, errx.With(ErrStoreSave, ": read blobs directory: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(blobsDir, entry.Name())
		digest, fsType, ok := digestAndFSTypeFromBlobPath(path)
		if !ok {
			// Remove legacy layer blobs that are no longer supported.
			if strings.HasPrefix(entry.Name(), "sha256_") && filepath.Ext(entry.Name()) != "."+layerFSTypeEROFS {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return removed, errx.With(ErrStoreSave, ": remove legacy blob %q: %w", entry.Name(), err)
				}
				removed++
			}
			continue
		}
		if _, used := referenced[digest+"|"+fsType]; used {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return removed, errx.With(ErrStoreSave, ": remove unreferenced blob %q: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}
