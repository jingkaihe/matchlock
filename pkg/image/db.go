package image

import (
	"database/sql"
	"os"
	"path/filepath"

	"github.com/jingkaihe/matchlock/pkg/storedb"
)

const imageModule = "image"

func defaultImageCacheDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "matchlock", "images")
}

func imageDBPathForLocalBase(localBaseDir string) string {
	return filepath.Join(filepath.Dir(localBaseDir), "metadata.db")
}

func imageDBPathForCacheDir(cacheDir string) string {
	return filepath.Join(cacheDir, "metadata.db")
}

func openImageDBByPath(path string) (*sql.DB, error) {
	return storedb.Open(storedb.OpenOptions{
		Path:       path,
		Module:     imageModule,
		Migrations: imageMigrations(),
	})
}

func openImageDBForLocalBase(localBaseDir string) (*sql.DB, error) {
	return openImageDBByPath(imageDBPathForLocalBase(localBaseDir))
}

func openImageDBForCacheDir(cacheDir string) (*sql.DB, error) {
	if cacheDir == "" {
		cacheDir = defaultImageCacheDir()
	}
	return openImageDBByPath(imageDBPathForCacheDir(cacheDir))
}

func imageMigrations() []storedb.Migration {
	return []storedb.Migration{
		{
			Version: 1,
			Name:    "create_images",
			SQL: `
CREATE TABLE IF NOT EXISTS images (
  scope TEXT NOT NULL,
  tag TEXT NOT NULL,
  digest TEXT,
  size INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  source TEXT,
  rootfs_path TEXT NOT NULL,
  oci_json TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (scope, tag)
);
CREATE INDEX IF NOT EXISTS idx_images_scope_created ON images(scope, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_images_digest ON images(digest);
`,
		},
		{
			Version: 2,
			Name:    "create_image_layers_erofs_schema",
			SQL: `
CREATE TABLE IF NOT EXISTS image_layers (
  scope TEXT NOT NULL,
  tag TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  digest TEXT NOT NULL,
  fs_type TEXT NOT NULL DEFAULT 'erofs',
  size INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (scope, tag, ordinal)
);
CREATE INDEX IF NOT EXISTS idx_image_layers_ref ON image_layers(scope, tag);
CREATE INDEX IF NOT EXISTS idx_image_layers_digest ON image_layers(digest);
CREATE INDEX IF NOT EXISTS idx_image_layers_digest_fs ON image_layers(digest, fs_type);

CREATE TABLE IF NOT EXISTS layer_refs (
  digest TEXT NOT NULL,
  fs_type TEXT NOT NULL DEFAULT 'erofs',
  ref_count INTEGER NOT NULL DEFAULT 0,
  size INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (digest, fs_type)
);
CREATE INDEX IF NOT EXISTS idx_layer_refs_count ON layer_refs(ref_count);

DELETE FROM image_layers
 WHERE LOWER(COALESCE(fs_type, '')) <> 'erofs';

DELETE FROM images
 WHERE NOT EXISTS (
   SELECT 1
     FROM image_layers
    WHERE image_layers.scope = images.scope
      AND image_layers.tag = images.tag
 );

DELETE FROM layer_refs;
INSERT OR REPLACE INTO layer_refs(digest, fs_type, ref_count, size, updated_at)
SELECT digest, 'erofs', COUNT(*), MAX(size), strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
  FROM image_layers
 GROUP BY digest;
`,
		},
	}
}
