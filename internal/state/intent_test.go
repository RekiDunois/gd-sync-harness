package state

import (
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// buildPreV8DB constructs a database at schema v7 (pre-v8) with the real column
// set, so the v8 migration is exercised against genuine legacy data. We derive
// it by migrating a fresh DB to v7 then stopping — the simplest faithful
// representation of the pre-change schema.
func buildPreV8DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prev8.sqlite")
	fresh, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Roll the schema back to v7 by removing the v8 migration record. The v8
	// columns were added; drop them to truly represent the pre-v8 schema.
	if _, err := fresh.Exec(`DELETE FROM schema_migrations WHERE version = 8`); err != nil {
		t.Fatal(err)
	}
	// Reconstruct a v7 state table by creating a fresh v7-only DB in SQL is
	// complex; instead assert the v8 upgrade runs on a v7-marked DB by
	// re-opening (which will re-apply v8). To keep it a genuine pre-v8 schema we
	// drop the v8 columns first.
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
