package state

import (
	"database/sql"
	"errors"
	"fmt"

	"knowledge-sync/internal/policy"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrIDExists      = errors.New("profile id already exists")
	ErrIDTombstoned  = errors.New("profile id is tombstoned; use forget or restore")
	ErrNotTombstoned = errors.New("profile is not tombstoned")
)

// Profile is the canonical profile configuration record.
type Profile struct {
	ID                  string   `json:"id"`
	ProfileUUID         string   `json:"profile_uuid"`
	Type                string   `json:"type"` // "obsidian" | "generic"
	SourcePath          string   `json:"source_path"`
	RemoteName          string   `json:"remote_name"`
	RemoteFolderID      string   `json:"remote_folder_id"`
	RemoteDisplayPath   string   `json:"remote_display_path"`
	Enabled             bool     `json:"enabled"`
	MaxDelete           int      `json:"max_delete"`
	MaxFileSize         int64    `json:"max_file_size"` // bytes; 0 = unlimited
	DeletedAt           *string  `json:"deleted_at,omitempty"`
	Tombstoned          bool     `json:"tombstoned"`
	DeletionRequestedAt *string  `json:"deletion_requested_at,omitempty"`
	Excludes            []string `json:"excludes"`
}

// scanProfile scans a full profile row (excludes loaded separately).
func scanProfile(row interface{ Scan(...any) error }) (*Profile, error) {
	var p Profile
	var enabled, tomb int
	var deleted, delReq sql.NullString
	if err := row.Scan(
		&p.ID, &p.ProfileUUID, &p.Type, &p.SourcePath, &p.RemoteName,
		&p.RemoteFolderID, &p.RemoteDisplayPath, &enabled, &p.MaxDelete,
		&p.MaxFileSize, &deleted, &tomb, &delReq,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.Enabled = enabled == 1
	p.Tombstoned = tomb == 1
	if deleted.Valid {
		p.DeletedAt = &deleted.String
	}
	if delReq.Valid {
		p.DeletionRequestedAt = &delReq.String
	}
	return &p, nil
}

const profileCols = `id, profile_uuid, type, source_path, remote_name,
	remote_folder_id, remote_display_path, enabled, max_delete, max_file_size,
	deleted_at, tombstoned, deletion_requested_at`

// GetProfile returns a profile by ID (tombstoned or not).
func (d *DB) GetProfile(id string) (*Profile, error) {
	p, err := d.getProfileRow(id)
	if err != nil {
		return nil, err
	}
	excludes, err := d.GetExcludes(id)
	if err != nil {
		return nil, err
	}
	p.Excludes = excludes
	return p, nil
}

func (d *DB) getProfileRow(id string) (*Profile, error) {
	return scanProfile(d.QueryRow(`SELECT `+profileCols+` FROM profiles WHERE id = ?`, id))
}

// ListProfiles returns all profiles sorted by id.
func (d *DB) ListProfiles() ([]*Profile, error) {
	rows, err := d.Query(`SELECT ` + profileCols + ` FROM profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Profile
	for rows.Next() {
		var p Profile
		var enabled, tomb int
		var deleted, delReq sql.NullString
		if err := rows.Scan(
			&p.ID, &p.ProfileUUID, &p.Type, &p.SourcePath, &p.RemoteName,
			&p.RemoteFolderID, &p.RemoteDisplayPath, &enabled, &p.MaxDelete,
			&p.MaxFileSize, &deleted, &tomb, &delReq,
		); err != nil {
			return nil, err
		}
		p.Enabled = enabled == 1
		p.Tombstoned = tomb == 1
		if deleted.Valid {
			p.DeletedAt = &deleted.String
		}
		if delReq.Valid {
			p.DeletionRequestedAt = &delReq.String
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ActiveProfiles returns non-tombstoned profiles.
func (d *DB) ActiveProfiles() ([]*Profile, error) {
	all, err := d.ListProfiles()
	if err != nil {
		return nil, err
	}
	var out []*Profile
	for _, p := range all {
		if !p.Tombstoned {
			out = append(out, p)
		}
	}
	return out, nil
}

// CreateProfile inserts a new profile, rejecting duplicate and tombstoned IDs.
func (d *DB) CreateProfile(p *Profile) error {
	return d.CreateProfileWithPolicy(p, &policy.Snapshot{})
}

// CreateProfileWithPolicy atomically inserts profile/runtime/sync state and the
// initial committed policy bundle.
func (d *DB) CreateProfileWithPolicy(p *Profile, snap *policy.Snapshot) error {
	if snap == nil {
		snap = &policy.Snapshot{}
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM profiles WHERE id = ?`, p.ID).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		var tomb int
		if err := tx.QueryRow(`SELECT tombstoned FROM profiles WHERE id = ?`, p.ID).Scan(&tomb); err != nil {
			return err
		}
		if tomb == 1 {
			return ErrIDTombstoned
		}
		return ErrIDExists
	}

	now := Now().Format(timeFmt)
	enabled := boolInt(p.Enabled)
	tomb := boolInt(p.Tombstoned)
	var deleted interface{}
	if p.DeletedAt != nil {
		deleted = *p.DeletedAt
	}
	if _, err := tx.Exec(`INSERT INTO profiles (
		id, profile_uuid, type, source_path, remote_name, remote_folder_id,
		remote_display_path, enabled, max_delete, max_file_size, deleted_at,
		tombstoned, created_at, updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.ProfileUUID, p.Type, p.SourcePath, p.RemoteName,
		p.RemoteFolderID, p.RemoteDisplayPath, enabled, p.MaxDelete,
		p.MaxFileSize, deleted, tomb, now, now,
	); err != nil {
		return err
	}
	// Generation one is the durable initial-sync epoch. Subsequent filesystem
	// events therefore always advance beyond the initial target generation.
	if _, err := tx.Exec(`INSERT INTO profile_runtime (profile_id, source_generation) VALUES (?, 1)`, p.ID); err != nil {
		return err
	}
	// A new profile always records durable reconciliation intent: the initial
	// full reconciliation is the only way to establish initialization evidence
	// (§24.1). The worker claims this debt; no queued sync_run row is required.
	if _, err := tx.Exec(`INSERT INTO profile_sync_state
		(profile_id, desired_generation, state)
		VALUES (?, 1, ?)`, p.ID, StateInitializing); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO compiler_profile_state (profile_id) VALUES (?)`, p.ID); err != nil {
		return err
	}
	hash := snap.Hash()
	if _, err := tx.Exec(`INSERT INTO profile_ignore_policy
		(profile_id, policy_source, policy_hash, committed_generation, committed_at, refresh_state, matcher_warning_count)
		VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		p.ID, PolicySourceGitignore, hash, 1, now, len(snap.Warnings)); err != nil {
		return err
	}
	for i, f := range snap.Files {
		if _, err := tx.Exec(`INSERT INTO profile_ignore_snapshot_files
			(profile_id, policy_hash, relative_path, scope_dir, content, content_order)
			VALUES (?, ?, ?, ?, ?, ?)`, p.ID, hash, f.RelativePath, f.ScopeDir, f.Content, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateProfileFields updates mutable config fields on an existing profile.
func (d *DB) UpdateProfileFields(p *Profile) error {
	_, err := d.Exec(`UPDATE profiles SET
		type = ?, source_path = ?, remote_name = ?, remote_folder_id = ?,
		remote_display_path = ?, enabled = ?, max_delete = ?, max_file_size = ?,
		updated_at = ?
		WHERE id = ?`,
		p.Type, p.SourcePath, p.RemoteName, p.RemoteFolderID,
		p.RemoteDisplayPath, boolInt(p.Enabled), p.MaxDelete, p.MaxFileSize,
		Now().Format(timeFmt), p.ID,
	)
	return err
}

// SetProfileEnabled flips the enabled flag.
func (d *DB) SetProfileEnabled(id string, enabled bool) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE profiles SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolInt(enabled), Now().Format(timeFmt), id)
	if err != nil {
		return err
	}
	if err := checkRows(res); err != nil {
		return err
	}
	if enabled {
		_, err = tx.Exec(`UPDATE compiler_profile_state SET derived_state = 'pending'
			WHERE profile_id = ? AND derived_state = 'blocked_disabled'`, id)
	} else {
		_, err = tx.Exec(`UPDATE compiler_profile_state SET derived_state = 'blocked_disabled'
			WHERE profile_id = ?`, id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// TombstoneProfile marks a profile deleted (soft delete), clearing enabled.
func (d *DB) TombstoneProfile(id string) error {
	now := Now().Format(timeFmt)
	res, err := d.Exec(`UPDATE profiles SET enabled = 0, deleted_at = ?, tombstoned = 1, updated_at = ? WHERE id = ?`,
		now, now, id)
	if err != nil {
		return err
	}
	return checkRows(res)
}

// RestoreProfile clears tombstone state on a deleted profile.
func (d *DB) RestoreProfile(id string) error {
	_, err := d.Exec(`UPDATE profiles SET tombstoned = 0, deleted_at = NULL, updated_at = ? WHERE id = ?`,
		Now().Format(timeFmt), id)
	return err
}

// RequestProfileDeletion durably records deletion intent and disables the
// profile. After this point no new reconciliation/retry claims may be created
// for the profile (§19). Final row removal is deferred until no active run
// remains.
func (d *DB) RequestProfileDeletion(id string) error {
	now := Now().Format(timeFmt)
	res, err := d.Exec(`UPDATE profiles SET
		enabled = 0,
		deletion_requested_at = ?,
		updated_at = ?
		WHERE id = ?`, now, now, id)
	if err != nil {
		return err
	}
	return checkRows(res)
}

// CancelProfileDeletion clears a pending deletion request (used to make a
// deletion request durable only, never by sync retry paths).
func (d *DB) CancelProfileDeletion(id string) error {
	_, err := d.Exec(`UPDATE profiles SET deletion_requested_at = NULL, updated_at = ? WHERE id = ?`,
		Now().Format(timeFmt), id)
	return err
}

// DeletingProfiles returns non-tombstoned profiles with a pending deletion
// request, ordered by deletion_requested_at.
func (d *DB) DeletingProfiles() ([]*Profile, error) {
	rows, err := d.Query(`SELECT ` + profileCols + ` FROM profiles
		WHERE tombstoned = 0 AND deletion_requested_at IS NOT NULL
		ORDER BY deletion_requested_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ForgetProfile permanently deletes the tombstone row.
func (d *DB) ForgetProfile(id string) error {
	p, err := d.getProfileRow(id)
	if err != nil {
		return err
	}
	if !p.Tombstoned {
		return ErrNotTombstoned
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM profile_excludes WHERE profile_id = ?`,
		`DELETE FROM pending_events WHERE profile_id = ?`,
		`DELETE FROM profile_runtime WHERE profile_id = ?`,
		`DELETE FROM profile_sync_state WHERE profile_id = ?`,
		`DELETE FROM sync_runs WHERE profile_id = ?`,
		`DELETE FROM manifest WHERE profile_id = ?`,
		`DELETE FROM profiles WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func checkRows(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ValidateID implements the recommended ID grammar [a-z0-9][a-z0-9-]*.
func ValidateID(id string) error {
	if len(id) == 0 {
		return errors.New("profile id must not be empty")
	}
	for i, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0)
		if !ok {
			return fmt.Errorf("invalid profile id %q: must match [a-z0-9][a-z0-9-]*", id)
		}
	}
	return nil
}
