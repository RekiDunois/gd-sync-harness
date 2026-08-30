package state

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

// Prune request states (§14.1).
const (
	PruneStatePreviewed        = "previewed"
	PruneStateApprovalRequired = "approval_required"
	PruneStatePending          = "pending"
	PruneStateRunning          = "running"
	PruneStateRetrying         = "retrying"
	PruneStateCompleted        = "completed"
	PruneStateStale            = "stale"
	PruneStateSuperseded       = "superseded"
	PruneStateFailed           = "failed"
)

// Prune target states (§13.3, §14.5).
const (
	PruneTargetPending = "pending"
	PruneTargetDeleted = "deleted"
	PruneTargetMissing = "missing"
)

// PruneRequest is the durable immutable suppressed-object delete request
// (§13.3).
type PruneRequest struct {
	RequestID        string  `json:"request_id"`
	ProfileID        string  `json:"profile_id"`
	PolicyHash       string  `json:"policy_hash"`
	State            string  `json:"state"`
	CandidateCount   int     `json:"candidate_count"`
	CandidateDigest  string  `json:"candidate_digest"`
	DefaultMaxDelete int     `json:"default_max_delete"`
	AuthorizedLimit  *int    `json:"authorized_limit,omitempty"`
	DeletedCount     int     `json:"deleted_count"`
	MissingCount     int     `json:"missing_count"`
	LastError        *string `json:"last_error,omitempty"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
	CompletedAt      *string `json:"completed_at,omitempty"`
}

// PruneTarget is one managed path in a prune request (§13.3).
type PruneTarget struct {
	RequestID    string  `json:"request_id"`
	RelPath      string  `json:"rel_path"`
	State        string  `json:"state"`
	AttemptCount int     `json:"attempt_count"`
	LastError    *string `json:"last_error,omitempty"`
	UpdatedAt    string  `json:"updated_at"`
}

func scanPruneRequest(row interface{ Scan(...any) error }) (*PruneRequest, error) {
	var r PruneRequest
	var limit sql.NullInt64
	var lastErr, comp sql.NullString
	if err := row.Scan(&r.RequestID, &r.ProfileID, &r.PolicyHash, &r.State,
		&r.CandidateCount, &r.CandidateDigest, &r.DefaultMaxDelete,
		&limit, &r.DeletedCount, &r.MissingCount, &lastErr,
		&r.CreatedAt, &r.UpdatedAt, &comp); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if limit.Valid {
		v := int(limit.Int64)
		r.AuthorizedLimit = &v
	}
	r.LastError = nullStr(lastErr)
	r.CompletedAt = nullStr(comp)
	return &r, nil
}

const pruneRequestCols = `request_id, profile_id, policy_hash, state, candidate_count,
	candidate_digest, default_max_delete, authorized_limit, deleted_count, missing_count,
	last_error, created_at, updated_at, completed_at`

// CreatePrunePreview creates a durable prune request from the currently
// suppressed ledger rows for the committed refreshed policy_hash (§13.3). It
// refuses when the policy refresh is not ready, supersedes older unexecuted
// previews, and never supersedes pending/running/retrying requests.
func (d *DB) CreatePrunePreview(profileID, requestID string) (*PruneRequest, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	pol, err := getCommittedPolicyTx(tx, profileID)
	if err != nil {
		return nil, err
	}
	if pol.RefreshState != PolicyRefreshReady || pol.RefreshedPolicyHash == nil || *pol.RefreshedPolicyHash != pol.PolicyHash {
		return nil, fmt.Errorf("policy refresh pending; prune preview not ready")
	}
	hash := pol.PolicyHash

	// Freeze the execution set from the current suppressed ledger rows for the
	// refreshed policy hash (§13.3).
	rows, err := tx.Query(`SELECT rel_path FROM manifest
		WHERE profile_id = ? AND state = ? AND suppressed_policy_hash = ? ORDER BY rel_path`,
		profileID, ManifestSuppressed, hash)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		paths = append(paths, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	digest := digestPaths(paths)
	now := Now().Format(timeFmt)
	var maxDelete int
	if err := tx.QueryRow(`SELECT max_delete FROM profiles WHERE id = ?`, profileID).Scan(&maxDelete); err != nil {
		return nil, err
	}

	// Supersede older unexecuted previews/approval-required requests.
	if _, err := tx.Exec(`UPDATE prune_requests SET state = ?, updated_at = ?
		WHERE profile_id = ? AND state IN (?, ?)`,
		PruneStateSuperseded, now, profileID, PruneStatePreviewed, PruneStateApprovalRequired); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(`INSERT INTO prune_requests
		(request_id, profile_id, policy_hash, state, candidate_count, candidate_digest,
		 default_max_delete, deleted_count, missing_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		requestID, profileID, hash, PruneStatePreviewed, len(paths), digest, maxDelete, now, now); err != nil {
		return nil, err
	}
	for _, p := range paths {
		if _, err := tx.Exec(`INSERT INTO prune_targets (request_id, rel_path, state, updated_at)
			VALUES (?, ?, ?, ?)`, requestID, p, PruneTargetPending, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &PruneRequest{
		RequestID: requestID, ProfileID: profileID, PolicyHash: hash, State: PruneStatePreviewed,
		CandidateCount: len(paths), CandidateDigest: digest, DefaultMaxDelete: maxDelete,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func getCommittedPolicyTx(tx *sql.Tx, profileID string) (*CommittedPolicy, error) {
	return scanCommittedPolicy(tx.QueryRow(`SELECT `+committedPolicyCols+`
		FROM profile_ignore_policy WHERE profile_id = ?`, profileID))
}

// GetPruneRequest returns a prune request by ID.
func (d *DB) GetPruneRequest(requestID string) (*PruneRequest, error) {
	return scanPruneRequest(d.QueryRow(`SELECT `+pruneRequestCols+`
		FROM prune_requests WHERE request_id = ?`, requestID))
}

// GetActivePruneRequest returns the most recent non-terminal prune request for
// a profile (order: created_at desc). Returns nil when none exists.
func (d *DB) GetActivePruneRequest(profileID string) (*PruneRequest, error) {
	row, err := scanPruneRequest(d.QueryRow(`SELECT `+pruneRequestCols+`
		FROM prune_requests WHERE profile_id = ? AND state NOT IN (?, ?, ?)
		ORDER BY created_at DESC LIMIT 1`,
		profileID, PruneStateCompleted, PruneStateStale, PruneStateSuperseded))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return row, nil
}

// PruneTargets returns the target rows for a request ordered by rel_path.
func (d *DB) PruneTargets(requestID string) ([]PruneTarget, error) {
	rows, err := d.Query(`SELECT request_id, rel_path, state, attempt_count, last_error, updated_at
		FROM prune_targets WHERE request_id = ? ORDER BY rel_path`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PruneTarget
	for rows.Next() {
		var t PruneTarget
		var lastErr sql.NullString
		if err := rows.Scan(&t.RequestID, &t.RelPath, &t.State, &t.AttemptCount, &lastErr, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.LastError = nullStr(lastErr)
		out = append(out, t)
	}
	return out, rows.Err()
}

// AuthorizePrune persists the accepted authorization limit on an
// approval_required/previewed request before queueing it so worker retries
// retain the same authorization (§14.2). candidate_count > limit deletes zero
// objects: the request returns to approval_required and never queues.
func (d *DB) AuthorizePrune(requestID string, allowDeletes int) (*PruneRequest, error) {
	req, err := d.GetPruneRequest(requestID)
	if err != nil {
		return nil, err
	}
	effective := allowDeletes
	if effective <= 0 {
		effective = req.DefaultMaxDelete
	}
	if req.CandidateCount > effective {
		// Insufficient ceiling: keep approval_required, delete zero.
		return req, fmt.Errorf("prune candidate_count %d exceeds effective limit %d; delete zero objects (approval remains required)", req.CandidateCount, effective)
	}
	now := Now().Format(timeFmt)
	if _, err := d.Exec(`UPDATE prune_requests SET
		authorized_limit = ?, state = ?, updated_at = ?
		WHERE request_id = ?`, effective, PruneStatePending, now, requestID); err != nil {
		return nil, err
	}
	req.AuthorizedLimit = &effective
	req.State = PruneStatePending
	req.UpdatedAt = now
	return req, nil
}

// ClaimPrune atomically claims a pending prune request for worker execution.
// It returns ClaimNoDebt when no eligible pending request exists.
func (d *DB) ClaimPrune(profileID string) (*PruneRequest, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	req, err := scanPruneRequest(tx.QueryRow(`SELECT `+pruneRequestCols+`
		FROM prune_requests WHERE profile_id = ? AND state = ? ORDER BY created_at LIMIT 1`,
		profileID, PruneStatePending))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	// Verify policy freshness under the same transaction (§14.3).
	pol, err := getCommittedPolicyTx(tx, profileID)
	if err != nil {
		return nil, err
	}
	if pol.PolicyHash != req.PolicyHash || pol.RefreshState != PolicyRefreshReady ||
		pol.RefreshedPolicyHash == nil || *pol.RefreshedPolicyHash != pol.PolicyHash {
		now := Now().Format(timeFmt)
		if _, err := tx.Exec(`UPDATE prune_requests SET state = ?, updated_at = ?
			WHERE request_id = ?`, PruneStateStale, now, req.RequestID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		req.State = PruneStateStale
		return req, nil
	}
	now := Now().Format(timeFmt)
	if _, err := tx.Exec(`UPDATE prune_requests SET state = ?, updated_at = ?
		WHERE request_id = ?`, PruneStateRunning, now, req.RequestID); err != nil {
		return nil, err
	}
	// Reset pending targets to pending with attempt count preserved.
	if _, err := tx.Exec(`UPDATE prune_targets SET state = ?
		WHERE request_id = ? AND state = ?`, PruneTargetPending, req.RequestID, PruneTargetMissing); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	req.State = PruneStateRunning
	req.UpdatedAt = now
	return req, nil
}

// MarkPruneTargetResult records one target's outcome and updates request
// counters.
func (d *DB) MarkPruneTargetResult(requestID, relPath string, result string, errMsg string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now().Format(timeFmt)
	var prev string
	if err := tx.QueryRow(`SELECT state FROM prune_targets WHERE request_id = ? AND rel_path = ?`,
		requestID, relPath).Scan(&prev); err != nil {
		return err
	}
	var lastErr interface{}
	if errMsg != "" {
		lastErr = errMsg
	}
	attempts := 1
	var cur int
	if err := tx.QueryRow(`SELECT attempt_count FROM prune_targets WHERE request_id = ? AND rel_path = ?`,
		requestID, relPath).Scan(&cur); err != nil {
		return err
	}
	if cur > 0 {
		attempts = cur + 1
	}
	if _, err := tx.Exec(`UPDATE prune_targets SET state = ?, attempt_count = ?, last_error = ?, updated_at = ?
		WHERE request_id = ? AND rel_path = ?`, result, attempts, lastErr, now, requestID, relPath); err != nil {
		return err
	}
	// Update request counters.
	var delta string
	switch result {
	case PruneTargetDeleted:
		delta = "deleted_count = deleted_count + 1"
	case PruneTargetMissing:
		delta = "missing_count = missing_count + 1"
	}
	if delta != "" {
		if _, err := tx.Exec(`UPDATE prune_requests SET `+delta+`, updated_at = ? WHERE request_id = ?`, now, requestID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CommitPruneComplete finalizes a completed prune request: removes the
// corresponding suppressed ledger rows, marks the request completed with
// summary counts, and deletes per-target rows to avoid unbounded growth
// (§14.7).
func (d *DB) CommitPruneComplete(requestID string) error {
	req, err := d.GetPruneRequest(requestID)
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	targets, err := pruneTargetsTx(tx, requestID)
	if err != nil {
		return err
	}
	now := Now().Format(timeFmt)
	for _, t := range targets {
		if t.State == PruneTargetDeleted {
			if _, err := tx.Exec(`DELETE FROM manifest WHERE profile_id = ? AND rel_path = ? AND state = ?`,
				req.ProfileID, t.RelPath, ManifestSuppressed); err != nil {
				return err
			}
		}
	}
	// The deleted/missing counters were already updated by
	// MarkPruneTargetResult; do not overwrite them with the stale request
	// object's values.
	if _, err := tx.Exec(`UPDATE prune_requests SET state = ?, completed_at = ?, updated_at = ?
		WHERE request_id = ?`,
		PruneStateCompleted, now, now, requestID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM prune_targets WHERE request_id = ?`, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func pruneTargetsTx(tx *sql.Tx, requestID string) ([]PruneTarget, error) {
	rows, err := tx.Query(`SELECT request_id, rel_path, state, attempt_count, last_error, updated_at
		FROM prune_targets WHERE request_id = ?`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PruneTarget
	for rows.Next() {
		var t PruneTarget
		var lastErr sql.NullString
		if err := rows.Scan(&t.RequestID, &t.RelPath, &t.State, &t.AttemptCount, &lastErr, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.LastError = nullStr(lastErr)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClaimRetryingPrune finds an approved request in running/retrying state left
// by a dead worker and returns it for ownership reacquisition. It returns nil
// when none exists. Policy staleness is checked by the caller under the profile
// lock.
func (d *DB) ClaimRetryingPrune(profileID string) (*PruneRequest, error) {
	row, err := scanPruneRequest(d.QueryRow(`SELECT `+pruneRequestCols+`
		FROM prune_requests WHERE profile_id = ? AND state IN (?, ?)
		ORDER BY created_at LIMIT 1`, profileID, PruneStateRunning, PruneStateRetrying))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	now := Now().Format(timeFmt)
	if _, err := d.Exec(`UPDATE prune_requests SET state = ?, updated_at = ?
		WHERE request_id = ?`, PruneStateRunning, now, row.RequestID); err != nil {
		return nil, err
	}
	row.State = PruneStateRunning
	row.UpdatedAt = now
	return row, nil
}

// SetPruneRetrying marks a request retrying and records the last error.
func (d *DB) SetPruneRetrying(requestID string, errMsg string) error {
	now := Now().Format(timeFmt)
	_, err := d.Exec(`UPDATE prune_requests SET state = ?, last_error = ?, updated_at = ?
		WHERE request_id = ?`, PruneStateRetrying, errMsg, now, requestID)
	return err
}

// SetPruneFailed marks a request failed while retaining target rows for
// diagnosis/resume (§14.6).
func (d *DB) SetPruneFailed(requestID string, errMsg string) error {
	now := Now().Format(timeFmt)
	_, err := d.Exec(`UPDATE prune_requests SET state = ?, last_error = ?, updated_at = ?
		WHERE request_id = ?`, PruneStateFailed, errMsg, now, requestID)
	return err
}

func digestPaths(paths []string) string {
	h := sha256.New()
	for _, p := range paths {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
