package state

// Event kinds persisted in pending_events.
const (
	EventCreate = "create"
	EventModify = "modify"
	EventDelete = "delete"
	EventRename = "rename"
	EventOther  = "other"
)

// PendingEvent is one queued file change.
type PendingEvent struct {
	ID               int64  `json:"id"`
	ProfileID        string `json:"profile_id"`
	Path             string `json:"path"`
	EventKind        string `json:"event_kind"`
	FirstSeen        string `json:"first_seen"`
	LastSeen         string `json:"last_seen"`
	SourceGeneration int64  `json:"source_generation"`
}

// UpsertPendingEvent inserts or bumps last_seen for an existing event.
func (d *DB) UpsertPendingEvent(profileID, path, kind string, generation int64) error {
	now := Now().Format(timeFmt)
	_, err := d.Exec(`INSERT INTO pending_events (profile_id, path, event_kind, first_seen, last_seen, source_generation)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (profile_id, path) DO UPDATE SET
			event_kind = excluded.event_kind,
			last_seen = excluded.last_seen,
			source_generation = excluded.source_generation`,
		profileID, path, kind, now, now, generation)
	return err
}

// UpsertDeleteEvent upserts an event keeping it a delete kind.
func (d *DB) UpsertDeleteEvent(profileID, path string, generation int64) error {
	return d.UpsertPendingEvent(profileID, path, EventDelete, generation)
}

// ListPending returns all pending events for a profile ordered by first_seen.
func (d *DB) ListPending(profileID string) ([]PendingEvent, error) {
	rows, err := d.Query(`SELECT id, profile_id, path, event_kind, first_seen, last_seen, source_generation
		FROM pending_events WHERE profile_id = ? ORDER BY first_seen`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingEvent
	for rows.Next() {
		var e PendingEvent
		if err := rows.Scan(&e.ID, &e.ProfileID, &e.Path, &e.EventKind, &e.FirstSeen, &e.LastSeen, &e.SourceGeneration); err != nil {
			return nil, err
		}
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
