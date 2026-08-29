package state

import (
	"database/sql"
)

// Public top-level sync states (§11).
const (
	StateInitializing = "initializing"
	StateSyncing      = "syncing"
	StateReady        = "ready"
	StateError        = "error"
)

// WorkerInterruptedCode marks runs orphaned by a worker restart (§17.2).
const WorkerInterruptedCode = "worker_interrupted"

// ProfileSyncState is the authoritative durable reconciliation intent and
// operational state for a profile (§7.2). desired_generation is the sole
// durable source of reconciliation intent; sync_runs rows are execution
// attempts/history, never a second queue authority.
type ProfileSyncState struct {
	ProfileID             string  `json:"profile_id"`
	DesiredGeneration     int64   `json:"desired_generation"`
	LastSuccessGeneration *int64  `json:"last_success_generation"`
	InitializedAt         *string `json:"initialized_at"`
	LastSuccessAt         *string `json:"last_success_at"`
	CurrentRunID          *string `json:"current_run_id"`
	State                 string  `json:"state"`
	Phase                 string  `json:"phase"`
	RetryClassification   *string `json:"retry_classification"`
	ConsecutiveFailures   int     `json:"consecutive_failures"`
	NextRetryAt           *string `json:"next_retry_at"`
	LastProgressAt        *string `json:"last_progress_at"`
	LastErrorCode         *string `json:"last_error_code"`
	LastError             *string `json:"last_error"`
}

// HasDebt reports whether reconciliation debt exists:
// desired_generation > last_success_generation, with NULL last-success
// (never initialized) handled as debt.
func (s *ProfileSyncState) HasDebt() bool {
	return s.DesiredGeneration > 0 &&
		(s.LastSuccessGeneration == nil || s.DesiredGeneration > *s.LastSuccessGeneration)
}

// IsInitialized reports whether at least one reconciliation succeeded.
func (s *ProfileSyncState) IsInitialized() bool {
	return s.InitializedAt != nil
}

func scanSyncState(row interface{ Scan(...any) error }) (*ProfileSyncState, error) {
	var s ProfileSyncState
	var lsg sql.NullInt64
	var ini, lsa, run, rc, next, lastP, errCode, errMsg, phase sql.NullString
	var st string
	if err := row.Scan(&s.ProfileID, &s.DesiredGeneration, &lsg, &ini, &lsa,
		&run, &st, &phase, &rc, &s.ConsecutiveFailures, &next,
		&lastP, &errCode, &errMsg); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.State = st
	s.Phase = phase.String
	if lsg.Valid {
		s.LastSuccessGeneration = &lsg.Int64
	}
	s.InitializedAt = nullStr(ini)
	s.LastSuccessAt = nullStr(lsa)
	s.CurrentRunID = nullStr(run)
	s.RetryClassification = nullStr(rc)
	s.NextRetryAt = nullStr(next)
	s.LastProgressAt = nullStr(lastP)
	s.LastErrorCode = nullStr(errCode)
	s.LastError = nullStr(errMsg)
	return &s, nil
}

const syncStateCols = `profile_id, desired_generation, last_success_generation,
	initialized_at, last_success_at, current_run_id, state, phase,
	retry_classification, consecutive_failures, next_retry_at,
	last_progress_at, last_error_code, last_error`

// GetSyncState returns the sync state for a profile.
func (d *DB) GetSyncState(profileID string) (*ProfileSyncState, error) {
	return scanSyncState(d.QueryRow(`SELECT `+syncStateCols+`
		FROM profile_sync_state WHERE profile_id = ?`, profileID))
}

// EnsureSyncState inserts a default sync-state row if missing.
func (d *DB) EnsureSyncState(profileID string) error {
	_, err := d.Exec(`INSERT OR IGNORE INTO profile_sync_state
		(profile_id, desired_generation, state)
		VALUES (?, 0, ?)`, profileID, StateInitializing)
	return err
}

