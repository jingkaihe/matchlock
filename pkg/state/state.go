package state

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
)

type VMState struct {
	ID        string          `json:"id"`
	PID       int             `json:"pid"`
	Status    string          `json:"status"`
	Image     string          `json:"image"`
	CreatedAt time.Time       `json:"created_at"`
	Config    json.RawMessage `json:"config,omitempty"`
}

type Manager struct {
	baseDir string
	db      *sql.DB
	initErr error
}

func NewManager() *Manager {
	home, _ := os.UserHomeDir()
	baseDir := filepath.Join(home, ".matchlock", "vms")
	return NewManagerWithDir(baseDir)
}

func NewManagerWithDir(baseDir string) *Manager {
	_ = os.MkdirAll(baseDir, 0755)

	db, err := openStateDB(baseDir)
	return &Manager{
		baseDir: baseDir,
		db:      db,
		initErr: err,
	}
}

func (m *Manager) ready() error {
	if m.initErr != nil {
		return errx.Wrap(ErrStateDBInit, m.initErr)
	}
	if m.db == nil {
		return ErrStateDBInit
	}
	return nil
}

func (m *Manager) Register(id string, config interface{}) error {
	if err := m.ready(); err != nil {
		return err
	}

	dir := filepath.Join(m.baseDir, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errx.Wrap(ErrRegisterVM, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0700); err != nil {
		return errx.Wrap(ErrRegisterVM, err)
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		return errx.Wrap(ErrRegisterVM, err)
	}

	var cfg struct {
		Image string `json:"image"`
	}
	_ = json.Unmarshal(configJSON, &cfg)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = m.db.Exec(
		`INSERT INTO vms (id, pid, status, image, config_json, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   pid = excluded.pid,
		   status = excluded.status,
		   image = excluded.image,
		   config_json = excluded.config_json,
		   updated_at = excluded.updated_at`,
		id,
		os.Getpid(),
		"running",
		cfg.Image,
		configJSON,
		now,
		now,
	)
	if err != nil {
		_ = os.RemoveAll(dir)
		return errx.Wrap(ErrRegisterVM, err)
	}
	return nil
}

func (m *Manager) Unregister(id string) error {
	if err := m.ready(); err != nil {
		return err
	}

	res, err := m.db.Exec(`UPDATE vms SET status = ?, pid = 0, updated_at = ? WHERE id = ?`, "stopped", time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return errx.Wrap(ErrUnregisterVM, err)
	}
	rows, err := res.RowsAffected()
	if err == nil && rows == 0 {
		return fmt.Errorf("VM %s not found", id)
	}
	return nil
}

func (m *Manager) List() ([]VMState, error) {
	if err := m.ready(); err != nil {
		return nil, err
	}

	rows, err := m.db.Query(`SELECT id, pid, status, image, config_json, created_at FROM vms ORDER BY created_at DESC`)
	if err != nil {
		return nil, errx.Wrap(ErrListVMs, err)
	}
	defer rows.Close()

	var states []VMState
	var crashedIDs []string
	for rows.Next() {
		state, err := scanVMStateRow(rows)
		if err != nil {
			return nil, errx.Wrap(ErrListVMs, err)
		}

		if state.Status == "running" && !m.isProcessRunning(state.PID) {
			state.Status = "crashed"
			crashedIDs = append(crashedIDs, state.ID)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, errx.Wrap(ErrListVMs, err)
	}

	// Avoid issuing writes while iterating rows because storedb uses a single
	// open connection, which can block when a SELECT cursor is still active.
	if err := rows.Close(); err != nil {
		return nil, errx.Wrap(ErrListVMs, err)
	}

	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for _, vmID := range crashedIDs {
		if _, err := m.db.Exec(`UPDATE vms SET status = ?, updated_at = ? WHERE id = ?`, "crashed", updatedAt, vmID); err != nil {
			return nil, errx.Wrap(ErrListVMs, err)
		}
	}

	return states, nil
}

func (m *Manager) Get(id string) (VMState, error) {
	if err := m.ready(); err != nil {
		return VMState{}, err
	}

	row := m.db.QueryRow(`SELECT id, pid, status, image, config_json, created_at FROM vms WHERE id = ?`, id)
	state, err := scanVMStateRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return VMState{}, fmt.Errorf("VM %s not found", id)
		}
		return VMState{}, errx.Wrap(ErrGetVM, err)
	}
	return state, nil
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanVMStateRow(scanner rowScanner) (VMState, error) {
	var state VMState
	var createdAtText string
	var configBytes []byte
	if err := scanner.Scan(&state.ID, &state.PID, &state.Status, &state.Image, &configBytes, &createdAtText); err != nil {
		return VMState{}, err
	}
	if len(configBytes) > 0 {
		state.Config = configBytes
		if state.Image == "" {
			var cfg struct {
				Image string `json:"image"`
			}
			if err := json.Unmarshal(configBytes, &cfg); err == nil {
				state.Image = cfg.Image
			}
		}
	}
	if createdAtText != "" {
		createdAt, err := time.Parse(time.RFC3339Nano, createdAtText)
		if err != nil {
			createdAt, err = time.Parse(time.RFC3339, createdAtText)
			if err != nil {
				return VMState{}, err
			}
		}
		state.CreatedAt = createdAt
	}
	return state, nil
}

func (m *Manager) Kill(id string) error {
	state, err := m.Get(id)
	if err != nil {
		return err
	}
	if state.PID == 0 {
		return fmt.Errorf("VM %s is not running", id)
	}

	process, err := os.FindProcess(state.PID)
	if err != nil {
		return errx.Wrap(ErrKillVM, err)
	}
	return process.Signal(syscall.SIGTERM)
}

func (m *Manager) Remove(id string) error {
	state, err := m.Get(id)
	if err != nil {
		return err
	}
	if state.Status == "running" {
		if m.isProcessRunning(state.PID) {
			return fmt.Errorf("cannot remove running VM %s, kill it first", id)
		}
		_, _ = m.db.Exec(`UPDATE vms SET status = ?, updated_at = ? WHERE id = ?`, "crashed", time.Now().UTC().Format(time.RFC3339Nano), id)
	}

	tx, err := m.db.Begin()
	if err != nil {
		return errx.Wrap(ErrRemoveVM, err)
	}
	if _, err := tx.Exec(`DELETE FROM subnet_allocations WHERE vm_id = ?`, id); err != nil {
		_ = tx.Rollback()
		return errx.Wrap(ErrRemoveVM, err)
	}
	if _, err := tx.Exec(`DELETE FROM vms WHERE id = ?`, id); err != nil {
		_ = tx.Rollback()
		return errx.Wrap(ErrRemoveVM, err)
	}
	if err := tx.Commit(); err != nil {
		return errx.Wrap(ErrRemoveVM, err)
	}

	if err := os.RemoveAll(filepath.Join(m.baseDir, id)); err != nil {
		return errx.Wrap(ErrRemoveVM, err)
	}
	return nil
}

func (m *Manager) Prune() ([]string, error) {
	states, err := m.List()
	if err != nil {
		return nil, err
	}

	var pruned []string
	for _, state := range states {
		if state.Status == "crashed" || state.Status == "stopped" {
			if err := m.Remove(state.ID); err == nil {
				pruned = append(pruned, state.ID)
			}
		}
	}
	_, _ = m.db.Exec(`DELETE FROM subnet_allocations WHERE vm_id NOT IN (SELECT id FROM vms)`)
	return pruned, nil
}

func (m *Manager) isProcessRunning(pid int) bool {
	if pid == 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func (m *Manager) LogPath(id string) string {
	return filepath.Join(m.baseDir, id, "logs", "vm.log")
}

func (m *Manager) SocketPath(id string) string {
	return filepath.Join(m.baseDir, id, "socket")
}

func (m *Manager) ExecSocketPath(id string) string {
	return filepath.Join(m.baseDir, id, "exec.sock")
}

func (m *Manager) Dir(id string) string {
	return filepath.Join(m.baseDir, id)
}

func (m *Manager) DBPath() string {
	return stateDBPath(m.baseDir)
}
