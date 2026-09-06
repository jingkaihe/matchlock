package state

import (
	"database/sql"
	"path/filepath"

	"github.com/jingkaihe/matchlock/pkg/storedb"
)

const stateModule = "state"

func stateDBPath(baseDir string) string {
	rootDir := filepath.Dir(filepath.Clean(baseDir))
	return filepath.Join(rootDir, "state.db")
}

func openStateDB(baseDir string) (*sql.DB, error) {
	return storedb.Open(storedb.OpenOptions{
		Path:       stateDBPath(baseDir),
		Module:     stateModule,
		Migrations: stateMigrations(),
	})
}

func stateMigrations() []storedb.Migration {
	return []storedb.Migration{
		{
			Version: 1,
			Name:    "create_vms",
			SQL: `
CREATE TABLE IF NOT EXISTS vms (
  id TEXT PRIMARY KEY,
  pid INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL,
  image TEXT NOT NULL DEFAULT '',
  config_json BLOB,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_vms_status ON vms(status);
CREATE INDEX IF NOT EXISTS idx_vms_created_at ON vms(created_at);
`,
		},
		{
			Version: 2,
			Name:    "create_subnet_allocations",
			SQL: `
CREATE TABLE IF NOT EXISTS subnet_allocations (
  vm_id TEXT PRIMARY KEY,
  octet INTEGER NOT NULL UNIQUE,
  gateway_ip TEXT NOT NULL,
  guest_ip TEXT NOT NULL,
  subnet TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_subnet_allocations_octet ON subnet_allocations(octet);
`,
		},
		{
			// Per-VM IPv6 unique-local addressing (see ipv6ForOctet in
			// subnet.go). The columns are derivable from octet, so a legacy row
			// left with the '' default keeps working: Get() derives the values
			// instead of erroring. ALTER TABLE ... ADD COLUMN is not available
			// with an IF NOT EXISTS guard in SQLite, so the statements must only
			// ever run once per database — which is exactly what the versioned
			// migration ledger above guarantees (a second Open skips version 3).
			Version: 3,
			Name:    "add_subnet_allocations_ipv6",
			SQL: `
ALTER TABLE subnet_allocations ADD COLUMN gateway_ip6 TEXT NOT NULL DEFAULT '';
ALTER TABLE subnet_allocations ADD COLUMN guest_ip6 TEXT NOT NULL DEFAULT '';
ALTER TABLE subnet_allocations ADD COLUMN subnet6 TEXT NOT NULL DEFAULT '';
`,
		},
	}
}
