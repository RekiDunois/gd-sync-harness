package state

// ManifestEntry is one row in the local manifest, keyed by profile + rel_path.
type ManifestEntry struct {
	ProfileID string `json:"profile_id"`
	RelPath   string `json:"rel_path"`
	Size      int64  `json:"size"`
	ModTime   int64  `json:"mod_time"` // unix seconds
	Hash      string `json:"hash"`
}

// ManifestUpsert inserts or updates a manifest row.
func (d *DB) ManifestUpsert(e ManifestEntry) error {
	_, err := d.Exec(`INSERT INTO manifest (profile_id, rel_path, size, mod_time, hash)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (profile_id, rel_path) DO UPDATE SET
			size = excluded.size, mod_time = excluded.mod_time, hash = excluded.hash`,
		e.ProfileID, e.RelPath, e.Size, e.ModTime, e.Hash)
	return err
}

// ManifestGet returns a single manifest entry, ErrNotFound if absent.
func (d *DB) ManifestGet(profileID, relPath string) (*ManifestEntry, error) {
	var e ManifestEntry
	err := d.QueryRow(`SELECT profile_id, rel_path, size, mod_time, hash
		FROM manifest WHERE profile_id = ? AND rel_path = ?`, profileID, relPath).
		Scan(&e.ProfileID, &e.RelPath, &e.Size, &e.ModTime, &e.Hash)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// ManifestCount returns number of manifest rows for a profile.
func (d *DB) ManifestCount(profileID string) (int, error) {
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM manifest WHERE profile_id = ?`, profileID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ManifestDelete removes a manifest row.
func (d *DB) ManifestDelete(profileID, relPath string) error {
	_, err := d.Exec(`DELETE FROM manifest WHERE profile_id = ? AND rel_path = ?`, profileID, relPath)
	return err
}

// ManifestReplaceAll atomically replaces the manifest for a profile.
func (d *DB) ManifestReplaceAll(profileID string, entries []ManifestEntry) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM manifest WHERE profile_id = ?`, profileID); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO manifest (profile_id, rel_path, size, mod_time, hash)
			VALUES (?, ?, ?, ?, ?)`, e.ProfileID, e.RelPath, e.Size, e.ModTime, e.Hash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ManifestAll returns all manifest entries for a profile.
func (d *DB) ManifestAll(profileID string) ([]ManifestEntry, error) {
	rows, err := d.Query(`SELECT profile_id, rel_path, size, mod_time, hash
		FROM manifest WHERE profile_id = ? ORDER BY rel_path`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManifestEntry
	for rows.Next() {
		var e ManifestEntry
		if err := rows.Scan(&e.ProfileID, &e.RelPath, &e.Size, &e.ModTime, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
