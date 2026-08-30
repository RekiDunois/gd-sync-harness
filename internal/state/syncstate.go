package state

import (
	"database/sql"
	"time"
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
	LastHeartbeatAt       *string `json:"last_heartbeat_at"`
	ReconcileNotBeforeAt  *string `json:"reconcile_not_before_at"`
	ReconcileDeadlineAt   *string `json:"reconcile_deadline_at"`
	LimitedFailures       int     `json:"limited_failures"`
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
	var ini, lsa, run, rc, next, lastP, heartbeat, notBefore, deadline, errCode, errMsg, phase sql.NullString
	var st string
	if err := row.Scan(&s.ProfileID, &s.DesiredGeneration, &lsg, &ini, &lsa,
		&run, &st, &phase, &rc, &s.ConsecutiveFailures, &next,
		&lastP, &errCode, &errMsg, &heartbeat, &notBefore, &deadline,
		&s.LimitedFailures); err != nil {
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
	s.LastHeartbeatAt = nullStr(heartbeat)
	s.ReconcileNotBeforeAt = nullStr(notBefore)
	s.ReconcileDeadlineAt = nullStr(deadline)
	s.LastErrorCode = nullStr(errCode)
	s.LastError = nullStr(errMsg)
	return &s, nil
}

const syncStateCols = `profile_id, desired_generation, last_success_generation,
	initialized_at, last_success_at, current_run_id, state, phase,
	retry_classification, consecutive_failures, next_retry_at,
	last_progress_at, last_error_code, last_error, last_heartbeat_at,
	reconcile_not_before_at, reconcile_deadline_at, limited_failures`

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
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A manual request is one durable opportunity, not an unconditional
	// increment for every duplicate wake-up.
	if _, err := tx.Exec(`UPDATE profile_sync_state SET desired_generation = MAX(
		desired_generation,
		COALESCE((SELECT source_generation FROM profile_runtime WHERE profile_id = ?), 0),
		COALESCE(last_success_generation, 0) + 1
	) WHERE profile_id = ?`, profileID, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE profile_runtime SET reconcile_requested = 1 WHERE profile_id = ?`, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE profile_sync_state SET state = CASE
			WHEN state = ? THEN state -- never clobber an error gate
			WHEN desired_generation > COALESCE(last_success_generation, 0) THEN
				CASE WHEN last_success_generation IS NULL THEN ? ELSE ? END
			ELSE state
		END
		WHERE profile_id = ?`, StateError, StateInitializing, StateSyncing, profileID); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsureReconcileGeneration coalesces full-reconcile intent at a filesystem
// generation. Repeating the same generation is idempotent.
func (d *DB) EnsureReconcileGeneration(profileID string, generation int64) error {
	if generation < 1 {
		generation = 1
	}
	_, err := d.Exec(`UPDATE profile_sync_state SET
		desired_generation = MAX(desired_generation, ?),
		state = CASE
			WHEN state = ? THEN state
			WHEN last_success_generation IS NULL THEN ?
			ELSE ?
		END
		WHERE profile_id = ?`, generation, StateError, StateInitializing, StateSyncing, profileID)
	return err
}

// ScheduleDestructiveReconcile persists a full intent and its debounce window.
func (d *DB) ScheduleDestructiveReconcile(profileID string, generation int64, now time.Time) error {
	if err := d.EnsureReconcileGeneration(profileID, generation); err != nil {
		return err
	}
	notBefore := now.Add(10 * time.Second).Format(timeFmt)
	deadline := now.Add(60 * time.Second).Format(timeFmt)
	_, err := d.Exec(`UPDATE profile_sync_state SET
		reconcile_not_before_at = CASE WHEN reconcile_not_before_at IS NULL OR reconcile_not_before_at < ? THEN ? ELSE reconcile_not_before_at END,
		reconcile_deadline_at = CASE WHEN reconcile_deadline_at IS NULL THEN ? ELSE reconcile_deadline_at END
		WHERE profile_id = ?`, notBefore, notBefore, deadline, profileID)
	return err
}

// PromoteToFullReconcile atomically records the latest full intent, collapses
// detailed pending paths, and starts the durable destructive debounce window.
func (d *DB) PromoteToFullReconcile(profileID string, generation int64, now time.Time) error {
	if generation < 1 {
		generation = 1
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	notBefore := now.Add(10 * time.Second).Format(timeFmt)
	deadline := now.Add(60 * time.Second).Format(timeFmt)
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		desired_generation = MAX(desired_generation, ?),
		reconcile_not_before_at = CASE WHEN reconcile_not_before_at IS NULL OR reconcile_not_before_at < ? THEN ? ELSE reconcile_not_before_at END,
		reconcile_deadline_at = CASE WHEN reconcile_deadline_at IS NULL THEN ? ELSE reconcile_deadline_at END,
		state = CASE WHEN state = ? THEN state WHEN last_success_generation IS NULL THEN ? ELSE ? END
		WHERE profile_id = ?`, generation, notBefore, notBefore, deadline, StateError, StateInitializing, StateSyncing, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE profile_runtime SET reconcile_requested = 1 WHERE profile_id = ?`, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM pending_events WHERE profile_id = ?`, profileID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReopenSyncGate clears a durable terminal/retry gate so a fresh explicit
// reconciliation becomes eligible (§18.3). last_error is preserved until a
// successful run clears it.
func (d *DB) ReopenSyncGate(profileID string) error {
	_, err := d.Exec(`UPDATE profile_sync_state SET
		retry_classification = NULL,
		next_retry_at = NULL,
		limited_failures = 0,
		reconcile_not_before_at = NULL,
		reconcile_deadline_at = NULL,
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
	ClaimDeferred
)

// ClaimRun atomically claims the next reconciliation attempt for a profile.
// It verifies lifecycle, debt, retry gates, and the absence of an active run
// inside a single write transaction, capturing target_generation =
// desired_generation (§24.1, §25). It atomically consumes any pending manual
// one-shot options into the new run's audit fields (§11.2). It returns the
// claimed run and the result.
func (d *DB) ClaimRun(profileID, runID string) (*SyncRun, ClaimResult, error) {
	return d.claimRun(profileID, runID)
}

func (d *DB) claimRun(profileID, runID string) (*SyncRun, ClaimResult, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, ClaimProfileInactive, err
	}
	defer tx.Rollback()

	var tomb, enabled int
	var del sql.NullString
	var maxDelete int
	if err := tx.QueryRow(`SELECT tombstoned, enabled, deletion_requested_at, max_delete
		FROM profiles WHERE id = ?`, profileID).Scan(&tomb, &enabled, &del, &maxDelete); err != nil {
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
	var curRun, state, rc, next, notBefore, deadline sql.NullString
	if err := tx.QueryRow(`SELECT desired_generation, last_success_generation,
		current_run_id, state, COALESCE(retry_classification, ''), next_retry_at,
		reconcile_not_before_at, reconcile_deadline_at
		FROM profile_sync_state WHERE profile_id = ?`, profileID).
		Scan(&s.DesiredGeneration, &lsg, &curRun, &state, &rc, &next, &notBefore, &deadline); err != nil {
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
	if !hasDebt {
		return nil, ClaimNoDebt, nil
	}
	nowT := Now()
	now := nowT.Format(timeFmt)
	bypassDebounce := false
	if state.String == StateError {
		if rc.String == RetryTerminal {
			return nil, ClaimGateBlocked, nil
		}
		if (rc.String == RetryRetryable || rc.String == RetryRetryableLimited) && next.Valid && now < next.String {
			return nil, ClaimGateBlocked, nil
		}
	}

	target := s.DesiredGeneration
	kind := RunKindFull
	if !lsg.Valid {
		kind = RunKindInitial
	}
	// Compute the effective delete limit from any unconsumed one-attempt manual
	// override, falling back to the profile's persistent budget (§11.2). The
	// override is consumed in the same transaction that creates the run row.
	effectiveDelete := 0
	var manualOverride interface{}
	pending, err := readPendingManualTx(tx, profileID)
	if err != nil {
		return nil, ClaimProfileInactive, err
	}
	if pending != nil && pending.Generation == target {
		effectiveDelete = pending.AllowDeletes
		if pending.AllowDeletes > 0 {
			manualOverride = pending.AllowDeletes
		}
		bypassDebounce = bypassDebounce || pending.BypassDebounce
	}
	// The durable destructive debounce window applies unless a manual request
	// bypassed it. Non-manual (filesystem/scheduled) intent always respects it.
	if !bypassDebounce && notBefore.Valid && now < notBefore.String {
		return nil, ClaimDeferred, nil
	}
	if effectiveDelete == 0 {
		effectiveDelete = maxDelete
	}
	run := &SyncRun{
		ID: runID, ProfileID: profileID, Kind: kind,
		TargetGeneration: target, Status: RunRunning, Phase: PhaseQueued,
		StartedAt: now, EffectiveMaxDelete: effectiveDelete,
	}
	if manualOverride != nil {
		if v, ok := manualOverride.(int); ok {
			mv := v
			run.ManualDeleteOverride = &mv
		}
	}
	if _, err := tx.Exec(`INSERT INTO sync_runs (
		id, profile_id, kind, target_generation, status, phase, started_at,
		files_discovered, files_completed, bytes_total, bytes_completed,
		error_code, error_classification, error,
		effective_max_delete, manual_delete_override
	) VALUES (?,?,?,?,?,?,?,0,0,0,0,'','','',?,?)`,
		run.ID, run.ProfileID, run.Kind, run.TargetGeneration, run.Status,
		run.Phase, run.StartedAt, effectiveDelete, manualOverride); err != nil {
		return nil, ClaimProfileInactive, err
	}
	// Consume the one-shot manual override: a failed attempt does not
	// automatically grant the same override to a retry (§11.3).
	if pending != nil && pending.Generation == target {
		if _, err := tx.Exec(`UPDATE profile_sync_state SET
			pending_manual_generation = NULL,
			pending_manual_allow_deletes = NULL,
			pending_manual_bypass_debounce = 0
			WHERE profile_id = ?`, profileID); err != nil {
			return nil, ClaimProfileInactive, err
		}
	}
	newState := StateInitializing
	if lsg.Valid {
		newState = StateSyncing
	}
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		current_run_id = ?, state = ?, phase = ?, last_progress_at = ?, last_heartbeat_at = ?,
		reconcile_not_before_at = NULL, reconcile_deadline_at = NULL
		WHERE profile_id = ?`, runID, newState, PhaseQueued, now, now, profileID); err != nil {
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
		limited_failures = 0,
		phase = NULL,
		state = CASE
			WHEN desired_generation <= MAX(COALESCE(last_success_generation, 0), ?) THEN ?
			ELSE ?
		END
		WHERE profile_id = ?`,
		target, now, now, target, StateReady, StateSyncing, profileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM pending_events WHERE profile_id = ? AND source_generation <= ?`, profileID, target); err != nil {
		return err
	}
	// Backward-compatible runtime markers.
	if _, err := tx.Exec(`UPDATE profile_runtime SET
		last_reconcile_success = ?, reconcile_requested = CASE
			WHEN (SELECT desired_generation FROM profile_sync_state WHERE profile_id = ?) > ? THEN 1 ELSE 0 END,
		last_error = NULL WHERE profile_id = ?`, now, profileID, target, profileID); err != nil {
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
	var nextRetry interface{}
	if f.Classification == RetryRetryable && f.Code != WorkerInterruptedCode {
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
	if f.Classification == RetryRetryableLimited {
		var cur int
		if err := tx.QueryRow(`SELECT limited_failures FROM profile_sync_state WHERE profile_id = ?`, profileID).Scan(&cur); err != nil {
			return err
		}
		cur++
		if cur > 3 {
			f.Classification = RetryTerminal
			f.Code = "unknown_error_limit"
		} else {
			nextRetry = nowT.Add(LimitedRetryBackoff(cur)).Format(timeFmt)
			if _, err := tx.Exec(`UPDATE profile_sync_state SET limited_failures = ?, next_retry_at = ? WHERE profile_id = ?`, cur, nextRetry, profileID); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE sync_runs SET status = ?, completed_at = ?,
		error_code = ?, error_classification = ?, error = ?
		WHERE id = ? AND profile_id = ? AND status = ?`,
		RunFailed, now, f.Code, f.Classification, f.Message, runID, profileID, RunRunning); err != nil {
		return err
	}
	if f.Code == WorkerInterruptedCode {
		nextRetry = now
	}

	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		current_run_id = NULL,
		state = ?,
		phase = NULL,
		last_error_code = ?,
		last_error = ?,
		retry_classification = ?,
		next_retry_at = ?,
		last_heartbeat_at = ?
		WHERE profile_id = ?`,
		StateError, f.Code, f.Message, f.Classification, nextRetry, now, profileID); err != nil {
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
	if _, err := d.Exec(`UPDATE sync_runs SET phase = ?, upload_started_at = CASE WHEN ? = ? THEN COALESCE(upload_started_at, ?) ELSE upload_started_at END
		WHERE id = ? AND profile_id = ? AND status = ?`,
		phase, phase, PhaseUploading, now, runID, profileID, RunRunning); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE profile_sync_state SET phase = ? WHERE profile_id = ?`, phase, profileID)
	return err
}

// UpdateRunFilesDiscovered records the discovered file count for a run.
func (d *DB) UpdateRunFilesDiscovered(profileID, runID string, files int64) error {
	now := Now().Format(timeFmt)
	if _, err := d.Exec(`UPDATE sync_runs SET files_discovered = ?, last_progress_at = ?
		WHERE id = ? AND profile_id = ? AND status = ?`,
		files, now, runID, profileID, RunRunning); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE profile_sync_state SET last_progress_at = ?, last_heartbeat_at = ? WHERE profile_id = ?`, now, now, profileID)
	return err
}

// UpdateRunHeartbeat records telemetry without treating it as measurable work.
func (d *DB) UpdateRunHeartbeat(profileID, runID string) error {
	now := Now().Format(timeFmt)
	if _, err := d.Exec(`UPDATE sync_runs SET last_heartbeat_at = ?
		WHERE id = ? AND profile_id = ? AND status = ?`,
		now, runID, profileID, RunRunning); err != nil {
		return err
	}
	_, err := d.Exec(`UPDATE profile_sync_state SET last_heartbeat_at = ? WHERE profile_id = ?`, now, profileID)
	return err
}

// ProgressSnapshot keeps the state package independent from rclone's parser.
type ProgressSnapshot struct {
	FilesCompleted, BytesCompleted, BytesTotal             int64
	ChecksCompleted, ChecksTotal, ItemsListed, ErrorsCount int64
	SpeedBytesPerSecond                                    float64
	CurrentItem                                            *string
	CurrentItemBytes, CurrentItemSize                      int64
	ActiveTransfers                                        int64
}

// UpdateRunStats persists structured telemetry. last_progress_at changes only
// when measurable is true; every valid frame updates last_heartbeat_at.
func (d *DB) UpdateRunStats(profileID, runID string, s ProgressSnapshot, measurable bool) error {
	now := Now().Format(timeFmt)
	var progress interface{}
	if measurable {
		progress = now
	}
	if _, err := d.Exec(`UPDATE sync_runs SET files_completed = ?, bytes_completed = ?, bytes_total = ?,
		last_heartbeat_at = ?, checks_completed = ?, checks_total = ?, items_listed = ?, errors_count = ?,
		speed_bytes_per_second = ?, current_item = ?, current_item_bytes = ?, current_item_size = ?, active_transfers = ?,
		last_progress_at = COALESCE(?, last_progress_at)
		WHERE id = ? AND profile_id = ? AND status = ?`,
		s.FilesCompleted, s.BytesCompleted, s.BytesTotal, now, s.ChecksCompleted, s.ChecksTotal,
		s.ItemsListed, s.ErrorsCount, s.SpeedBytesPerSecond, s.CurrentItem, s.CurrentItemBytes,
		s.CurrentItemSize, s.ActiveTransfers, progress, runID, profileID, RunRunning); err != nil {
		return err
	}
	if measurable {
		_, err := d.Exec(`UPDATE profile_sync_state SET last_heartbeat_at = ?, last_progress_at = ? WHERE profile_id = ?`, now, now, profileID)
		return err
	}
	_, err := d.Exec(`UPDATE profile_sync_state SET last_heartbeat_at = ? WHERE profile_id = ?`, now, profileID)
	return err
}
