package state

import (
	"database/sql"
	"time"
)

// Event kinds persisted in pending_events.
const (
	EventCreate = "create"
	EventModify = "modify"
	EventDelete = "delete"
	EventRename = "rename"
	EventOther  = "other"
)

// IsSafeFastKind reports whether an event kind is safe for targeted fast
// processing (create/modify). Destructive or uncertain kinds (delete, rename,
// other, or unknown) must fail closed to a full reconcile (§6.1).
func IsSafeFastKind(kind string) bool {
	return kind == EventCreate || kind == EventModify
}

// PendingEvent is one queued file change.
type PendingEvent struct {
	ID               int64  `json:"id"`
	ProfileID        string `json:"profile_id"`
	Path             string `json:"path"`
	EventKind        string `json:"event_kind"`
	FirstSeen        string `json:"first_seen"`
	LastSeen         string `json:"last_seen"`
	SourceGeneration int64  `json:"source_generation"`
	// Policy context recorded durably with the event for delete-vs-suppress
	// classification (§9.2).
	ObservedPolicyHash *string `json:"observed_policy_hash,omitempty"`
	ObservedPolicyGen  *int64  `json:"observed_policy_generation,omitempty"`
	PolicyContextKnown bool    `json:"policy_context_known"`
}

// UpsertPendingEvent inserts or bumps last_seen for an existing event.
func (d *DB) UpsertPendingEvent(profileID, path, kind string, generation int64) error {
	now := Now().Format(timeFmt)
	_, err := d.Exec(`INSERT INTO pending_events (profile_id, path, event_kind, first_seen, last_seen, source_generation)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (profile_id, path) DO UPDATE SET
			event_kind = excluded.event_kind,
			last_seen = excluded.last_seen,
			source_generation = MAX(pending_events.source_generation, excluded.source_generation)`,
		profileID, path, kind, now, now, generation)
	return err
}

// UpsertDeleteEvent upserts an event keeping it a delete kind.
func (d *DB) UpsertDeleteEvent(profileID, path string, generation int64) error {
	return d.UpsertPendingEvent(profileID, path, EventDelete, generation)
}

// RecordEvent atomically advances the source generation and records either a
// detailed fast event or a collapsed full-reconcile intent. This closes the
// crash window between bumping a generation and persisting its consequence.
// The current committed policy hash/generation is captured with the event so
// delete-vs-suppress classification has durable ordering evidence (§9.2).
func (d *DB) RecordEvent(profileID, path, kind string, full bool) (int64, error) {
	return d.recordEvent(profileID, path, kind, full, "")
}

// RecordEventWithPolicy is RecordEvent with an explicit committed policy hash
// captured at call time (the caller may already hold the committed policy).
func (d *DB) RecordEventWithPolicy(profileID, path, kind string, full bool, policyHash string) (int64, error) {
	return d.recordEvent(profileID, path, kind, full, policyHash)
}

