package state

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const (
	CompilerRunRunning        = "running"
	CompilerRunSucceeded      = "succeeded"
	CompilerRunFailed         = "failed"
	CompilerRunInterrupted    = "interrupted"
	CompilerLocalGeneration   = "generation"
	CompilerLocalAbsent       = "absent"
	CompilerDesiredGeneration = "generation"
	CompilerDesiredAbsent     = "absent"
)

// DerivedBindingFingerprint is the stable identity of a remote derived root.
func DerivedBindingFingerprint(remoteName, folderID string) string {
	remoteName = strings.TrimSuffix(remoteName, ":")
	sum := sha256.Sum256([]byte("derived-binding-v1\x00" + remoteName + "\x00" + folderID))
	return hex.EncodeToString(sum[:])
}

type CompilerRun struct {
	ID                      string  `json:"id"`
	ProfileID               string  `json:"profile_id"`
	CandidateGenerationID   *string `json:"candidate_generation_id,omitempty"`
	StartedAt               string  `json:"started_at"`
	CompletedAt             *string `json:"completed_at,omitempty"`
	Status                  string  `json:"status"`
	CompilerVersion         string  `json:"compiler_version"`
	SchemaVersion           int     `json:"schema_version"`
	SourceSnapshotID        *string `json:"source_snapshot_id,omitempty"`
	PolicyHash              *string `json:"policy_hash,omitempty"`
	EligibilityContractHash *string `json:"eligibility_contract_hash,omitempty"`
	FileCount               int     `json:"file_count"`
	WarningCount            int     `json:"warning_count"`
	Error                   string  `json:"error"`
}

type CompilerProfileState struct {
	ProfileID                         string  `json:"profile_id"`
	LastSuccessGenerationID           *string `json:"last_success_generation_id,omitempty"`
	LastSuccessAt                     *string `json:"last_success_at,omitempty"`
	LastSourceSnapshotID              *string `json:"last_source_snapshot_id,omitempty"`
	LastPolicyHash                    *string `json:"last_policy_hash,omitempty"`
	LastEligibilityContractHash       *string `json:"last_eligibility_contract_hash,omitempty"`
	LocalMode                         string  `json:"local_mode"`
	LocalCleanState                   string  `json:"local_clean_state"`
	LocalCleanOperationID             *string `json:"local_clean_operation_id,omitempty"`
	DesiredDerivedMode                string  `json:"desired_derived_mode"`
	DesiredDerivedGenerationID        *string `json:"desired_derived_generation_id,omitempty"`
	DesiredDerivedRevision            int64   `json:"desired_derived_revision"`
	CurrentRemoteBindingFingerprint   *string `json:"current_remote_binding_fingerprint,omitempty"`
	RemotePublishedGenerationID       *string `json:"remote_published_generation_id,omitempty"`
	RemotePublishedBindingFingerprint *string `json:"remote_published_binding_fingerprint,omitempty"`
	RemoteState                       string  `json:"remote_state"`
	RemoteStateBindingFingerprint     *string `json:"remote_state_binding_fingerprint,omitempty"`
	DerivedState                      string  `json:"derived_state"`
	ActivePublishGenerationID         *string `json:"active_publish_generation_id,omitempty"`
	DerivedRetryTargetKey             *string `json:"derived_retry_target_key,omitempty"`
	DerivedRetryClassification        *string `json:"derived_retry_classification,omitempty"`
	DerivedConsecutiveFailures        int     `json:"derived_consecutive_failures"`
	DerivedLimitedFailures            int     `json:"derived_limited_failures"`
	DerivedNextRetryAt                *string `json:"derived_next_retry_at,omitempty"`
	DerivedTerminalErrorCode          *string `json:"derived_terminal_error_code,omitempty"`
	LastDerivedError                  *string `json:"last_derived_error,omitempty"`
	LastDerivedSuccessAt              *string `json:"last_derived_success_at,omitempty"`
}

