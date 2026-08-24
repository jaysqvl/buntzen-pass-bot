// Package store owns the durable SQLite state for the Go control plane.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	secretcrypto "github.com/jaysqvl/buntzen-pass-bot/internal/crypto"
	"github.com/jaysqvl/buntzen-pass-bot/internal/lockfile"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound            = errors.New("record not found")
	ErrConflict            = errors.New("record conflicts with existing state")
	ErrUnversionedDatabase = errors.New("refusing to overwrite an unversioned database")
	ErrTransitionConflict  = errors.New("job state transition conflict")
	ErrDecisionConflict    = errors.New("job already has a different decision")
	ErrResourceLimit       = fmt.Errorf("%w: per-user resource limit reached", ErrConflict)
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Store struct {
	db        *sql.DB
	encryptor *secretcrypto.Encryptor
	path      string
	now       func() time.Time
}

func Open(ctx context.Context, databasePath string, encryptor *secretcrypto.Encryptor) (*Store, error) {
	if strings.TrimSpace(databasePath) == "" {
		return nil, errors.New("database path is required")
	}
	if encryptor == nil {
		return nil, errors.New("encryptor is required")
	}
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	abs, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	databaseURL := (&url.URL{Scheme: "file", Path: abs}).String()
	dsn := databaseURL + "?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := os.Chmod(abs, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure database file: %w", err)
	}
	return &Store{db: db, encryptor: encryptor, path: abs, now: func() time.Time { return time.Now().UTC() }}, nil
}

func OpenMigrated(ctx context.Context, databasePath string, encryptor *secretcrypto.Encryptor) (*Store, error) {
	abs, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	lock, err := lockfile.Acquire(abs + ".migrate.lock")
	if err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer lock.Close()
	store, err := Open(ctx, databasePath, encryptor)
	if err != nil {
		return nil, err
	}
	if err := store.migrateUnlocked(ctx); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Migrate(ctx context.Context) error {
	lock, err := lockfile.Acquire(s.path + ".migrate.lock")
	if err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer lock.Close()
	return s.migrateUnlocked(ctx)
}

func (s *Store) migrateUnlocked(ctx context.Context) error {
	var migrationTable int
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'",
	).Scan(&migrationTable); err != nil {
		return fmt.Errorf("inspect migration state: %w", err)
	}
	if migrationTable == 0 {
		var existingTables int
		if err := s.db.QueryRowContext(ctx, `
			SELECT count(*) FROM sqlite_master
			WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		`).Scan(&existingTables); err != nil {
			return fmt.Errorf("inspect existing schema: %w", err)
		}
		if existingTables > 0 {
			return ErrUnversionedDatabase
		}
		if _, err := s.db.ExecContext(ctx, `
			CREATE TABLE schema_migrations (
				version INTEGER PRIMARY KEY,
				name TEXT NOT NULL,
				applied_at TEXT NOT NULL
			)
		`); err != nil {
			return fmt.Errorf("create migration registry: %w", err)
		}
	}

	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		var version int
		if _, err := fmt.Sscanf(entry.Name(), "%04d_", &version); err != nil || version <= 0 {
			return fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		var applied int
		if err := s.db.QueryRowContext(ctx,
			"SELECT count(*) FROM schema_migrations WHERE version = ?", version,
		).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %d: %w", version, err)
		}
		if applied > 0 {
			continue
		}
		script, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %d: %w", version, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)",
			version, entry.Name(), formatTime(s.now()),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func formatTime(value time.Time) string {
	// Fixed-width UTC timestamps preserve chronological ordering in SQLite TEXT
	// comparisons, including values that fall within the same second.
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func parseTime(value string) (time.Time, error) {
	result, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse database timestamp %q: %w", value, err)
	}
	return result, nil
}

func parseOptionalTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func optionalTimeValue(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func mapWriteError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "limit reached") {
		return fmt.Errorf("%w: %v", ErrResourceLimit, err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "foreign key constraint failed") {
		return fmt.Errorf("%w: record is in use or references a missing dependency", ErrConflict)
	}
	return err
}

func (s *Store) classifyGuardedUpdate(ctx context.Context, table string, id int64, result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected row count: %w", err)
	}
	if count > 0 {
		return nil
	}
	var exists int
	// table is always a package-owned constant at the call sites below.
	if err := s.db.QueryRowContext(ctx, fmt.Sprintf("SELECT count(*) FROM %s WHERE id = ?", table), id).Scan(&exists); err != nil {
		return fmt.Errorf("classify guarded update: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	return fmt.Errorf("%w: record has a queued or active job", ErrConflict)
}

func (s *Store) classifyOwnedGuardedUpdate(
	ctx context.Context,
	table string,
	userID, id int64,
	result sql.Result,
) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected row count: %w", err)
	}
	if count > 0 {
		return nil
	}
	var exists int
	// table is always a package-owned constant at the call sites below.
	if err := s.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT count(*) FROM %s WHERE id = ? AND user_id = ?", table),
		id, userID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("classify owned guarded update: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	return fmt.Errorf("%w: record has a queued or active job", ErrConflict)
}