func (d *DB) recordEvent(profileID, path, kind string, full bool, policyHash string) (int64, error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE profile_runtime SET source_generation = source_generation + 1 WHERE profile_id = ?`, profileID); err != nil {
		return 0, err
	}
	var generation int64
	if err := tx.QueryRow(`SELECT source_generation FROM profile_runtime WHERE profile_id = ?`, profileID).Scan(&generation); err != nil {
		return 0, err
	}
	var desired int64
	var last sql.NullInt64
	var current sql.NullString
	if err := tx.QueryRow(`SELECT desired_generation, last_success_generation, current_run_id FROM profile_sync_state WHERE profile_id = ?`, profileID).
		Scan(&desired, &last, &current); err != nil {
		return 0, err
	}
	// Capture the current committed policy context if the caller did not
	// supply one explicitly.
	observedHash := policyHash
	observedGen := sql.NullInt64{}
	policyKnown := false
	if observedHash == "" {
		if err := tx.QueryRow(`SELECT policy_hash, committed_generation FROM profile_ignore_policy WHERE profile_id = ?`, profileID).
			Scan(&observedHash, &observedGen); err == nil {
			policyKnown = true
		}
	} else {
		if err := tx.QueryRow(`SELECT committed_generation FROM profile_ignore_policy WHERE profile_id = ?`, profileID).
			Scan(&observedGen); err == nil {
			policyKnown = true
		}
	}
	var hashArg interface{}
	var genArg interface{}
	if policyKnown {
		hashArg = observedHash
		genArg = observedGen.Int64
	}
	knownArg := 0
	if policyKnown {
		knownArg = 1
	}
	// Event-kind aware decision. Safe create/modify events stay in the fast
	// lane: they advance source_generation and keep durable path evidence but
	// never promote to full-reconcile debt merely because a run is active or
	// older full debt exists. Destructive/uncertain events (and the bootstrap
	// fallback) fail closed to a full reconcile, collapsing prior pending
	// evidence (§6 of the ignored-churn fix plan).
	safe := IsSafeFastKind(kind)
	bootstrapNeedsFull := !last.Valid && desired == 0 && !current.Valid
	if full || !safe || bootstrapNeedsFull {
		now := Now()
		notBefore := now.Add(10 * time.Second).Format(timeFmt)
		deadline := now.Add(60 * time.Second).Format(timeFmt)
		if _, err := tx.Exec(`UPDATE profile_sync_state SET desired_generation = MAX(desired_generation, ?),
			reconcile_not_before_at = CASE WHEN reconcile_not_before_at IS NULL OR reconcile_not_before_at < ? THEN ? ELSE reconcile_not_before_at END,
			reconcile_deadline_at = CASE WHEN reconcile_deadline_at IS NULL THEN ? ELSE reconcile_deadline_at END,
			state = CASE WHEN state = ? THEN state WHEN last_success_generation IS NULL THEN ? ELSE ? END
			WHERE profile_id = ?`, generation, notBefore, notBefore, deadline, StateError, StateInitializing, StateSyncing, profileID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE profile_runtime SET reconcile_requested = 1 WHERE profile_id = ?`, profileID); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM pending_events WHERE profile_id = ?`, profileID); err != nil {
			return 0, err
		}
	} else {
		now := Now().Format(timeFmt)
		if _, err := tx.Exec(`INSERT INTO pending_events (profile_id, path, event_kind, first_seen, last_seen, source_generation,
			observed_policy_hash, observed_policy_generation, policy_context_known)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (profile_id, path) DO UPDATE SET event_kind = excluded.event_kind,
			last_seen = excluded.last_seen, source_generation = MAX(pending_events.source_generation, excluded.source_generation),
			observed_policy_hash = excluded.observed_policy_hash,
			observed_policy_generation = excluded.observed_policy_generation,
			policy_context_known = excluded.policy_context_known`,
			profileID, path, kind, now, now, generation, hashArg, genArg, knownArg); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return generation, nil
}

// ListPending returns all pending events for a profile ordered by first_seen.
func (d *DB) ListPending(profileID string) ([]PendingEvent, error) {
	rows, err := d.Query(`SELECT id, profile_id, path, event_kind, first_seen, last_seen, source_generation,
		observed_policy_hash, observed_policy_generation, policy_context_known
		FROM pending_events WHERE profile_id = ? ORDER BY first_seen`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingEvent
	for rows.Next() {
		var e PendingEvent
		var hash sql.NullString
		var gen sql.NullInt64
		var known int
		if err := rows.Scan(&e.ID, &e.ProfileID, &e.Path, &e.EventKind, &e.FirstSeen, &e.LastSeen,
			&e.SourceGeneration, &hash, &gen, &known); err != nil {
			return nil, err
		}
		e.ObservedPolicyHash = nullStr(hash)
		if gen.Valid {
			v := gen.Int64
			e.ObservedPolicyGen = &v
		}
		e.PolicyContextKnown = known == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountPending returns the number of pending events for a profile.
func (d *DB) CountPending(profileID string) (int, error) {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM pending_events WHERE profile_id = ?`, profileID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// HasDestructivePending reports whether any delete/rename/uncertain events are
// pending for a profile.
func (d *DB) HasDestructivePending(profileID string) (bool, error) {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM pending_events
		WHERE profile_id = ? AND event_kind IN (?, ?, ?)`,
		profileID, EventDelete, EventRename, EventOther).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ClearPending removes all pending events for a profile.
func (d *DB) ClearPending(profileID string) error {
	_, err := d.Exec(`DELETE FROM pending_events WHERE profile_id = ?`, profileID)
	return err
}

// ClearPendingPaths removes specific pending paths.
func (d *DB) ClearPendingPaths(profileID string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range paths {
		if _, err := tx.Exec(`DELETE FROM pending_events WHERE profile_id = ? AND path = ?`, profileID, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearPendingEvents removes only the exact event versions included in a fast
// batch. A newer event for the same path survives the clear.
func (d *DB) ClearPendingEvents(profileID string, events []PendingEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if _, err := tx.Exec(`DELETE FROM pending_events
			WHERE profile_id = ? AND path = ? AND source_generation <= ?`,
			profileID, e.Path, e.SourceGeneration); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearPendingThroughGeneration is used after full success. Events observed
// while the run was active have a larger generation and remain durable.
func (d *DB) ClearPendingThroughGeneration(profileID string, generation int64) error {
	_, err := d.Exec(`DELETE FROM pending_events WHERE profile_id = ? AND source_generation <= ?`, profileID, generation)
	return err
}
