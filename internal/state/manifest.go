package state

import (
	"database/sql"
	"strings"
)

// Managed ledger states (§10.1).
const (
	ManifestActive     = "active"
	ManifestSuppressed = "suppressed"
)

// ManifestEntry is one row in the managed mirror ledger, keyed by profile +
// rel_path. Existing manifest rows migrate as active (§18.2).
type ManifestEntry struct {
	ProfileID string `json:"profile_id"`
	RelPath   string `json:"rel_path"`
	Size      int64  `json:"size"`
	ModTime   int64  `json:"mod_time"` // unix seconds
	Hash      string `json:"hash"`
	// State is the managed ledger state: active | suppressed (§10.1).
	State string `json:"state"`
	// SuppressedPolicyHash is the committed policy hash that suppressed the
	// object, when State == suppressed.
	SuppressedPolicyHash *string `json:"suppressed_policy_hash,omitempty"`
	// SuppressedGeneration is the unified generation at suppression time.
	SuppressedGeneration *int64 `json:"suppressed_generation,omitempty"`
}

const manifestCols = `profile_id, rel_path, size, mod_time, hash, state,
	suppressed_policy_hash, suppressed_generation`

func scanManifestEntry(row interface{ Scan(...any) error }) (*ManifestEntry, error) {
	var e ManifestEntry
	var sp sql.NullString
	var sg sql.NullInt64
	if err := row.Scan(&e.ProfileID, &e.RelPath, &e.Size, &e.ModTime, &e.Hash,
		&e.State, &sp, &sg); err != nil {
		return nil, err
	}
	e.SuppressedPolicyHash = nullStr(sp)
	if sg.Valid {
		v := sg.Int64
		e.SuppressedGeneration = &v
	}
	return &e, nil
}

// ManifestUpsert inserts or updates a manifest row, preserving the managed
// state when the row already exists.
func (d *DB) ManifestUpsert(e ManifestEntry) error {
	state := e.State
	if state == "" {
		state = ManifestActive
	}
	_, err := d.Exec(`INSERT INTO manifest (profile_id, rel_path, size, mod_time, hash, state, suppressed_policy_hash, suppressed_generation)
		VALUES (?, ?, ?, ?, ?, ?, NULL, NULL)
		ON CONFLICT (profile_id, rel_path) DO UPDATE SET
			size = excluded.size, mod_time = excluded.mod_time, hash = excluded.hash,
			state = CASE WHEN manifest.state IS NULL OR manifest.state = '' THEN excluded.state ELSE manifest.state END,
			suppressed_policy_hash = CASE WHEN manifest.state = ? THEN manifest.suppressed_policy_hash ELSE NULL END,
			suppressed_generation = CASE WHEN manifest.state = ? THEN manifest.suppressed_generation ELSE NULL END`,
		e.ProfileID, e.RelPath, e.Size, e.ModTime, e.Hash, state, ManifestSuppressed, ManifestSuppressed)
	return err
}

// ManifestGet returns a single manifest entry, ErrNotFound if absent.
func (d *DB) ManifestGet(profileID, relPath string) (*ManifestEntry, error) {
	e, err := scanManifestEntry(d.QueryRow(`SELECT `+manifestCols+`
		FROM manifest WHERE profile_id = ? AND rel_path = ?`, profileID, relPath))
	if err != nil {
		return nil, err
	}
	return e, nil
}

// ManifestCount returns number of manifest rows for a profile.
func (d *DB) ManifestCount(profileID string) (int, error) {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM manifest WHERE profile_id = ?`, profileID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ManifestCounts returns the active and suppressed counts separately.
func (d *DB) ManifestCounts(profileID string) (active int, suppressed int, err error) {
	if err := d.QueryRow(`SELECT COUNT(*) FROM manifest WHERE profile_id = ? AND state = ?`, profileID, ManifestActive).Scan(&active); err != nil {
		return 0, 0, err
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM manifest WHERE profile_id = ? AND state = ?`, profileID, ManifestSuppressed).Scan(&suppressed); err != nil {
		return 0, 0, err
	}
	return active, suppressed, nil
}

