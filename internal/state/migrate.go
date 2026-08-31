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
		// v4: durable asynchronous initial sync (§7.2, §8). Adds the
		// profile_sync_state table as the sole durable reconciliation intent
		// authority, the sync_runs attempt/history table, and a durable
		// deletion lifecycle gate on profiles. Existing profiles are backfilled
		// from trustworthy durable success evidence (last_reconcile_success) per
		// §23.1; profiles without evidence are scheduled for reconciliation so
		// initialization evidence can be established.
		`
		ALTER TABLE profiles ADD COLUMN deletion_requested_at TEXT;
		CREATE TABLE profile_sync_state (
			profile_id TEXT PRIMARY KEY,
			desired_generation INTEGER NOT NULL DEFAULT 0,
			last_success_generation INTEGER,
			initialized_at TEXT,
			last_success_at TEXT,
			current_run_id TEXT,
			state TEXT NOT NULL DEFAULT 'initializing',
			phase TEXT,
			retry_classification TEXT,
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			next_retry_at TEXT,
			last_progress_at TEXT,
			last_error_code TEXT,
			last_error TEXT
		);
		CREATE TABLE sync_runs (
			id TEXT PRIMARY KEY,
			profile_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			target_generation INTEGER NOT NULL,
			status TEXT NOT NULL,
			phase TEXT,
			started_at TEXT NOT NULL,
			completed_at TEXT,
			files_discovered INTEGER NOT NULL DEFAULT 0,
			files_completed INTEGER NOT NULL DEFAULT 0,
			bytes_total INTEGER NOT NULL DEFAULT 0,
			bytes_completed INTEGER NOT NULL DEFAULT 0,
			last_progress_at TEXT,
			error_code TEXT NOT NULL DEFAULT '',
			error_classification TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX idx_sync_runs_profile ON sync_runs(profile_id);
		`,
		// v5: migration-time backfill of profile_sync_state from trustworthy
		// durable success evidence (§23.1). Profiles whose last_reconcile_success
		// is set become initialized with last_success_generation = source_generation
		// (the stored generation). Profiles with no durable success evidence
		// keep last_success_generation NULL (unproven initialization) and get
		// desired_generation advanced so a worker reconciliation establishes
		// evidence; they are never inferred ready from profile existence.
		`
		INSERT OR IGNORE INTO profile_sync_state (
			profile_id, desired_generation, last_success_generation,
			initialized_at, last_success_at, state
		)
		SELECT p.id,
			COALESCE(r.source_generation, 0),
			CASE WHEN r.last_reconcile_success IS NULL THEN NULL ELSE COALESCE(r.source_generation, 0) END,
			r.last_reconcile_success,
			r.last_reconcile_success,
			CASE WHEN r.last_reconcile_success IS NULL THEN 'initializing' ELSE 'ready' END
		FROM profiles p
		LEFT JOIN profile_runtime r ON r.profile_id = p.id;

		UPDATE profile_sync_state SET
			state = CASE
				WHEN last_success_generation IS NULL THEN 'initializing'
				WHEN desired_generation > last_success_generation THEN 'syncing'
				ELSE 'ready'
			END;

		UPDATE profile_sync_state
		SET desired_generation = 1
		WHERE last_success_generation IS NULL
			AND desired_generation = 0;
		`,
		// v6: structured rclone progress and durable destructive debounce.
		// These are additive so existing profile, manifest, pending, and run
		// history remain intact during an in-place upgrade.
		`
		ALTER TABLE profile_sync_state ADD COLUMN last_heartbeat_at TEXT;
		ALTER TABLE profile_sync_state ADD COLUMN reconcile_not_before_at TEXT;
		ALTER TABLE profile_sync_state ADD COLUMN reconcile_deadline_at TEXT;
		ALTER TABLE profile_sync_state ADD COLUMN limited_failures INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN last_heartbeat_at TEXT;
		ALTER TABLE sync_runs ADD COLUMN checks_completed INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN checks_total INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN items_listed INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN errors_count INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN speed_bytes_per_second REAL NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN current_item TEXT;
		ALTER TABLE sync_runs ADD COLUMN current_item_bytes INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN current_item_size INTEGER NOT NULL DEFAULT 0;
		CREATE TABLE remote_operation_leases (
			id TEXT PRIMARY KEY,
			remote_name TEXT NOT NULL,
			priority INTEGER NOT NULL,
			owner_pid INTEGER NOT NULL,
			state TEXT NOT NULL,
			created_at TEXT NOT NULL,
			lease_until TEXT NOT NULL
		);
		CREATE INDEX idx_remote_leases ON remote_operation_leases(remote_name, state, priority, created_at);
		`,
		// v7: transfer concurrency and upload throughput anchor.
		`
		ALTER TABLE sync_runs ADD COLUMN active_transfers INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN upload_started_at TEXT;
		`,
		// v8: worker live status telemetry and durable one-attempt manual intent
		// (§10, §11, §16). Pending manual execution metadata attaches to the
		// existing desired_generation intent (no second request queue);
		// sync_runs gains the effective destructive budget audit fields.
		`
		ALTER TABLE profile_sync_state ADD COLUMN pending_manual_generation INTEGER;
		ALTER TABLE profile_sync_state ADD COLUMN pending_manual_allow_deletes INTEGER;
		ALTER TABLE profile_sync_state ADD COLUMN pending_manual_bypass_debounce INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN effective_max_delete INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE sync_runs ADD COLUMN manual_delete_override INTEGER;
		`,
		// v9: durable committed ignore policy snapshot (§4, §18.1) and the
		// active/suppressed managed-ledger evolution (§10). The manifest gains a
		// state column so a full eligible scan cannot erase suppressed ownership
		// records; existing rows migrate as active. pending_events gains
		// durable policy-order evidence for delete-vs-suppress classification
		// (§9.2).
		`
		CREATE TABLE profile_ignore_policy (
			profile_id TEXT PRIMARY KEY,
			policy_source TEXT NOT NULL,
			policy_hash TEXT NOT NULL,
			committed_generation INTEGER NOT NULL,
			committed_at TEXT NOT NULL,
			refresh_state TEXT NOT NULL DEFAULT 'pending',
			refreshed_policy_hash TEXT,
			matcher_warning_count INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
		);
		CREATE TABLE profile_ignore_snapshot_files (
			profile_id TEXT NOT NULL,
			policy_hash TEXT NOT NULL,
			relative_path TEXT NOT NULL,
			scope_dir TEXT NOT NULL,
			content BLOB NOT NULL,
			content_order INTEGER NOT NULL,
			PRIMARY KEY(profile_id, policy_hash, relative_path),
			FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
		);
		CREATE INDEX idx_ignore_snapshot_profile ON profile_ignore_snapshot_files(profile_id);
		ALTER TABLE manifest ADD COLUMN state TEXT NOT NULL DEFAULT 'active';
		ALTER TABLE manifest ADD COLUMN suppressed_policy_hash TEXT;
		ALTER TABLE manifest ADD COLUMN suppressed_generation INTEGER;
		ALTER TABLE pending_events ADD COLUMN observed_policy_hash TEXT;
		ALTER TABLE pending_events ADD COLUMN observed_policy_generation INTEGER;
		ALTER TABLE pending_events ADD COLUMN policy_context_known INTEGER NOT NULL DEFAULT 0;
		CREATE TABLE prune_requests (
			request_id TEXT PRIMARY KEY,
			profile_id TEXT NOT NULL,
			policy_hash TEXT NOT NULL,
			state TEXT NOT NULL,
			candidate_count INTEGER NOT NULL,
			candidate_digest TEXT NOT NULL,
			default_max_delete INTEGER NOT NULL,
			authorized_limit INTEGER,
			deleted_count INTEGER NOT NULL DEFAULT 0,
			missing_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			completed_at TEXT,
			FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
		);
		CREATE TABLE prune_targets (
			request_id TEXT NOT NULL,
			rel_path TEXT NOT NULL,
			state TEXT NOT NULL,
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(request_id, rel_path),
			FOREIGN KEY(request_id) REFERENCES prune_requests(request_id) ON DELETE CASCADE
		);
		CREATE INDEX idx_prune_requests_profile ON prune_requests(profile_id, state, created_at);
		CREATE INDEX idx_prune_targets_request ON prune_targets(request_id);
		`,
		// v10: reserve the derived namespace in the managed ledger. Any frozen
		// prune request containing a reserved target is stale before it can be
		// executed.
		`
		UPDATE manifest SET state = 'protected', suppressed_policy_hash = NULL,
			suppressed_generation = NULL
		WHERE rel_path = '.knowledge-derived' OR rel_path LIKE '.knowledge-derived/%';
		UPDATE prune_requests SET state = 'stale', updated_at = CURRENT_TIMESTAMP
		WHERE state NOT IN ('completed', 'stale', 'superseded')
		AND EXISTS (
			SELECT 1 FROM prune_targets t
			WHERE t.request_id = prune_requests.request_id
			AND (t.rel_path = '.knowledge-derived' OR t.rel_path LIKE '.knowledge-derived/%')
		);
		`,
		// v11: local compiler operational state and derived desired-state shell.
		`
		CREATE TABLE compiler_runs (
			id TEXT PRIMARY KEY,
			profile_id TEXT NOT NULL,
			candidate_generation_id TEXT,
			started_at TEXT NOT NULL,
			completed_at TEXT,
			status TEXT NOT NULL,
			compiler_version TEXT NOT NULL,
			schema_version INTEGER NOT NULL,
			source_snapshot_id TEXT,
			policy_hash TEXT,
			eligibility_contract_hash TEXT,
			file_count INTEGER NOT NULL DEFAULT 0,
			warning_count INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
		);
		CREATE INDEX idx_compiler_runs_profile ON compiler_runs(profile_id, started_at);
		CREATE TABLE compiler_profile_state (
			profile_id TEXT PRIMARY KEY,
			last_success_generation_id TEXT,
			last_success_at TEXT,
			last_source_snapshot_id TEXT,
			last_policy_hash TEXT,
			last_eligibility_contract_hash TEXT,
			local_mode TEXT NOT NULL DEFAULT 'absent',
			local_clean_state TEXT NOT NULL DEFAULT 'none',
			local_clean_operation_id TEXT,
			desired_derived_mode TEXT NOT NULL DEFAULT 'absent',
			desired_derived_generation_id TEXT,
			desired_derived_revision INTEGER NOT NULL DEFAULT 0,
			current_remote_binding_fingerprint TEXT,
			remote_published_generation_id TEXT,
			remote_published_binding_fingerprint TEXT,
			remote_state TEXT NOT NULL DEFAULT 'unknown',
			remote_state_binding_fingerprint TEXT,
			derived_state TEXT NOT NULL DEFAULT 'pending',
			active_publish_generation_id TEXT,
			last_derived_error TEXT,
			last_derived_success_at TEXT,
			FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
		);
		INSERT OR IGNORE INTO compiler_profile_state (profile_id)
			SELECT id FROM profiles;
		`,
		// v12: durable DerivedSync attempt history and retry gate.
		`
		ALTER TABLE compiler_profile_state ADD COLUMN derived_retry_target_key TEXT;
		ALTER TABLE compiler_profile_state ADD COLUMN derived_retry_classification TEXT;
		ALTER TABLE compiler_profile_state ADD COLUMN derived_consecutive_failures INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE compiler_profile_state ADD COLUMN derived_limited_failures INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE compiler_profile_state ADD COLUMN derived_next_retry_at TEXT;
		ALTER TABLE compiler_profile_state ADD COLUMN derived_terminal_error_code TEXT;
		CREATE TABLE compiler_derived_runs (
			id TEXT PRIMARY KEY,
			profile_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			target_generation_id TEXT,
			target_binding_fingerprint TEXT NOT NULL,
			target_desired_revision INTEGER NOT NULL,
			status TEXT NOT NULL,
			phase TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			completed_at TEXT,
			files_completed INTEGER NOT NULL DEFAULT 0,
			bytes_completed INTEGER NOT NULL DEFAULT 0,
			last_progress_at TEXT,
			error_code TEXT NOT NULL DEFAULT '',
			error_classification TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(profile_id) REFERENCES profiles(id) ON DELETE CASCADE
		);
		CREATE INDEX idx_compiler_derived_runs_profile ON compiler_derived_runs(profile_id, started_at);
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
