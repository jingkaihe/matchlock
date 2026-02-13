package storedb

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jingkaihe/matchlock/internal/errx"
	_ "modernc.org/sqlite"
)

type Migration struct {
	Version int
	Name    string
	SQL     string
}

type OpenOptions struct {
	Path       string
	Module     string
	Migrations []Migration
}

func Open(opts OpenOptions) (*sql.DB, error) {
	if opts.Path == "" {
		return nil, ErrDBPathRequired
	}
	if opts.Module == "" {
		return nil, ErrModuleRequired
	}

	if err := os.MkdirAll(filepath.Dir(opts.Path), 0700); err != nil {
		return nil, errx.Wrap(ErrOpenDB, err)
	}

	db, err := sql.Open("sqlite", opts.Path)
	if err != nil {
		return nil, errx.Wrap(ErrOpenDB, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if err := withInitLock(opts.Path, func() error {
		if err := configure(db); err != nil {
			return err
		}
		return migrate(db, opts.Path, opts.Module, opts.Migrations)
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func configure(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA busy_timeout = 15000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA journal_mode = WAL",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			return errx.With(ErrConfigureDB, ": %s: %w", pragma, err)
		}
	}
	return nil
}

func migrate(db *sql.DB, dbPath, module string, migrations []Migration) error {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
  module TEXT NOT NULL,
  version INTEGER NOT NULL,
  name TEXT NOT NULL,
  applied_at TEXT NOT NULL,
  PRIMARY KEY (module, version)
)`); err != nil {
		return errx.Wrap(ErrCreateMigrationTbl, err)
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	seen := make(map[int]bool, len(migrations))
	for _, m := range migrations {
		if seen[m.Version] {
			return errx.With(ErrDuplicateMigration, ": module=%s version=%d", module, m.Version)
		}
		seen[m.Version] = true
	}

	applied := make(map[int]bool, len(migrations))
	rows, err := db.Query(`SELECT version FROM schema_migrations WHERE module = ?`, module)
	if err != nil {
		return errx.Wrap(ErrReadMigrations, err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return errx.Wrap(ErrReadMigrations, err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return errx.Wrap(ErrReadMigrations, err)
	}
	rows.Close()

	pending := make([]Migration, 0, len(migrations))
	for _, m := range migrations {
		if !applied[m.Version] {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	backupPath := dbPath + ".premigrate.bak"
	if err := createBackup(db, backupPath); err != nil {
		return err
	}
	removeBackup := func() error {
		if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
			return errx.Wrap(ErrRemoveBackup, err)
		}
		return nil
	}

	for _, m := range pending {
		tx, err := db.Begin()
		if err != nil {
			_ = db.Close()
			restoreErr := restoreFromBackup(dbPath, backupPath)
			removeErr := removeBackup()
			return joinMigrationErrors(errx.With(ErrApplyMigration, ": begin %s/%d %s: %w", module, m.Version, m.Name, err), restoreErr, removeErr)
		}

		if _, err := tx.Exec(m.SQL); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			restoreErr := restoreFromBackup(dbPath, backupPath)
			removeErr := removeBackup()
			return joinMigrationErrors(errx.With(ErrApplyMigration, ": %s/%d %s: %w", module, m.Version, m.Name, err), restoreErr, removeErr)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations(module, version, name, applied_at) VALUES (?, ?, ?, ?)`,
			module,
			m.Version,
			m.Name,
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			restoreErr := restoreFromBackup(dbPath, backupPath)
			removeErr := removeBackup()
			return joinMigrationErrors(errx.With(ErrRecordMigration, ": %s/%d %s: %w", module, m.Version, m.Name, err), restoreErr, removeErr)
		}
		if err := tx.Commit(); err != nil {
			_ = db.Close()
			restoreErr := restoreFromBackup(dbPath, backupPath)
			removeErr := removeBackup()
			return joinMigrationErrors(errx.With(ErrCommitMigration, ": %s/%d %s: %w", module, m.Version, m.Name, err), restoreErr, removeErr)
		}
	}

	return removeBackup()
}

func joinMigrationErrors(base error, restoreErr error, removeErr error) error {
	var errs []error
	errs = append(errs, base)
	if restoreErr != nil {
		errs = append(errs, restoreErr)
	}
	if removeErr != nil {
		errs = append(errs, removeErr)
	}
	return errors.Join(errs...)
}

func createBackup(db *sql.DB, backupPath string) error {
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return errx.Wrap(ErrCreateBackup, err)
	}
	escaped := strings.ReplaceAll(backupPath, "'", "''")
	if _, err := db.Exec("VACUUM INTO '" + escaped + "'"); err != nil {
		return errx.Wrap(ErrCreateBackup, err)
	}
	return nil
}

func restoreFromBackup(dbPath, backupPath string) error {
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm", dbPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return errx.Wrap(ErrRollbackRestore, err)
		}
	}

	src, err := os.Open(backupPath)
	if err != nil {
		return errx.Wrap(ErrRollbackRestore, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dbPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return errx.Wrap(ErrRollbackRestore, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return errx.Wrap(ErrCopyBackup, err)
	}
	if err := dst.Close(); err != nil {
		return errx.Wrap(ErrCopyBackup, err)
	}
	return nil
}

func withInitLock(dbPath string, fn func() error) error {
	lockPath := dbPath + ".init.lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return errx.Wrap(ErrOpenInitLock, err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return errx.Wrap(ErrAcquireInitLock, err)
	}

	fnErr := fn()

	unlockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	if unlockErr != nil {
		unlockWrapped := errx.Wrap(ErrReleaseInitLock, unlockErr)
		if fnErr != nil {
			return errors.Join(fnErr, unlockWrapped)
		}
		return unlockWrapped
	}

	return fnErr
}
