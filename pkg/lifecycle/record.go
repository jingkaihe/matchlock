package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	sqlite "modernc.org/sqlite"
)

const (
	sqlitePrimaryErrMask     = 0xFF
	sqlitePrimaryBusy        = 5
	sqlitePrimaryLocked      = 6
	recordWriteRetryAttempts = 8
	recordWriteRetryBaseWait = 25 * time.Millisecond
)

type Phase string

const (
	PhaseCreating      Phase = "creating"
	PhaseCreated       Phase = "created"
	PhaseStarting      Phase = "starting"
	PhaseRunning       Phase = "running"
	PhaseStopping      Phase = "stopping"
	PhaseStopped       Phase = "stopped"
	PhaseCleaning      Phase = "cleaning"
	PhaseCleaned       Phase = "cleaned"
	PhaseCreateFailed  Phase = "create_failed"
	PhaseStartFailed   Phase = "start_failed"
	PhaseStopFailed    Phase = "stop_failed"
	PhaseCleanupFailed Phase = "cleanup_failed"
)

type CleanupResult struct {
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Resources struct {
	StateDir      string `json:"state_dir,omitempty"`
	Workspace     string `json:"workspace,omitempty"`
	RootfsPath    string `json:"rootfs_path,omitempty"`
	SubnetFile    string `json:"subnet_file,omitempty"`
	GatewayIP     string `json:"gateway_ip,omitempty"`
	GuestIP       string `json:"guest_ip,omitempty"`
	SubnetCIDR    string `json:"subnet_cidr,omitempty"`
	VsockPath     string `json:"vsock_path,omitempty"`
	TAPName       string `json:"tap_name,omitempty"`
	FirewallTable string `json:"firewall_table,omitempty"`
	NATTable      string `json:"nat_table,omitempty"`
}

type Record struct {
	Version   int                      `json:"version"`
	VMID      string                   `json:"vm_id"`
	Backend   string                   `json:"backend,omitempty"`
	Phase     Phase                    `json:"phase"`
	UpdatedAt time.Time                `json:"updated_at"`
	LastError string                   `json:"last_error,omitempty"`
	Resources Resources                `json:"resources,omitempty"`
	Cleanup   map[string]CleanupResult `json:"cleanup,omitempty"`
}

type Store struct {
	vmDir   string
	vmID    string
	db      *sql.DB
	initErr error
	mu      sync.Mutex
}

func NewStore(vmDir string) *Store {
	db, err := openLifecycleDB(vmDir)
	return &Store{
		vmDir:   vmDir,
		vmID:    filepath.Base(filepath.Clean(vmDir)),
		db:      db,
		initErr: err,
	}
}

func (s *Store) Path() string {
	return lifecycleDBPath(s.vmDir)
}

func (s *Store) Exists() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.readyNoLock(); err != nil {
		return false
	}
	vmID := s.resolveVMIDNoLock()
	if vmID == "" {
		return false
	}
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM vm_lifecycle WHERE vm_id = ?`, vmID).Scan(&one)
	return err == nil
}

func (s *Store) Init(vmID, backend, stateDir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.readyNoLock(); err != nil {
		return err
	}
	s.vmID = vmID
	rec := &Record{
		Version:   1,
		VMID:      vmID,
		Backend:   backend,
		Phase:     PhaseCreating,
		UpdatedAt: time.Now().UTC(),
		Resources: Resources{
			StateDir: stateDir,
		},
		Cleanup: make(map[string]CleanupResult),
	}
	return s.saveNoLock(rec)
}

func (s *Store) Load() (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyNoLock(); err != nil {
		return nil, err
	}
	return s.loadNoLock()
}

func (s *Store) History() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.readyNoLock(); err != nil {
		return nil, err
	}
	vmID := s.resolveVMIDNoLock()
	if vmID == "" {
		return nil, nil
	}

	rows, err := s.db.Query(
		`SELECT version, vm_id, backend, phase, updated_at, last_error, resources_json, cleanup_json
		   FROM vm_lifecycle
		  WHERE vm_id = ?
		  ORDER BY version ASC`,
		vmID,
	)
	if err != nil {
		return nil, errx.Wrap(ErrReadRecord, err)
	}
	defer rows.Close()

	history := make([]Record, 0, 8)
	for rows.Next() {
		var (
			version       int
			vmIDRow       string
			backend       string
			phase         string
			updatedAtText string
			lastError     string
			resourcesJSON []byte
			cleanupJSON   []byte
		)
		if err := rows.Scan(&version, &vmIDRow, &backend, &phase, &updatedAtText, &lastError, &resourcesJSON, &cleanupJSON); err != nil {
			return nil, errx.Wrap(ErrReadRecord, err)
		}
		rec, err := decodeLifecycleRecord(version, vmIDRow, backend, phase, updatedAtText, lastError, resourcesJSON, cleanupJSON)
		if err != nil {
			return nil, err
		}
		history = append(history, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errx.Wrap(ErrReadRecord, err)
	}
	return history, nil
}

func (s *Store) SetPhase(phase Phase) error {
	return s.Update(func(r *Record) error {
		if err := validateTransition(r.Phase, phase); err != nil {
			return err
		}
		r.Phase = phase
		return nil
	})
}

func (s *Store) SetLastError(err error) error {
	return s.Update(func(r *Record) error {
		if err == nil {
			r.LastError = ""
			return nil
		}
		r.LastError = err.Error()
		return nil
	})
}

func (s *Store) SetResource(updateFn func(*Resources)) error {
	return s.Update(func(r *Record) error {
		updateFn(&r.Resources)
		return nil
	})
}

func (s *Store) MarkCleanup(name string, opErr error) error {
	return s.Update(func(r *Record) error {
		if r.Cleanup == nil {
			r.Cleanup = make(map[string]CleanupResult)
		}
		result := CleanupResult{
			Status:    "ok",
			UpdatedAt: time.Now().UTC(),
		}
		if opErr != nil {
			result.Status = "error"
			result.Error = opErr.Error()
		}
		r.Cleanup[name] = result
		return nil
	})
}

func (s *Store) Update(updateFn func(*Record) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.readyNoLock(); err != nil {
		return err
	}
	rec, err := s.loadNoLock()
	if err != nil {
		return err
	}
	if err := updateFn(rec); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now().UTC()
	return s.saveNoLock(rec)
}

func (s *Store) readyNoLock() error {
	if s.initErr != nil {
		return errx.Wrap(ErrReadRecord, s.initErr)
	}
	if s.db == nil {
		return ErrReadRecord
	}
	return nil
}

func (s *Store) resolveVMIDNoLock() string {
	if s.vmID != "" && s.vmID != "." {
		return s.vmID
	}
	base := filepath.Base(filepath.Clean(s.vmDir))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

func (s *Store) loadNoLock() (*Record, error) {
	vmID := s.resolveVMIDNoLock()
	if vmID == "" {
		return &Record{
			Version: 0,
			Phase:   PhaseCreating,
			Cleanup: make(map[string]CleanupResult),
		}, nil
	}

	var (
		version       int
		vmIDRow       string
		backend       string
		phase         string
		updatedAtText string
		lastError     string
		resourcesJSON []byte
		cleanupJSON   []byte
	)
	err := s.db.QueryRow(
		`SELECT version, vm_id, backend, phase, updated_at, last_error, resources_json, cleanup_json
		   FROM vm_lifecycle
		  WHERE vm_id = ?
		  ORDER BY version DESC
		  LIMIT 1`,
		vmID,
	).Scan(&version, &vmIDRow, &backend, &phase, &updatedAtText, &lastError, &resourcesJSON, &cleanupJSON)
	if err == sql.ErrNoRows {
		return &Record{
			Version: 0,
			VMID:    vmID,
			Phase:   PhaseCreating,
			Cleanup: make(map[string]CleanupResult),
		}, nil
	}
	if err != nil {
		return nil, errx.Wrap(ErrReadRecord, err)
	}

	rec, err := decodeLifecycleRecord(version, vmIDRow, backend, phase, updatedAtText, lastError, resourcesJSON, cleanupJSON)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func decodeLifecycleRecord(
	version int,
	vmID string,
	backend string,
	phase string,
	updatedAtText string,
	lastError string,
	resourcesJSON []byte,
	cleanupJSON []byte,
) (Record, error) {
	rec := Record{
		Version:   version,
		VMID:      vmID,
		Backend:   backend,
		Phase:     Phase(phase),
		LastError: lastError,
		Cleanup:   make(map[string]CleanupResult),
	}

	if updatedAtText != "" {
		parsed, parseErr := time.Parse(time.RFC3339Nano, updatedAtText)
		if parseErr != nil {
			parsed, parseErr = time.Parse(time.RFC3339, updatedAtText)
			if parseErr != nil {
				return Record{}, errx.Wrap(ErrDecodeRecord, parseErr)
			}
		}
		rec.UpdatedAt = parsed
	}
	if len(resourcesJSON) > 0 {
		if err := json.Unmarshal(resourcesJSON, &rec.Resources); err != nil {
			return Record{}, errx.Wrap(ErrDecodeRecord, err)
		}
	}
	if len(cleanupJSON) > 0 {
		if err := json.Unmarshal(cleanupJSON, &rec.Cleanup); err != nil {
			return Record{}, errx.Wrap(ErrDecodeRecord, err)
		}
	}
	return rec, nil
}

func (s *Store) saveNoLock(rec *Record) error {
	if rec.VMID == "" {
		rec.VMID = s.resolveVMIDNoLock()
	}
	if rec.VMID == "" {
		return ErrWriteRecord
	}
	s.vmID = rec.VMID
	if rec.Cleanup == nil {
		rec.Cleanup = make(map[string]CleanupResult)
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}

	resourcesJSON, err := json.Marshal(rec.Resources)
	if err != nil {
		return errx.Wrap(ErrEncodeRecord, err)
	}
	cleanupJSON, err := json.Marshal(rec.Cleanup)
	if err != nil {
		return errx.Wrap(ErrEncodeRecord, err)
	}

	var lastBusyErr error
	for attempt := 0; attempt < recordWriteRetryAttempts; attempt++ {
		version, writeErr := s.saveAttemptNoLock(rec, resourcesJSON, cleanupJSON)
		if writeErr == nil {
			rec.Version = version
			return nil
		}
		if !isSQLiteBusy(writeErr) {
			return errx.Wrap(ErrWriteRecord, writeErr)
		}
		lastBusyErr = writeErr
		if attempt < recordWriteRetryAttempts-1 {
			time.Sleep(recordWriteRetryBaseWait * time.Duration(attempt+1))
		}
	}

	return errx.Wrap(ErrWriteRecord, lastBusyErr)
}

func (s *Store) saveAttemptNoLock(rec *Record, resourcesJSON, cleanupJSON []byte) (int, error) {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	inTx := true
	defer func() {
		if inTx {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	version, err := nextLifecycleVersion(ctx, conn, rec.VMID)
	if err != nil {
		return 0, err
	}

	if _, err := conn.ExecContext(
		ctx,
		`INSERT INTO vm_lifecycle(vm_id, version, backend, phase, updated_at, last_error, resources_json, cleanup_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.VMID,
		version,
		rec.Backend,
		string(rec.Phase),
		rec.UpdatedAt.UTC().Format(time.RFC3339Nano),
		rec.LastError,
		resourcesJSON,
		cleanupJSON,
	); err != nil {
		return 0, err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	inTx = false
	return version, nil
}

func nextLifecycleVersion(ctx context.Context, conn *sql.Conn, vmID string) (int, error) {
	var version int
	err := conn.QueryRowContext(
		ctx,
		`INSERT INTO vm_lifecycle_head(vm_id, last_version)
		 VALUES (?, COALESCE((SELECT MAX(version) FROM vm_lifecycle WHERE vm_id = ?), 0) + 1)
		 ON CONFLICT(vm_id) DO UPDATE SET
		   last_version = vm_lifecycle_head.last_version + 1
		 RETURNING last_version`,
		vmID,
		vmID,
	).Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & sqlitePrimaryErrMask {
		case sqlitePrimaryBusy, sqlitePrimaryLocked:
			return true
		}
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "database is busy")
}