// RequestReconcile records durable reconciliation intent by advancing
// desired_generation (the sole intent authority) and setting the legacy
// reconcile_requested flag. It also refreshes the stored public state so a
// profile with unsatisfied debt is not momentarily reported `ready` (§11).
func (d *DB) RequestReconcile(profileID string) error {
	if err := d.EnsureSyncState(profileID); err != nil {
		return err
	}
	if _, err := d.Exec(`UPDATE profile_sync_state
		SET desired_generation = desired_generation + 1
		WHERE profile_id = ?`, profileID); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE profile_runtime SET reconcile_requested = 1 WHERE profile_id = ?`, profileID)
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE profile_sync_state SET state = CASE
			WHEN state = ? THEN state -- never clobber an error gate
			WHEN desired_generation > COALESCE(last_success_generation, 0) THEN
				CASE WHEN last_success_generation IS NULL THEN ? ELSE ? END
			ELSE state
		END
		WHERE profile_id = ?`, StateError, StateInitializing, StateSyncing, profileID)
	return err
}

// ReopenSyncGate clears a durable terminal/retry gate so a fresh explicit
// reconciliation becomes eligible (§18.3). last_error is preserved until a
// successful run clears it.
func (d *DB) ReopenSyncGate(profileID string) error {
	_, err := d.Exec(`UPDATE profile_sync_state SET
		retry_classification = NULL,
		next_retry_at = NULL,
		state = CASE
			WHEN desired_generation > COALESCE(last_success_generation, 0) THEN
				CASE WHEN last_success_generation IS NULL THEN ? ELSE ? END
			ELSE ?
		END
		WHERE profile_id = ?`,
		StateInitializing, StateSyncing, StateReady, profileID)
	return err
}

// ClaimResult distinguishes why a claim did or did not take place.
type ClaimResult int

const (
	ClaimOK ClaimResult = iota
	ClaimActiveRun
	ClaimNoDebt
	ClaimGateBlocked
	ClaimProfileInactive
)

// ClaimRun atomically claims the next reconciliation attempt for a profile.
// It verifies lifecycle, debt (unless force), retry gates, and the absence of an
// active run inside a single write transaction, capturing target_generation =
// desired_generation (§24.1, §25). It returns the claimed run and the result.
func (d *DB) ClaimRun(profileID, runID string) (*SyncRun, ClaimResult, error) {
	return d.ClaimRunMode(profileID, runID, false)
}

