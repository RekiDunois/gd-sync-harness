package state

import (
	"database/sql"
)

// Run status values.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
)

// Run kinds.
const (
	RunKindInitial     = "initial"
	RunKindFull        = "full"
	RunKindIncremental = "incremental"
)

// Run phase values (internal granularity, §11).
const (
	PhaseQueued      = "queued"
	PhaseScanning    = "scanning"
	PhasePlanning    = "planning"
	PhaseUploading   = "uploading"
	PhaseDownloading = "downloading"
	PhaseDeleting    = "deleting"
	PhaseReconciling = "reconciling"
	PhaseFinalizing  = "finalizing"
)

// SyncRun is one execution attempt of reconciliation (§8).
type SyncRun struct {
	ID                  string  `json:"id"`
	ProfileID           string  `json:"profile_id"`
	Kind                string  `json:"kind"`
	TargetGeneration    int64   `json:"target_generation"`
	Status              string  `json:"status"`
	Phase               string  `json:"phase"`
	StartedAt           string  `json:"started_at"`
	CompletedAt         *string `json:"completed_at,omitempty"`
	FilesDiscovered     int64   `json:"files_discovered"`
	FilesCompleted      int64   `json:"files_completed"`
	BytesTotal          int64   `json:"bytes_total"`
	BytesCompleted      int64   `json:"bytes_completed"`
	LastProgressAt      *string `json:"last_progress_at,omitempty"`
	LastHeartbeatAt     *string `json:"last_heartbeat_at,omitempty"`
	ChecksCompleted     int64   `json:"checks_completed"`
	ChecksTotal         int64   `json:"checks_total"`
	ItemsListed         int64   `json:"items_listed"`
	ErrorsCount         int64   `json:"errors_count"`
	SpeedBytesPerSecond float64 `json:"speed_bytes_per_second"`
	CurrentItem         *string `json:"current_item,omitempty"`
	CurrentItemBytes    int64   `json:"current_item_bytes"`
	CurrentItemSize     int64   `json:"current_item_size"`
	ActiveTransfers     int64   `json:"active_transfers"`
	UploadStartedAt     *string `json:"upload_started_at,omitempty"`
	ErrorCode           string  `json:"error_code"`
	ErrorClassification string  `json:"error_classification"`
	Error               string  `json:"error"`
}

const runCols = `id, profile_id, kind, target_generation, status, phase,
	started_at, completed_at, files_discovered, files_completed,
	bytes_total, bytes_completed, last_progress_at, error_code,
	error_classification, error, last_heartbeat_at, checks_completed,
	checks_total, items_listed, errors_count, speed_bytes_per_second,
	current_item, current_item_bytes, current_item_size, active_transfers, upload_started_at`

func scanRun(row interface{ Scan(...any) error }) (*SyncRun, error) {
	var r SyncRun
	var comp, last, phase, heartbeat, current, uploadStarted sql.NullString
	if err := row.Scan(&r.ID, &r.ProfileID, &r.Kind, &r.TargetGeneration,
		&r.Status, &phase, &r.StartedAt, &comp, &r.FilesDiscovered,
		&r.FilesCompleted, &r.BytesTotal, &r.BytesCompleted, &last,
		&r.ErrorCode, &r.ErrorClassification, &r.Error, &heartbeat,
		&r.ChecksCompleted, &r.ChecksTotal, &r.ItemsListed, &r.ErrorsCount,
		&r.SpeedBytesPerSecond, &current, &r.CurrentItemBytes, &r.CurrentItemSize, &r.ActiveTransfers, &uploadStarted); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Phase = phase.String
	r.CompletedAt = nullStr(comp)
	r.LastProgressAt = nullStr(last)
	r.LastHeartbeatAt = nullStr(heartbeat)
	r.CurrentItem = nullStr(current)
	r.UploadStartedAt = nullStr(uploadStarted)
	return &r, nil
}

func scanRuns(rows *sql.Rows) ([]*SyncRun, error) {
	defer rows.Close()
	var out []*SyncRun
	for rows.Next() {
		var r SyncRun
		var comp, last, phase, heartbeat, current, uploadStarted sql.NullString
		if err := rows.Scan(&r.ID, &r.ProfileID, &r.Kind, &r.TargetGeneration,
			&r.Status, &phase, &r.StartedAt, &comp, &r.FilesDiscovered,
			&r.FilesCompleted, &r.BytesTotal, &r.BytesCompleted, &last,
			&r.ErrorCode, &r.ErrorClassification, &r.Error, &heartbeat,
			&r.ChecksCompleted, &r.ChecksTotal, &r.ItemsListed, &r.ErrorsCount,
			&r.SpeedBytesPerSecond, &current, &r.CurrentItemBytes, &r.CurrentItemSize, &r.ActiveTransfers, &uploadStarted); err != nil {
			return nil, err
		}
		r.Phase = phase.String
		r.CompletedAt = nullStr(comp)
		r.LastProgressAt = nullStr(last)
		r.LastHeartbeatAt = nullStr(heartbeat)
		r.CurrentItem = nullStr(current)
		r.UploadStartedAt = nullStr(uploadStarted)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CreateRun inserts a running attempt for a profile. It must be invoked inside
// the same transaction as the claim boundary so run ownership and the run row
// commit atomically.
func (d *DB) CreateRun(p *SyncRun) error {
	_, err := d.Exec(`INSERT INTO sync_runs (
		id, profile_id, kind, target_generation, status, phase, started_at,
		files_discovered, files_completed, bytes_total, bytes_completed,
		error_code, error_classification, error
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.ProfileID, p.Kind, p.TargetGeneration, p.Status, p.Phase,
		p.StartedAt, p.FilesDiscovered, p.FilesCompleted, p.BytesTotal,
		p.BytesCompleted, p.ErrorCode, p.ErrorClassification, p.Error)
	return err
}

// GetRun returns a single run.
func (d *DB) GetRun(id string) (*SyncRun, error) {
	return scanRun(d.QueryRow(`SELECT `+runCols+` FROM sync_runs WHERE id = ?`, id))
}

// ListRuns returns runs for a profile, newest first.
func (d *DB) ListRuns(profileID string, limit int) ([]*SyncRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.Query(`SELECT `+runCols+` FROM sync_runs
		WHERE profile_id = ? ORDER BY started_at DESC LIMIT ?`, profileID, limit)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// OrphanRuns marks all running runs for a profile as failed with
// worker_interrupted (immediately retryable) (§17.2).
func (d *DB) OrphanRuns(profileID, workerInterruptedCode string) error {
	now := Now().Format(timeFmt)
	_, err := d.Exec(`UPDATE sync_runs SET status = ?, completed_at = ?,
		error_code = ?, error_classification = ?, error = ?
		WHERE profile_id = ? AND status = ?`,
		RunFailed, now, workerInterruptedCode, RetryRetryable,
		"worker interrupted; run orphaned on ownership reacquisition",
		profileID, RunRunning)
	return err
}
