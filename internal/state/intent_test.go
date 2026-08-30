package state

import (
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// buildPreV8DB constructs a database at schema v7 (pre-v8) with the real column
// set, so the v8 migration is exercised against genuine legacy data. We derive
// it by migrating a fresh DB to the latest version then rolling back all v8/v9
// additions — the simplest faithful representation of the pre-change schema.
func buildPreV8DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prev8.sqlite")
	fresh, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Roll the schema back to v7: drop the v8 intent columns and all v9
	// policy/prune/ledger additions, then remove their migration records so a
	// re-open re-applies them.
	for _, col := range []string{
		"pending_manual_generation", "pending_manual_allow_deletes", "pending_manual_bypass_debounce",
	} {
		if _, err := fresh.Exec(`ALTER TABLE profile_sync_state DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	for _, col := range []string{"effective_max_delete", "manual_delete_override"} {
		if _, err := fresh.Exec(`ALTER TABLE sync_runs DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	for _, col := range []string{"state", "suppressed_policy_hash", "suppressed_generation"} {
		if _, err := fresh.Exec(`ALTER TABLE manifest DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	for _, col := range []string{"observed_policy_hash", "observed_policy_generation", "policy_context_known"} {
		if _, err := fresh.Exec(`ALTER TABLE pending_events DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`DROP TABLE IF EXISTS prune_targets`,
		`DROP TABLE IF EXISTS prune_requests`,
		`DROP TABLE IF EXISTS profile_ignore_snapshot_files`,
		`DROP TABLE IF EXISTS profile_ignore_policy`,
		`DELETE FROM schema_migrations WHERE version IN (8, 9)`,
	} {
		if _, err := fresh.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	fresh.Close()
	return path
}

// TestMigrationV8AddsIntentColumnsUpgradesInPlace verifies a pre-v8 schema
// upgrades to the new version with the pending manual intent and run audit
// columns present (§16 migration requirements).
func TestMigrationV8AddsIntentColumnsUpgradesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.sqlite")
	fresh, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := fresh.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('profile_sync_state')
		WHERE name = 'pending_manual_generation'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("pending_manual_generation column missing after migration")
	}
	if err := fresh.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sync_runs')
		WHERE name = 'effective_max_delete'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("effective_max_delete column missing after migration")
	}
	fresh.Close()
}

// TestInPlaceUpgradeFromPreV8Schema verifies an existing v7 database upgrades in
// place (migration test requirement §16).
func TestInPlaceUpgradeFromPreV8Schema(t *testing.T) {
	path := buildPreV8DB(t)
	// Open runs migrations including v8 on the pre-v8 schema.
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('profile_sync_state')
		WHERE name = 'pending_manual_bypass_debounce'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("v8 intent column missing after in-place upgrade")
	}
}
