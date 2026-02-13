package storedb

import "errors"

var (
	ErrDBPathRequired     = errors.New("database path is required")
	ErrModuleRequired     = errors.New("migration module is required")
	ErrOpenDB             = errors.New("open database")
	ErrOpenInitLock       = errors.New("open database init lock")
	ErrAcquireInitLock    = errors.New("acquire database init lock")
	ErrReleaseInitLock    = errors.New("release database init lock")
	ErrConfigureDB        = errors.New("configure database")
	ErrCreateMigrationTbl = errors.New("create schema_migrations table")
	ErrReadMigrations     = errors.New("read applied migrations")
	ErrDuplicateMigration = errors.New("duplicate migration version")
	ErrCreateBackup       = errors.New("create pre-migration backup")
	ErrApplyMigration     = errors.New("apply migration")
	ErrCommitMigration    = errors.New("commit migration")
	ErrRecordMigration    = errors.New("record migration")
	ErrRollbackRestore    = errors.New("restore database from backup")
	ErrRemoveBackup       = errors.New("remove migration backup")
	ErrCopyBackup         = errors.New("copy migration backup")
)
