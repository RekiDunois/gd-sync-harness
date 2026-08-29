package state

import (
	"database/sql"
	"fmt"
)

const timeFmt = "2006-01-02T15:04:05.000Z07:00"

// migrate runs schema migrations in a transaction. Uses schema_version
// tracking to apply only new migrations.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}

	migrations := []string{
		// v1: initial schema
		`
		CREATE TABLE IF NOT EXISTS profiles (
			id TEXT PRIMARY KEY,
			profile_uuid TEXT NOT NULL UNIQUE,
			type TEXT NOT NULL,
			source_path TEXT NOT NULL,
			remote_name TEXT NOT NULL,
			remote_folder_id TEXT NOT NULL,
			remote_display_path TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			max_delete INTEGER NOT NULL DEFAULT 100,
			max_file_size INTEGER NOT NULL DEFAULT 536870912,
			deleted_at TEXT,
			tombstoned INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS profile_excludes (
			profile_id TEXT NOT NULL,
			rule_type TEXT NOT NULL,
			rule_value TEXT NOT NULL,
			PRIMARY KEY (profile_id, rule_type, rule_value)
		);
		CREATE TABLE IF NOT EXISTS pending_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			profile_id TEXT NOT NULL,
			path TEXT NOT NULL,
			event_kind TEXT NOT NULL,
			first_seen TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			source_generation INTEGER NOT NULL DEFAULT 0,
			UNIQUE (profile_id, path)
		);
		CREATE INDEX IF NOT EXISTS idx_pending_profile ON pending_events(profile_id);
		CREATE TABLE IF NOT EXISTS profile_runtime (
			profile_id TEXT PRIMARY KEY,
			source_generation INTEGER NOT NULL DEFAULT 0,
			reconcile_requested INTEGER NOT NULL DEFAULT 0,
			last_fast_success TEXT,
			last_reconcile_success TEXT,
			last_error TEXT,
			watcher_status TEXT NOT NULL DEFAULT 'stopped'
		);
		CREATE TABLE IF NOT EXISTS remotes (
			remote_name TEXT PRIMARY KEY,
			backend TEXT NOT NULL,
			last_quota_check TEXT,
			total_bytes INTEGER NOT NULL DEFAULT 0,
			used_bytes INTEGER NOT NULL DEFAULT 0,
			free_bytes INTEGER NOT NULL DEFAULT 0,
			quota_status TEXT NOT NULL DEFAULT 'unknown'
		);
		CREATE TABLE IF NOT EXISTS manifest (
			profile_id TEXT NOT NULL,
			rel_path TEXT NOT NULL,
			size INTEGER NOT NULL,
			mod_time INTEGER NOT NULL,
			hash TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (profile_id, rel_path)
		);
		`,
		// v2: settings table for persisted tool paths so launchd jobs do not
		// depend on interactive-shell PATH (§31.2).
		`
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		`,
		// v3: rebuild pending_events with the (profile_id, path) UNIQUE
		// constraint. Older databases created the table without it, breaking
		// the ON CONFLICT upsert used to dedupe watcher events.
		`
		CREATE TABLE pending_events_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			profile_id TEXT NOT NULL,
			path TEXT NOT NULL,
			event_kind TEXT NOT NULL,
			first_seen TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			source_generation INTEGER NOT NULL DEFAULT 0,
			UNIQUE (profile_id, path)
		);
		INSERT INTO pending_events_new (id, profile_id, path, event_kind, first_seen, last_seen, source_generation)
			SELECT id, profile_id, path, event_kind, first_seen, last_seen, source_generation FROM pending_events;
		DROP TABLE pending_events;
		ALTER TABLE pending_events_new RENAME TO pending_events;
		CREATE INDEX idx_pending_profile ON pending_events(profile_id);
		`,
	}

	for i, m := range migrations {
		version := i + 1
		if version <= current {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration v%d: %w", version, err)
		}
		if _, err := tx.Exec(m); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration v%d: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, Now().Format(timeFmt)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration v%d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v%d: %w", version, err)
		}
	}
	return nil
}