// ClaimRunMode is ClaimRun with a force flag. When force is true the debt
// requirement is skipped so periodic safety-net reconciliations (hourly
// scheduled, explicit manual) run a full reconciliation even with no known
// debt, matching the pre-existing safety-net semantics. Lifecycle, gate, and
// single-active-run constraints always apply.
func (d *DB) ClaimRunMode(profileID, runID string, force bool) (*SyncRun, ClaimResult, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, ClaimProfileInactive, err
	}
	defer tx.Rollback()

	var tomb, enabled int
	var del sql.NullString
	if err := tx.QueryRow(`SELECT tombstoned, enabled, deletion_requested_at
		FROM profiles WHERE id = ?`, profileID).Scan(&tomb, &enabled, &del); err != nil {
		if err == sql.ErrNoRows {
			return nil, ClaimProfileInactive, nil
		}
		return nil, ClaimProfileInactive, err
	}
	if tomb == 1 {
		return nil, ClaimProfileInactive, nil
	}

	var s ProfileSyncState
	var lsg sql.NullInt64
	var curRun, state, rc, next sql.NullString
	if err := tx.QueryRow(`SELECT desired_generation, last_success_generation,
		current_run_id, state, COALESCE(retry_classification, ''), next_retry_at
		FROM profile_sync_state WHERE profile_id = ?`, profileID).
		Scan(&s.DesiredGeneration, &lsg, &curRun, &state, &rc, &next); err != nil {
		return nil, ClaimProfileInactive, err
	}
	if curRun.Valid {
		// An active run exists. Deletion serializes behind it (§19); report the
		// active-run condition so the worker allows the owned run to finish.
		return nil, ClaimActiveRun, nil
	}
	// Deletion intent and disabled lifecycle have higher priority than
	// reconciliation/retry intent: once no active run remains, no new claim may
	// be created (§19).
	if del.Valid || enabled == 0 {
		return nil, ClaimProfileInactive, nil
	}
	if lsg.Valid {
		s.LastSuccessGeneration = &lsg.Int64
	}
	hasDebt := s.HasDebt()
	if !force && !hasDebt {
		return nil, ClaimNoDebt, nil
	}
	nowT := Now()
	now := nowT.Format(timeFmt)
		if state.String == StateError {
		if rc.String == RetryTerminal {
			return nil, ClaimGateBlocked, nil
		}
		if rc.String == RetryRetryable && next.Valid && now < next.String {
			return nil, ClaimGateBlocked, nil
		}
	}

	target := s.DesiredGeneration
	kind := RunKindFull
	if !lsg.Valid {
		kind = RunKindInitial
	}
	run := &SyncRun{
		ID: runID, ProfileID: profileID, Kind: kind,
		TargetGeneration: target, Status: RunRunning, Phase: PhaseQueued,
		StartedAt: now,
	}
	if _, err := tx.Exec(`INSERT INTO sync_runs (
		id, profile_id, kind, target_generation, status, phase, started_at,
		files_discovered, files_completed, bytes_total, bytes_completed,
		error_code, error_classification, error
	) VALUES (?,?,?,?,?,?,?,0,0,0,0,'','','')`,
		run.ID, run.ProfileID, run.Kind, run.TargetGeneration, run.Status,
		run.Phase, run.StartedAt); err != nil {
		return nil, ClaimProfileInactive, err
	}
	newState := StateInitializing
	if lsg.Valid {
		newState = StateSyncing
	}
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		current_run_id = ?, state = ?, phase = ?, last_progress_at = ?
		WHERE profile_id = ?`, runID, newState, PhaseQueued, now, profileID); err != nil {
		return nil, ClaimProfileInactive, err
	}
	if err := tx.Commit(); err != nil {
		return nil, ClaimProfileInactive, err
	}
	return run, ClaimOK, nil
}

// RunFailure carries a structured failure for commit.
type RunFailure struct {
	Code           string
	Classification string
	Message        string
}

// CommitRunSuccess atomically binds success to a run's captured target and
// advances last_success_generation only through that target (§24.2).
func (d *DB) CommitRunSuccess(profileID, runID string, target int64) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := Now().Format(timeFmt)
	if _, err := tx.Exec(`UPDATE sync_runs SET status = ?, completed_at = ?, phase = ?
		WHERE id = ? AND profile_id = ? AND status = ?`,
		RunSucceeded, now, PhaseFinalizing, runID, profileID, RunRunning); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		last_success_generation = MAX(COALESCE(last_success_generation, 0), ?),
		initialized_at = COALESCE(initialized_at, ?),
		last_success_at = ?,
		current_run_id = NULL,
		last_error_code = NULL,
		last_error = NULL,
		retry_classification = NULL,
		next_retry_at = NULL,
		consecutive_failures = 0,
		phase = NULL,
		state = CASE
			WHEN desired_generation <= MAX(COALESCE(last_success_generation, 0), ?) THEN ?
			ELSE ?
		END
		WHERE profile_id = ?`,
		target, now, now, target, StateReady, StateSyncing, profileID); err != nil {
		return err
	}
	// Backward-compatible runtime markers.
	if _, err := tx.Exec(`UPDATE profile_runtime SET
		last_reconcile_success = ?, reconcile_requested = 0, last_error = NULL
		WHERE profile_id = ?`, now, profileID); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitRunFailure persists a failed attempt and applies the durable retry
