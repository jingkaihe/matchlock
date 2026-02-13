package lifecycle

import (
	"database/sql"
	"path/filepath"

	"github.com/jingkaihe/matchlock/pkg/storedb"
)

const lifecycleModule = "lifecycle"

func lifecycleDBPath(vmDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(vmDir)), "state.db")
}

func openLifecycleDB(vmDir string) (*sql.DB, error) {
	return storedb.Open(storedb.OpenOptions{
		Path:       lifecycleDBPath(vmDir),
		Module:     lifecycleModule,
		Migrations: lifecycleMigrations(),
	})
}

func lifecycleMigrations() []storedb.Migration {
	return []storedb.Migration{
		{
			Version: 1,
			Name:    "create_append_only_vm_lifecycle_table",
			SQL: `
CREATE TABLE IF NOT EXISTS vm_lifecycle (
  vm_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  backend TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_error TEXT NOT NULL DEFAULT '',
  resources_json TEXT NOT NULL DEFAULT '{}',
  cleanup_json TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (vm_id, version)
);
CREATE INDEX IF NOT EXISTS idx_vm_lifecycle_phase ON vm_lifecycle(phase);
CREATE INDEX IF NOT EXISTS idx_vm_lifecycle_vm_version ON vm_lifecycle(vm_id, version DESC);
`,
		},
		{
			Version: 2,
			Name:    "create_vm_lifecycle_head",
			SQL: `
CREATE TABLE IF NOT EXISTS vm_lifecycle_head (
  vm_id TEXT PRIMARY KEY,
  last_version INTEGER NOT NULL
);
`,
		},
	}
}