const compilerStateCols = `profile_id, last_success_generation_id, last_success_at,
	last_source_snapshot_id, last_policy_hash, last_eligibility_contract_hash,
	local_mode, local_clean_state, local_clean_operation_id, desired_derived_mode,
	desired_derived_generation_id, desired_derived_revision,
	current_remote_binding_fingerprint, remote_published_generation_id,
	remote_published_binding_fingerprint, remote_state, remote_state_binding_fingerprint,
	derived_state, active_publish_generation_id, last_derived_error, last_derived_success_at,
	derived_retry_target_key, derived_retry_classification, derived_consecutive_failures,
	derived_limited_failures, derived_next_retry_at, derived_terminal_error_code`

func scanCompilerState(row interface{ Scan(...any) error }) (*CompilerProfileState, error) {
	var state CompilerProfileState
	var values [18]sql.NullString
	if err := row.Scan(&state.ProfileID, &values[0], &values[1], &values[2], &values[3], &values[4],
		&state.LocalMode, &state.LocalCleanState, &values[5], &state.DesiredDerivedMode,
		&values[6], &state.DesiredDerivedRevision, &values[7], &values[8], &values[9],
		&state.RemoteState, &values[10], &state.DerivedState, &values[11], &values[12], &values[13],
		&values[14], &values[15], &state.DerivedConsecutiveFailures, &state.DerivedLimitedFailures,
		&values[16], &values[17]); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	state.LastSuccessGenerationID = nullStr(values[0])
	state.LastSuccessAt = nullStr(values[1])
	state.LastSourceSnapshotID = nullStr(values[2])
	state.LastPolicyHash = nullStr(values[3])
	state.LastEligibilityContractHash = nullStr(values[4])
	state.LocalCleanOperationID = nullStr(values[5])
	state.DesiredDerivedGenerationID = nullStr(values[6])
	state.CurrentRemoteBindingFingerprint = nullStr(values[7])
	state.RemotePublishedGenerationID = nullStr(values[8])
	state.RemotePublishedBindingFingerprint = nullStr(values[9])
	state.RemoteStateBindingFingerprint = nullStr(values[10])
	state.ActivePublishGenerationID = nullStr(values[11])
	state.LastDerivedError = nullStr(values[12])
	state.LastDerivedSuccessAt = nullStr(values[13])
	state.DerivedRetryTargetKey = nullStr(values[14])
	state.DerivedRetryClassification = nullStr(values[15])
	state.DerivedNextRetryAt = nullStr(values[16])
	state.DerivedTerminalErrorCode = nullStr(values[17])
	return &state, nil
}

func (d *DB) GetCompilerState(profileID string) (*CompilerProfileState, error) {
	return scanCompilerState(d.QueryRow(`SELECT `+compilerStateCols+` FROM compiler_profile_state WHERE profile_id = ?`, profileID))
}

func (d *DB) EnsureCompilerState(profileID string) error {
	_, err := d.Exec(`INSERT OR IGNORE INTO compiler_profile_state (profile_id) VALUES (?)`, profileID)
	return err
}