// gate (§18.2, §18.3). Retryable failures compute next_retry_at from capped
// exponential backoff; terminal failures close the automatic claim gate.
func (d *DB) CommitRunFailure(profileID, runID string, f RunFailure) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	nowT := Now()
	now := nowT.Format(timeFmt)
	if _, err := tx.Exec(`UPDATE sync_runs SET status = ?, completed_at = ?,
		error_code = ?, error_classification = ?, error = ?
		WHERE id = ? AND profile_id = ? AND status = ?`,
		RunFailed, now, f.Code, f.Classification, f.Message, runID, profileID, RunRunning); err != nil {
		return err
	}

	var nextRetry interface{}
	if f.Classification == RetryRetryable {
		var cur int
		if err := tx.QueryRow(`SELECT consecutive_failures FROM profile_sync_state
			WHERE profile_id = ?`, profileID).Scan(&cur); err != nil {
			return err
		}
		cur++
		delay := RetryBackoff(cur)
		nextRetry = nowT.Add(delay).Format(timeFmt)
		if _, err := tx.Exec(`UPDATE profile_sync_state SET
			consecutive_failures = ?, next_retry_at = ? WHERE profile_id = ?`,
			cur, nextRetry, profileID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		current_run_id = NULL,
		state = ?,
		phase = NULL,
		last_error_code = ?,
		last_error = ?,
		retry_classification = ?,
		next_retry_at = COALESCE(next_retry_at, ?)
		WHERE profile_id = ?`,
		StateError, f.Code, f.Message, f.Classification, nextRetry, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE profile_runtime SET last_error = ? WHERE profile_id = ?`,
		f.Message, profileID); err != nil {
		return err
	}
	return tx.Commit()
}

// OrphanCurrentRun marks inherited running attempts as failed with
// worker_interrupted (immediately retryable, no transport backoff inflation)
// and clears current_run_id while preserving generation debt (§17.2, §17.5).
func (d *DB) OrphanCurrentRun(profileID string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now().Format(timeFmt)
	if _, err := tx.Exec(`UPDATE sync_runs SET status = ?, completed_at = ?,
		error_code = ?, error_classification = ?, error = ?
		WHERE profile_id = ? AND status = ?`,
		RunFailed, now, WorkerInterruptedCode, RetryRetryable,
		"worker interrupted; run orphaned on ownership reacquisition",
		profileID, RunRunning); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		current_run_id = NULL,
		state = CASE
			WHEN desired_generation > COALESCE(last_success_generation, 0) THEN
				CASE WHEN last_success_generation IS NULL THEN ? ELSE ? END
			ELSE ?
		END,
		phase = NULL
		WHERE profile_id = ?`,
		StateInitializing, StateSyncing, StateReady, profileID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateRunPhase records the current phase on the active run.
func (d *DB) UpdateRunPhase(profileID, runID, phase string) error {
	now := Now().Format(timeFmt)
	if _, err := d.Exec(`UPDATE sync_runs SET phase = ?, last_progress_at = ?
		WHERE id = ? AND profile_id = ? AND status = ?`,
		phase, now, runID, profileID, RunRunning); err != nil {
		return err
	}
	return d.touchLastProgress(profileID, now)
}

// UpdateRunFilesDiscovered records the discovered file count for a run.
func (d *DB) UpdateRunFilesDiscovered(profileID, runID string, files int64) error {
	now := Now().Format(timeFmt)
	if _, err := d.Exec(`UPDATE sync_runs SET files_discovered = ?, last_progress_at = ?
		WHERE id = ? AND profile_id = ? AND status = ?`,
		files, now, runID, profileID, RunRunning); err != nil {
		return err
	}
	return d.touchLastProgress(profileID, now)
}

// UpdateRunProgress persists best-effort transfer counters (§10.1).
func (d *DB) UpdateRunProgress(profileID, runID string, filesDone, bytesDone, bytesTotal int64) error {
	now := Now().Format(timeFmt)
	if _, err := d.Exec(`UPDATE sync_runs SET files_completed = ?, bytes_completed = ?,
		bytes_total = ?, last_progress_at = ?
		WHERE id = ? AND profile_id = ? AND status = ?`,
		filesDone, bytesDone, bytesTotal, now, runID, profileID, RunRunning); err != nil {
		return err
	}
	return d.touchLastProgress(profileID, now)
}

func (d *DB) touchLastProgress(profileID, now string) error {
	_, err := d.Exec(`UPDATE profile_sync_state SET last_progress_at = ? WHERE profile_id = ?`,
		now, profileID)
	return err
}
