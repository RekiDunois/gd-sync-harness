package state

// RuntimeStatus values for profile_runtime.watcher_status.
const (
	WatcherStopped = "stopped"
	WatcherRunning = "running"
	WatcherError   = "error"
)

// Runtime is the mutable per-profile runtime state.
type Runtime struct {
	ProfileID            string  `json:"profile_id"`
	SourceGeneration     int64   `json:"source_generation"`
	ReconcileRequested   bool    `json:"reconcile_requested"`
	LastFastSuccess      *string `json:"last_fast_success,omitempty"`
	LastReconcileSuccess *string `json:"last_reconcile_success,omitempty"`
	LastError            *string `json:"last_error,omitempty"`
	WatcherStatus        string  `json:"watcher_status"`
}

type sqlNullString struct {
	Valid bool
	Str   string
}

func (s *sqlNullString) Scan(v any) error {
	if v == nil {
		s.Valid = false
		s.Str = ""
		return nil
	}
	switch t := v.(type) {
	case []byte:
		s.Valid = true
		s.Str = string(t)
	case string:
		s.Valid = true
		s.Str = t
	default:
		s.Valid = false
	}
	return nil
}

func nullStr(s sqlNullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.Str
}

func scanRuntime(row interface{ Scan(...any) error }) (*Runtime, error) {
	var r Runtime
	var rec int
	var fs, rs, le, ws sqlNullString
	if err := row.Scan(&r.ProfileID, &r.SourceGeneration, &rec, &fs, &rs, &le, &ws); err != nil {
		return nil, err
	}
	r.ReconcileRequested = rec == 1
	r.LastFastSuccess = nullStr(fs)
	r.LastReconcileSuccess = nullStr(rs)
	r.LastError = nullStr(le)
	r.WatcherStatus = ws.Str
	return &r, nil
}

// GetRuntime returns runtime state for a profile.
func (d *DB) GetRuntime(profileID string) (*Runtime, error) {
	return scanRuntime(d.QueryRow(`SELECT profile_id, source_generation, reconcile_requested,
		last_fast_success, last_reconcile_success, last_error, watcher_status
		FROM profile_runtime WHERE profile_id = ?`, profileID))
}

// EnsureRuntime creates a runtime row if missing.
func (d *DB) EnsureRuntime(profileID string) error {
	_, err := d.Exec(`INSERT OR IGNORE INTO profile_runtime (profile_id) VALUES (?)`, profileID)
	return err
}

// SetWatcherStatus updates watcher_status.
func (d *DB) SetWatcherStatus(profileID, status string) error {
	_, err := d.Exec(`UPDATE profile_runtime SET watcher_status = ? WHERE profile_id = ?`, status, profileID)
	return err
}

// BumpGeneration increments source_generation.
func (d *DB) BumpGeneration(profileID string) (int64, error) {
	_, err := d.Exec(`UPDATE profile_runtime SET source_generation = source_generation + 1 WHERE profile_id = ?`, profileID)
	if err != nil {
		return 0, err
	}
	r, err := d.GetRuntime(profileID)
	if err != nil {
		return 0, err
	}
	return r.SourceGeneration, nil
}

// RequestReconcile sets reconcile_requested=1.
func (d *DB) RequestReconcile(profileID string) error {
	_, err := d.Exec(`UPDATE profile_runtime SET reconcile_requested = 1 WHERE profile_id = ?`, profileID)
	return err
}

// ClearReconcile resets reconcile_requested=0.
func (d *DB) ClearReconcile(profileID string) error {
	_, err := d.Exec(`UPDATE profile_runtime SET reconcile_requested = 0 WHERE profile_id = ?`, profileID)
	return err
}

// MarkFastSuccess records a fast-sync success timestamp.
func (d *DB) MarkFastSuccess(profileID string) error {
	_, err := d.Exec(`UPDATE profile_runtime SET last_fast_success = ?, last_error = NULL WHERE profile_id = ?`,
		Now().Format(timeFmt), profileID)
	return err
}

// MarkReconcileSuccess records a reconciliation success timestamp.
func (d *DB) MarkReconcileSuccess(profileID string) error {
	_, err := d.Exec(`UPDATE profile_runtime SET last_reconcile_success = ?, reconcile_requested = 0, last_error = NULL WHERE profile_id = ?`,
		Now().Format(timeFmt), profileID)
	return err
}

// SetLastError records the latest error.
func (d *DB) SetLastError(profileID, errMsg string) error {
	_, err := d.Exec(`UPDATE profile_runtime SET last_error = ? WHERE profile_id = ?`, errMsg, profileID)
	return err
}