func (d *DB) StartCompilerRun(profileID, runID, generationID, compilerVersion string, schemaVersion int, policyHash, contractHash string) error {
	if err := d.EnsureCompilerState(profileID); err != nil {
		return err
	}
	now := Now().Format(timeFmt)
	_, err := d.Exec(`INSERT INTO compiler_runs
		(id, profile_id, candidate_generation_id, started_at, status, compiler_version,
		schema_version, policy_hash, eligibility_contract_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, runID, profileID, generationID, now,
		CompilerRunRunning, compilerVersion, schemaVersion, policyHash, contractHash)
	return err
}

// FinishCompilerRun records the local publication fact and advances the
// level-triggered desired derived state in one operational transaction.
func (d *DB) FinishCompilerRun(profileID, runID, generationID, snapshotID, policyHash, contractHash, bindingFingerprint string, fileCount, warningCount int) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now().Format(timeFmt)
	if _, err := tx.Exec(`UPDATE compiler_runs SET status = ?, completed_at = ?, source_snapshot_id = ?,
		file_count = ?, warning_count = ? WHERE id = ? AND profile_id = ? AND status = ?`,
		CompilerRunSucceeded, now, snapshotID, fileCount, warningCount, runID, profileID, CompilerRunRunning); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE compiler_profile_state SET
		last_success_generation_id = ?, last_success_at = ?, last_source_snapshot_id = ?,
		last_policy_hash = ?, last_eligibility_contract_hash = ?, local_mode = ?,
		current_remote_binding_fingerprint = ?,
		desired_derived_mode = ?, desired_derived_generation_id = ?,
		desired_derived_revision = desired_derived_revision + 1,
		derived_state = 'pending'
		WHERE profile_id = ?`, generationID, now, snapshotID, policyHash, contractHash,
		CompilerLocalGeneration, bindingFingerprint, CompilerDesiredGeneration, generationID, profileID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) FailCompilerRun(profileID, runID, message string) error {
	now := Now().Format(timeFmt)
	_, err := d.Exec(`UPDATE compiler_runs SET status = ?, completed_at = ?, error = ?
		WHERE id = ? AND profile_id = ? AND status = ?`, CompilerRunFailed, now, message, runID, profileID, CompilerRunRunning)
	return err
}

func (d *DB) MarkCompilerClean(profileID, operationID string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE compiler_profile_state SET desired_derived_mode = ?,
		desired_derived_generation_id = NULL, desired_derived_revision = desired_derived_revision + 1,
		local_clean_state = 'committing', local_clean_operation_id = ?, local_mode = 'absent',
		derived_state = 'pending'
		WHERE profile_id = ?`, CompilerDesiredAbsent, operationID, profileID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) FinishCompilerClean(profileID string) error {
	_, err := d.Exec(`UPDATE compiler_profile_state SET local_clean_state = 'none',
		local_clean_operation_id = NULL WHERE profile_id = ?`, profileID)
	return err
}

const (
	DerivedRunPublish     = "publish"
	DerivedRunPurge       = "purge"
	DerivedRunRunning     = "running"
	DerivedRunSuccess     = "succeeded"
	DerivedRunFailed      = "failed"
	DerivedRunInterrupted = "interrupted"
)

type DerivedRun struct {
	ID                       string  `json:"id"`
	ProfileID                string  `json:"profile_id"`
	Kind                     string  `json:"kind"`
	TargetGenerationID       *string `json:"target_generation_id,omitempty"`
	TargetBindingFingerprint string  `json:"target_binding_fingerprint"`
	TargetDesiredRevision    int64   `json:"target_desired_revision"`
	TargetKey                string  `json:"target_key"`
	Status                   string  `json:"status"`
	Phase                    string  `json:"phase"`
	StartedAt                string  `json:"started_at"`
	CompletedAt              *string `json:"completed_at,omitempty"`
	ErrorCode                string  `json:"error_code"`
	ErrorClassification      string  `json:"error_classification"`
	Error                    string  `json:"error"`
}

// DerivedTargetKey identifies the current level-triggered derived desire.
func DerivedTargetKey(mode string, generation *string, revision int64, binding string) string {
	g := ""
	if generation != nil {
		g = *generation
	}
	return fmt.Sprintf("derived-v1\x00%s\x00%s\x00%d\x00%s", mode, g, revision, binding)
}

// ClaimDerivedRun atomically claims the current derived desire. It returns
// nil when the desire is already current, gated, or another attempt is active.
func (d *DB) ClaimDerivedRun(profileID, runID, binding string) (*DerivedRun, bool, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var tomb, enabled int
	var deleting sql.NullString
	if err := tx.QueryRow(`SELECT tombstoned, enabled, deletion_requested_at FROM profiles WHERE id = ?`, profileID).Scan(&tomb, &enabled, &deleting); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if tomb == 1 || enabled == 0 || deleting.Valid {
		return nil, false, nil
	}

	var mode, desired, published, publishedBinding, remoteState, remoteBinding, active string
	var desiredRevision int64
	var desiredNull, publishedNull, publishedBindingNull, remoteBindingNull, activeNull sql.NullString
	var state string
	if err := tx.QueryRow(`SELECT desired_derived_mode, desired_derived_generation_id,
		 desired_derived_revision, remote_published_generation_id,
		 remote_published_binding_fingerprint, remote_state,
		 remote_state_binding_fingerprint, active_publish_generation_id,
		 derived_state FROM compiler_profile_state WHERE profile_id = ?`, profileID).
		Scan(&mode, &desiredNull, &desiredRevision, &publishedNull, &publishedBindingNull,
			&remoteState, &remoteBindingNull, &activeNull, &state); err != nil {
		return nil, false, err
	}
	desired = desiredNull.String
	published = publishedNull.String
	publishedBinding = publishedBindingNull.String
	remoteBinding = remoteBindingNull.String
	active = activeNull.String
	if desiredRevision == 0 {
		// The default row represents no compiler command yet, not an implicit
		// request to purge the derived namespace.
		return nil, false, nil
	}
	if active != "" {
		return nil, false, nil
	}
	current := (mode == CompilerDesiredAbsent && remoteState == "absent" && remoteBinding == binding) ||
		(mode == CompilerDesiredGeneration && desired != "" && published == desired && publishedBinding == binding && remoteState == "generation" && remoteBinding == binding)
	if current {
		return nil, false, nil
	}
	targetKey := DerivedTargetKey(mode, nullableString(desired), desiredRevision, binding)
	var retryKey, retryClass, nextRetry, terminal sql.NullString
	if err := tx.QueryRow(`SELECT derived_retry_target_key, derived_retry_classification,
		derived_next_retry_at, derived_terminal_error_code FROM compiler_profile_state WHERE profile_id = ?`, profileID).
		Scan(&retryKey, &retryClass, &nextRetry, &terminal); err != nil {
		return nil, false, err
	}
	if retryKey.Valid && retryKey.String == targetKey {
		if retryClass.String == RetryTerminal || (nextRetry.Valid && Now().Format(timeFmt) < nextRetry.String) {
			return nil, false, nil
		}
	}

	kind := DerivedRunPublish
	var targetGeneration interface{}
	if mode == CompilerDesiredAbsent {
		kind = DerivedRunPurge
	} else {
		targetGeneration = desired
	}
	now := Now().Format(timeFmt)
	if _, err := tx.Exec(`INSERT INTO compiler_derived_runs
		(id, profile_id, kind, target_generation_id, target_binding_fingerprint,
		target_desired_revision, status, phase, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '', ?)`, runID, profileID, kind, targetGeneration,
		binding, desiredRevision, DerivedRunRunning, now); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`UPDATE compiler_profile_state SET derived_state = 'syncing',
		active_publish_generation_id = ?, current_remote_binding_fingerprint = ?,
		derived_retry_target_key = ?, derived_retry_classification = NULL,
		derived_next_retry_at = NULL, derived_terminal_error_code = NULL
		WHERE profile_id = ?`, targetGeneration, binding, targetKey, profileID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &DerivedRun{ID: runID, ProfileID: profileID, Kind: kind, TargetGenerationID: nullableString(desired), TargetBindingFingerprint: binding, TargetDesiredRevision: desiredRevision, TargetKey: targetKey, Status: DerivedRunRunning}, true, nil
}

func nullableString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// FinishDerivedRunSuccess records remote publication only for the claimed run.
func (d *DB) FinishDerivedRunSuccess(profileID, runID, binding string, generation *string, kind string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now().Format(timeFmt)
	if _, err := tx.Exec(`UPDATE compiler_derived_runs SET status = ?, completed_at = ?
		WHERE id = ? AND profile_id = ? AND status = ?`, DerivedRunSuccess, now, runID, profileID, DerivedRunRunning); err != nil {
		return err
	}
	remoteState := "generation"
	if kind == DerivedRunPurge {
		remoteState = "absent"
	}
	derivedState := "pending"
	var mode, desired string
	var desiredNull sql.NullString
	if err := tx.QueryRow(`SELECT desired_derived_mode, desired_derived_generation_id FROM compiler_profile_state WHERE profile_id = ?`, profileID).Scan(&mode, &desiredNull); err != nil {
		return err
	}
	desired = desiredNull.String
	if (kind == DerivedRunPurge && mode == CompilerDesiredAbsent) || (kind == DerivedRunPublish && mode == CompilerDesiredGeneration && generation != nil && desired == *generation) {
		derivedState = "current"
	}
	if _, err := tx.Exec(`UPDATE compiler_profile_state SET
		remote_published_generation_id = ?, remote_published_binding_fingerprint = ?,
		remote_state = ?, remote_state_binding_fingerprint = ?, derived_state = ?,
		active_publish_generation_id = NULL, derived_retry_target_key = NULL,
		derived_retry_classification = NULL, derived_consecutive_failures = 0,
		derived_limited_failures = 0, derived_next_retry_at = NULL,
		derived_terminal_error_code = NULL, last_derived_error = NULL,
		last_derived_success_at = ? WHERE profile_id = ?`, generation, binding,
		remoteState, binding, derivedState, now, profileID); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishDerivedRunFailure stores a failure and applies its retry gate.
func (d *DB) FinishDerivedRunFailure(profileID, runID, targetKey, code, classification, message string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	if _, err := tx.Exec(`UPDATE compiler_derived_runs SET status = ?, completed_at = ?,
		error_code = ?, error_classification = ?, error = ?
		WHERE id = ? AND profile_id = ? AND status = ?`, DerivedRunFailed, now.Format(timeFmt), code,
		classification, message, runID, profileID, DerivedRunRunning); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE compiler_profile_state SET active_publish_generation_id = NULL WHERE profile_id = ?`, profileID); err != nil {
		return err
	}
	var failures, limited int
	if err := tx.QueryRow(`SELECT derived_consecutive_failures, derived_limited_failures FROM compiler_profile_state WHERE profile_id = ?`, profileID).Scan(&failures, &limited); err != nil {
		return err
	}
	failures++
	if classification == RetryRetryableLimited {
		limited++
	}
	next := now.Add(RetryBackoff(failures)).Format(timeFmt)
	if classification == RetryRetryableLimited {
		next = now.Add(LimitedRetryBackoff(limited)).Format(timeFmt)
	}
	terminalCode := interface{}(nil)
	if classification == RetryTerminal {
		next = ""
		terminalCode = code
	}
	if _, err := tx.Exec(`UPDATE compiler_profile_state SET derived_state = 'failed',
		active_publish_generation_id = NULL, last_derived_error = ?,
		derived_retry_classification = ?, derived_consecutive_failures = ?,
		derived_limited_failures = ?, derived_next_retry_at = NULLIF(?, ''),
		derived_terminal_error_code = ?, derived_retry_target_key = ?
		WHERE profile_id = ? AND (derived_retry_target_key = ? OR derived_retry_target_key IS NULL)`,
		message, classification, failures, limited, next, terminalCode, targetKey, profileID, targetKey); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) UpdateDerivedRunPhase(profileID, runID, phase string) error {
	_, err := d.Exec(`UPDATE compiler_derived_runs SET phase = ? WHERE id = ? AND profile_id = ? AND status = ?`, phase, runID, profileID, DerivedRunRunning)
	return err
}

// RecoverDerivedRuns releases pins left by a crashed worker. Publication is
// idempotent, so interrupted attempts become immediately retryable desires.
func (d *DB) RecoverDerivedRuns() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now().Format(timeFmt)
	if _, err := tx.Exec(`UPDATE compiler_derived_runs SET status = ?, completed_at = ?,
		error_code = ?, error_classification = ?, error = ? WHERE status = ?`,
		DerivedRunInterrupted, now, WorkerInterruptedCode, RetryRetryable,
		"derived run interrupted by worker restart", DerivedRunRunning); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE compiler_profile_state SET active_publish_generation_id = NULL,
		derived_state = CASE WHEN derived_state = 'syncing' THEN 'pending' ELSE derived_state END,
		derived_retry_target_key = NULL, derived_retry_classification = NULL,
		derived_next_retry_at = NULL WHERE active_publish_generation_id IS NOT NULL`); err != nil {
		return err
	}
	return tx.Commit()
}
