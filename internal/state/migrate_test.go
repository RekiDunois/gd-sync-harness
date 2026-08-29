package state

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// buildLegacyV3DB constructs a database at schema v3 (pre-migration) with the
// given profiles and runtime rows, so migrate() can be exercised against real
// legacy data.
func buildLegacyV3DB(t *testing.T, profiles map[string]*legacyRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(q string) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	mustExec(`CREATE TABLE profiles (
		id TEXT PRIMARY KEY, profile_uuid TEXT NOT NULL UNIQUE, type TEXT NOT NULL,
		source_path TEXT NOT NULL, remote_name TEXT NOT NULL, remote_folder_id TEXT NOT NULL,
		remote_display_path TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
		max_delete INTEGER NOT NULL DEFAULT 100, max_file_size INTEGER NOT NULL DEFAULT 536870912,
		deleted_at TEXT, tombstoned INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`)
	mustExec(`CREATE TABLE profile_runtime (
		profile_id TEXT PRIMARY KEY, source_generation INTEGER NOT NULL DEFAULT 0,
		reconcile_requested INTEGER NOT NULL DEFAULT 0, last_fast_success TEXT,
		last_reconcile_success TEXT, last_error TEXT, watcher_status TEXT NOT NULL DEFAULT 'stopped')`)
	mustExec(`CREATE TABLE profile_excludes (
		profile_id TEXT NOT NULL, rule_type TEXT NOT NULL, rule_value TEXT NOT NULL,
		PRIMARY KEY (profile_id, rule_type, rule_value))`)
	mustExec(`CREATE TABLE pending_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, profile_id TEXT NOT NULL, path TEXT NOT NULL,
		event_kind TEXT NOT NULL, first_seen TEXT NOT NULL, last_seen TEXT NOT NULL,
		source_generation INTEGER NOT NULL DEFAULT 0, UNIQUE (profile_id, path))`)
	mustExec(`CREATE TABLE remotes (
		remote_name TEXT PRIMARY KEY, backend TEXT NOT NULL, last_quota_check TEXT,
		total_bytes INTEGER NOT NULL DEFAULT 0, used_bytes INTEGER NOT NULL DEFAULT 0,
		free_bytes INTEGER NOT NULL DEFAULT 0, quota_status TEXT NOT NULL DEFAULT 'unknown')`)
	mustExec(`CREATE TABLE manifest (
		profile_id TEXT NOT NULL, rel_path TEXT NOT NULL, size INTEGER NOT NULL,
		mod_time INTEGER NOT NULL, hash TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (profile_id, rel_path))`)
	mustExec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	mustExec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`)

	for id, r := range profiles {
		del := "NULL"
		if r.DeletedAt != "" {
			del = "'" + r.DeletedAt + "'"
		}
		tomb := 0
		if r.Tombstoned {
			tomb = 1
		}
		if _, err := db.Exec(`INSERT INTO profiles (id, profile_uuid, type, source_path, remote_name,
			remote_folder_id, remote_display_path, enabled, max_delete, max_file_size,
			deleted_at, tombstoned, created_at, updated_at) VALUES (?, 'u-'||?, 'generic', '/s', 'g', 'f', 'p',
			?, 100, 0, `+del+`, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			id, id, boolInt(!r.Disabled), tomb); err != nil {
			t.Fatalf("insert profile %s: %v", id, err)
		}
		if _, err := db.Exec(`INSERT INTO profile_runtime (profile_id, source_generation, reconcile_requested,
			last_fast_success, last_reconcile_success, last_error, watcher_status)
			VALUES (?, ?, 0, NULL, ?, NULL, 'stopped')`,
			id, r.SourceGeneration, nullOrEmpty(r.LastReconcileSuccess)); err != nil {
			t.Fatalf("insert runtime %s: %v", id, err)
		}
	}
	mustExec(`INSERT INTO schema_migrations (version, applied_at) VALUES (3, '2026-01-01T00:00:00Z')`)
	return path
}

type legacyRow struct {
	SourceGeneration     int64
	LastReconcileSuccess string // empty = none
	Disabled             bool
	Tombstoned           bool
	DeletedAt            string // empty = none
}

func nullOrEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// TestMigrationBackfillsSuccessEvidence verifies §23.1: profiles with
// trustworthy durable success evidence become initialized; profiles without
// evidence get durable reconciliation intent (never inferred ready).
func TestMigrationBackfillsSuccessEvidence(t *testing.T) {
	path := buildLegacyV3DB(t, map[string]*legacyRow{
		"ready-profile": {SourceGeneration: 7, LastReconcileSuccess: "2026-08-01T00:00:00.000Z"},
		"fresh-profile": {SourceGeneration: 0, LastReconcileSuccess: ""},
	})

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// ready-profile: backfilled initialized with matching generation, no debt.
	ss, err := db.GetSyncState("ready-profile")
	if err != nil {
		t.Fatal(err)
	}
	if !ss.IsInitialized() {
		t.Fatalf("ready-profile should be backfilled initialized; ss=%+v", ss)
	}
	if ss.LastSuccessGeneration == nil || *ss.LastSuccessGeneration != 7 {
		t.Fatalf("last_success_generation = %v, want 7", ss.LastSuccessGeneration)
	}
	if ss.HasDebt() {
		t.Fatalf("ready-profile should have no debt after backfill; ss=%+v", ss)
	}

	// fresh-profile: no evidence → must NOT be ready; reconciliation scheduled.
	ss2, err := db.GetSyncState("fresh-profile")
	if err != nil {
		t.Fatal(err)
	}
	if ss2.IsInitialized() {
		t.Fatal("fresh-profile without evidence must not be inferred initialized")
	}
	if ss2.State != StateInitializing {
		t.Fatalf("fresh-profile state = %s, want initializing", ss2.State)
	}
	if !ss2.HasDebt() {
		t.Fatal("fresh-profile must have durable reconciliation debt after migration")
	}

	// A claim must be possible for fresh-profile (worker will converge it).
	run, res, err := db.ClaimRun("fresh-profile", "mig-run-1")
	if err != nil || res != ClaimOK {
		t.Fatalf("claim fresh-profile: res=%v err=%v", res, err)
	}
	if run.Kind != RunKindInitial {
		t.Fatalf("run kind = %s, want initial", run.Kind)
	}
}
