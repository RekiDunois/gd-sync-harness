package state

import (
	"database/sql"
	"errors"
)

// ManualReconcileIntent is the one-attempt manual execution metadata attached
// to a durable generation (§10.1, §10.2). It is never a second work queue:
// desired_generation remains the sole reconciliation-intent authority.
type ManualReconcileIntent struct {
	AllowDeletes   int
	BypassDebounce bool
}

// PendingManual carries the pending manual metadata read back from the DB.
type PendingManual struct {
	Generation     int64
	AllowDeletes   int
	BypassDebounce bool
	Consumed       bool // true when no unconsumed manual intent exists
}

// SubmitManualReconcile advances desired_generation by at least one durable
// opportunity (even when the source generation did not change), reopens the
// manual gate if one is closed, and records the new one-attempt manual
// metadata, replacing any previously unconsumed manual metadata (§10.2).
//
// It returns the exact submitted generation so a CLI waiter can bind to it
// (§14.4).
func (d *DB) SubmitManualReconcile(profileID string, intent ManualReconcileIntent) (int64, error) {
	if err := d.EnsureSyncState(profileID); err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Manual requests reopen the terminal/retry gate: an explicit reconcile-now
	// is the documented way to clear a terminal error (§18.4).
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		retry_classification = NULL,
		next_retry_at = NULL,
		limited_failures = 0,
		reconcile_not_before_at = NULL,
		reconcile_deadline_at = NULL
		WHERE profile_id = ?`, profileID); err != nil {
		return 0, err
	}
	// Advance the durable generation by at least one opportunity past the
	// current desired generation. Source-generation coalescing is preserved:
	// the manual request adds one manual opportunity on top of any known debt.
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		desired_generation = MAX(
			desired_generation + 1,
			COALESCE((SELECT source_generation FROM profile_runtime WHERE profile_id = ?), 0),
			COALESCE(last_success_generation, 0) + 1
		)
		WHERE profile_id = ?`, profileID, profileID); err != nil {
		return 0, err
	}
	var submitted int64
	if err := tx.QueryRow(`SELECT desired_generation FROM profile_sync_state WHERE profile_id = ?`, profileID).Scan(&submitted); err != nil {
		return 0, err
	}
	// Record (replacing) the unconsumed manual metadata for this submission.
	bypass := 0
	if intent.BypassDebounce {
		bypass = 1
	}
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		pending_manual_generation = ?,
		pending_manual_allow_deletes = ?,
		pending_manual_bypass_debounce = ?,
		state = CASE
			WHEN state = ? THEN state
			WHEN last_success_generation IS NULL THEN ?
			ELSE ?
		END
		WHERE profile_id = ?`,
		submitted, intent.AllowDeletes, bypass, StateError, StateInitializing, StateSyncing, profileID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE profile_runtime SET reconcile_requested = 1 WHERE profile_id = ?`, profileID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return submitted, nil
}

// SubmitScheduledReconcile advances/coalesces the durable generation for a
// scheduled safety reconciliation without overriding pending manual metadata
// (§10.3). Scheduled runs preserve the destructive debounce semantics that
// manual reconcile may bypass.
func (d *DB) SubmitScheduledReconcile(profileID string) (int64, error) {
	if err := d.EnsureSyncState(profileID); err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		desired_generation = MAX(
			desired_generation + 1,
			COALESCE((SELECT source_generation FROM profile_runtime WHERE profile_id = ?), 0),
			COALESCE(last_success_generation, 0) + 1
		)
		WHERE profile_id = ?`, profileID, profileID); err != nil {
		return 0, err
	}
	var submitted int64
	if err := tx.QueryRow(`SELECT desired_generation FROM profile_sync_state WHERE profile_id = ?`, profileID).Scan(&submitted); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		state = CASE
			WHEN state = ? THEN state
			WHEN last_success_generation IS NULL THEN ?
			ELSE ?
		END
		WHERE profile_id = ?`, StateError, StateInitializing, StateSyncing, profileID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return submitted, nil
}

// ReadPendingManual returns the unconsumed manual metadata for a profile, if
// any (§10.4 merge rules). A nil-generation result means no manual intent is
// pending.
func (d *DB) ReadPendingManual(profileID string) (*PendingManual, error) {
	var gen sql.NullInt64
	var allow, bypass sql.NullInt64
	err := d.QueryRow(`SELECT pending_manual_generation,
		COALESCE(pending_manual_allow_deletes, 0),
		COALESCE(pending_manual_bypass_debounce, 0)
		FROM profile_sync_state WHERE profile_id = ?`, profileID).
		Scan(&gen, &allow, &bypass)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &PendingManual{Consumed: true}, nil
		}
		return nil, err
	}
	if !gen.Valid {
		return &PendingManual{Consumed: true}, nil
	}
	return &PendingManual{
		Generation:     gen.Int64,
		AllowDeletes:   int(allow.Int64),
		BypassDebounce: bypass.Int64 == 1,
	}, nil
}

// readPendingManualTx is the in-transaction read used by claim time so the
// manual override is consumed atomically with run creation (§11.2).
func readPendingManualTx(tx *sql.Tx, profileID string) (*PendingManual, error) {
	var gen sql.NullInt64
	var allow, bypass sql.NullInt64
	err := tx.QueryRow(`SELECT pending_manual_generation,
		COALESCE(pending_manual_allow_deletes, 0),
		COALESCE(pending_manual_bypass_debounce, 0)
		FROM profile_sync_state WHERE profile_id = ?`, profileID).
		Scan(&gen, &allow, &bypass)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &PendingManual{Consumed: true}, nil
		}
		return nil, err
	}
	if !gen.Valid {
		return &PendingManual{Consumed: true}, nil
	}
	return &PendingManual{
		Generation:     gen.Int64,
		AllowDeletes:   int(allow.Int64),
		BypassDebounce: bypass.Int64 == 1,
	}, nil
}