// ManifestSuppressedCount returns the suppressed ledger count.
func (d *DB) ManifestSuppressedCount(profileID string) (int, error) {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM manifest WHERE profile_id = ? AND state = ?`, profileID, ManifestSuppressed).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ManifestDelete removes a manifest row.
func (d *DB) ManifestDelete(profileID, relPath string) error {
	_, err := d.Exec(`DELETE FROM manifest WHERE profile_id = ? AND rel_path = ?`, profileID, relPath)
	return err
}

// ManifestMarkSuppressed transitions an active row to suppressed for a policy
// hash and generation (§10.2).
func (d *DB) ManifestMarkSuppressed(profileID, relPath, policyHash string, generation int64) error {
	_, err := d.Exec(`UPDATE manifest SET state = ?, suppressed_policy_hash = ?, suppressed_generation = ?
		WHERE profile_id = ? AND rel_path = ?`, ManifestSuppressed, policyHash, generation, profileID, relPath)
	return err
}

// ManifestReactivate transitions a suppressed row back to active, clearing
// suppression provenance (§10.2).
func (d *DB) ManifestReactivate(profileID, relPath string) error {
	_, err := d.Exec(`UPDATE manifest SET state = ?, suppressed_policy_hash = NULL, suppressed_generation = NULL
		WHERE profile_id = ? AND rel_path = ?`, ManifestActive, profileID, relPath)
	return err
}

// ManifestApply is a state-aware apply used by policy/full refresh (§10.3). It
// never blanket-replaces suppressed rows. Desired active entries become active;
// a suppressed row that is no longer in the desired set stays suppressed;
// active rows absent from the desired set are removed (ordinary stale deletion).
func (d *DB) ManifestApply(profileID string, entries []ManifestEntry) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		seen[e.RelPath] = true
		if _, err := tx.Exec(`INSERT INTO manifest (profile_id, rel_path, size, mod_time, hash, state, suppressed_policy_hash, suppressed_generation)
			VALUES (?, ?, ?, ?, ?, 'active', NULL, NULL)
			ON CONFLICT (profile_id, rel_path) DO UPDATE SET
				size = excluded.size, mod_time = excluded.mod_time, hash = excluded.hash,
				state = CASE WHEN manifest.state = 'suppressed' THEN manifest.state ELSE excluded.state END,
				suppressed_policy_hash = CASE WHEN manifest.state = 'suppressed' THEN manifest.suppressed_policy_hash ELSE NULL END,
				suppressed_generation = CASE WHEN manifest.state = 'suppressed' THEN manifest.suppressed_generation ELSE NULL END`,
			e.ProfileID, e.RelPath, e.Size, e.ModTime, e.Hash); err != nil {
			return err
		}
	}
	// Remove active rows absent from the desired set; suppressed rows survive.
	if len(entries) > 0 {
		placeholders := make([]string, 0, len(entries))
		args := make([]any, 0, len(entries)+2)
		args = append(args, profileID, ManifestActive)
		for _, e := range entries {
			placeholders = append(placeholders, "?")
			args = append(args, e.RelPath)
		}
		q := `DELETE FROM manifest WHERE profile_id = ? AND state = ? AND rel_path NOT IN (` +
			strings.Join(placeholders, ",") + `)`
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
	} else {
		// No desired entries: all active rows are stale; suppressed survive.
		if _, err := tx.Exec(`DELETE FROM manifest WHERE profile_id = ? AND state = ?`, profileID, ManifestActive); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ManifestAll returns all manifest entries for a profile.
func (d *DB) ManifestAll(profileID string) ([]ManifestEntry, error) {
	rows, err := d.Query(`SELECT `+manifestCols+`
		FROM manifest WHERE profile_id = ? ORDER BY rel_path`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManifestEntry
	for rows.Next() {
		e, err := scanManifestEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ManifestAllState returns only the entries for a given ledger state.
func (d *DB) ManifestAllState(profileID, state string) ([]ManifestEntry, error) {
	rows, err := d.Query(`SELECT `+manifestCols+`
		FROM manifest WHERE profile_id = ? AND state = ? ORDER BY rel_path`, profileID, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManifestEntry
	for rows.Next() {
		e, err := scanManifestEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}
