package state

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"knowledge-sync/internal/policy"
)

// PolicyRefreshState aliases the policy refresh state values for state-layer
// consumers.
const (
	PolicyRefreshPending = policy.RefreshPending
	PolicyRefreshRunning = policy.RefreshRunning
	PolicyRefreshReady   = policy.RefreshReady
	PolicyRefreshError   = policy.RefreshError

	PolicySourceGitignore      = policy.SourceGitignore
	PolicySourceLegacyMigrated = policy.SourceLegacyMigrated
)

// CommittedPolicy is the durable committed ignore policy for a profile.
type CommittedPolicy struct {
	ProfileID           string  `json:"profile_id"`
	PolicySource        string  `json:"policy_source"`
	PolicyHash          string  `json:"policy_hash"`
	CommittedGeneration int64   `json:"committed_generation"`
	CommittedAt         string  `json:"committed_at"`
	RefreshState        string  `json:"refresh_state"`
	RefreshedPolicyHash *string `json:"refreshed_policy_hash,omitempty"`
	MatcherWarningCount int     `json:"matcher_warning_count"`
}

// PolicyCommitResult reports the outcome of an ignore update.
type PolicyCommitResult struct {
	Changed         bool   `json:"changed"`
	PolicyHash      string `json:"policy_hash"`
	Generation      int64  `json:"generation"`
	MatcherWarnings int    `json:"matcher_warnings"`
	RefreshState    string `json:"refresh_state"`
}

var ErrLegacyDropRequired = errors.New("legacy-migrated policy requires --accept-legacy-drop")

// scanCommittedPolicy scans a committed policy row.
func scanCommittedPolicy(row interface{ Scan(...any) error }) (*CommittedPolicy, error) {
	var p CommittedPolicy
	var refreshed sql.NullString
	if err := row.Scan(&p.ProfileID, &p.PolicySource, &p.PolicyHash,
		&p.CommittedGeneration, &p.CommittedAt, &p.RefreshState, &refreshed,
		&p.MatcherWarningCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.RefreshedPolicyHash = nullStr(refreshed)
	return &p, nil
}

const committedPolicyCols = `profile_id, policy_source, policy_hash, committed_generation,
	committed_at, refresh_state, refreshed_policy_hash, matcher_warning_count`

// GetCommittedPolicy returns the committed policy for a profile.
func (d *DB) GetCommittedPolicy(profileID string) (*CommittedPolicy, error) {
	return scanCommittedPolicy(d.QueryRow(`SELECT `+committedPolicyCols+`
		FROM profile_ignore_policy WHERE profile_id = ?`, profileID))
}

// backfillPolicyRows converts pre-policy profiles to the committed-policy model
// (§6.2). Profiles with structured excludes get a synthetic `legacy_migrated`
// snapshot preserving their current effective behavior; profiles with no
// excludes migrate directly to a safe empty `gitignore` policy. The backfill is
// idempotent and runs after schema migration.
func (d *DB) backfillPolicyRows() error {
	profiles, err := d.ListProfiles()
	if err != nil {
		return err
	}
	for _, p := range profiles {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM profile_ignore_policy WHERE profile_id = ?`, p.ID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		rt, err := d.GetRuntime(p.ID)
		if err != nil {
			return err
		}
		gen := rt.SourceGeneration
		if gen < 1 {
			gen = 1
		}
		excludes, err := d.GetExcludes(p.ID)
		if err != nil {
			return err
		}
		if len(excludes) == 0 {
			// No legacy rules: migrate directly to an empty gitignore policy.
			if err := d.EnsurePolicyRow(p.ID, gen); err != nil {
				return err
			}
			continue
		}
		// Convert structured rules into a synthetic legacy_migrated snapshot.
		rules := make([]policy.LegacyRule, 0, len(excludes))
		for _, ex := range excludes {
			idx := strings.Index(ex, ":")
			if idx <= 0 {
				continue
			}
			rules = append(rules, policy.LegacyRule{Kind: ex[:idx], Value: ex[idx+1:]})
		}
		snap := policy.ConvertLegacyRules(rules)
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		hash := snap.Hash()
		now := Now().Format(timeFmt)
		if _, err := tx.Exec(`INSERT INTO profile_ignore_policy
			(profile_id, policy_source, policy_hash, committed_generation, committed_at, refresh_state, matcher_warning_count)
			VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
			p.ID, PolicySourceLegacyMigrated, hash, gen, now, len(snap.Warnings)); err != nil {
			return err
		}
		for i, f := range snap.Files {
			if _, err := tx.Exec(`INSERT INTO profile_ignore_snapshot_files
				(profile_id, policy_hash, relative_path, scope_dir, content, content_order)
				VALUES (?, ?, ?, ?, ?, ?)`, p.ID, hash, f.RelativePath, f.ScopeDir, f.Content, i); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
func (d *DB) EnsurePolicyRow(profileID string, committedGeneration int64) error {
	_, err := d.Exec(`INSERT OR IGNORE INTO profile_ignore_policy
		(profile_id, policy_source, policy_hash, committed_generation, committed_at, refresh_state)
		VALUES (?, ?, 'empty', ?, ?, 'pending')`,
		profileID, PolicySourceGitignore, committedGeneration, Now().Format(timeFmt))
	return err
}

// GetPolicySnapshotFiles returns the committed snapshot files for a profile
// ordered by content_order.
func (d *DB) GetPolicySnapshotFiles(profileID string) ([]policy.File, error) {
	rows, err := d.Query(`SELECT relative_path, scope_dir, content FROM profile_ignore_snapshot_files
		WHERE profile_id = ? ORDER BY content_order`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []policy.File
	for rows.Next() {
		var f policy.File
		if err := rows.Scan(&f.RelativePath, &f.ScopeDir, &f.Content); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetCommittedSnapshot assembles a policy.Snapshot from the committed rows.
// It returns a nil snapshot when no committed policy exists.
func (d *DB) GetCommittedSnapshot(profileID string) (*policy.Snapshot, error) {
	files, err := d.GetPolicySnapshotFiles(profileID)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, nil
	}
	return &policy.Snapshot{Files: files}, nil
}

// CommitIgnoreSnapshot atomically persists a policy snapshot with a new
// content-derived policy_hash and advances the unified input generation exactly
// once per changed snapshot (§7). For policy_source=legacy_migrated profiles the
// first commit requires acceptLegacyDrop (§6.4). It marks the new policy
// refresh pending, advances desired_generation, records the durable debounce
// window, and marks older unexecuted prune requests stale where their
// policy_hash differs.
func (d *DB) CommitIgnoreSnapshot(profileID string, snap *policy.Snapshot, acceptLegacyDrop bool) (*PolicyCommitResult, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Verify the profile is valid for policy mutation.
	var tomb int
	if err := tx.QueryRow(`SELECT tombstoned FROM profiles WHERE id = ?`, profileID).Scan(&tomb); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if tomb == 1 {
		return nil, fmt.Errorf("profile %q is tombstoned", profileID)
	}

	var cur policySourceRow
	if err := tx.QueryRow(`SELECT policy_source, policy_hash, committed_generation FROM profile_ignore_policy
		WHERE profile_id = ?`, profileID).Scan(&cur.source, &cur.hash, &cur.generation); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}

	newHash := snap.Hash()
	if cur.hash == newHash && cur.source != "" {
		// Byte-identical snapshot: no-op, does not advance generation (§4.3).
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &PolicyCommitResult{Changed: false, PolicyHash: newHash,
			RefreshState: policy.RefreshReady, MatcherWarnings: len(snap.Warnings)}, nil
	}

	if cur.source == PolicySourceLegacyMigrated {
		if !acceptLegacyDrop {
			return nil, ErrLegacyDropRequired
		}
	}

	now := Now()
	nowStr := now.Format(timeFmt)

	// Advance the unified input generation exactly once (§7.1). Policy
	// generations share the same monotonic namespace as reconciliation intent.
	// Manual/scheduled reconciliation can move desired_generation ahead without
	// changing the policy row, so deriving the next value only from the prior
	// committed policy generation can create no debt at all. Allocate strictly
	// above every durable generation marker that can already have been observed.
	var sourceGeneration, desiredGeneration int64
	var lastSuccessGeneration sql.NullInt64
	if err := tx.QueryRow(`SELECT r.source_generation, s.desired_generation, s.last_success_generation
		FROM profile_runtime r
		JOIN profile_sync_state s ON s.profile_id = r.profile_id
		WHERE r.profile_id = ?`, profileID).
		Scan(&sourceGeneration, &desiredGeneration, &lastSuccessGeneration); err != nil {
		return nil, err
	}
	baseGeneration := cur.generation
	if sourceGeneration > baseGeneration {
		baseGeneration = sourceGeneration
	}
	if desiredGeneration > baseGeneration {
		baseGeneration = desiredGeneration
	}
	if lastSuccessGeneration.Valid && lastSuccessGeneration.Int64 > baseGeneration {
		baseGeneration = lastSuccessGeneration.Int64
	}
	gen := baseGeneration + 1
	if _, err := tx.Exec(`UPDATE profile_runtime SET source_generation = ? WHERE profile_id = ?`, gen, profileID); err != nil {
		return nil, err
	}

	// Persist the new policy row (replace), snapshot rows (replace).
	newSource := PolicySourceGitignore
	if cur.source == "" {
		newSource = PolicySourceGitignore
	}
	if _, err := tx.Exec(`DELETE FROM profile_ignore_snapshot_files WHERE profile_id = ?`, profileID); err != nil {
		return nil, err
	}
	for i, f := range snap.Files {
		if _, err := tx.Exec(`INSERT INTO profile_ignore_snapshot_files
			(profile_id, policy_hash, relative_path, scope_dir, content, content_order)
			VALUES (?, ?, ?, ?, ?, ?)`, profileID, newHash, f.RelativePath, f.ScopeDir, f.Content, i); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO profile_ignore_policy
		(profile_id, policy_source, policy_hash, committed_generation, committed_at, refresh_state, refreshed_policy_hash, matcher_warning_count)
		VALUES (?, ?, ?, ?, ?, ?, NULL, ?)
		ON CONFLICT(profile_id) DO UPDATE SET
			policy_source = excluded.policy_source,
			policy_hash = excluded.policy_hash,
			committed_generation = excluded.committed_generation,
			committed_at = excluded.committed_at,
			refresh_state = excluded.refresh_state,
			refreshed_policy_hash = NULL,
			matcher_warning_count = excluded.matcher_warning_count`,
		profileID, newSource, newHash, gen, nowStr, policy.RefreshPending, len(snap.Warnings)); err != nil {
		return nil, err
	}

	// Advance desired_generation so the worker refreshes the new policy (§7.2).
	// A policy commit changes eligibility but is non-destructive (suppressed
	// objects are protected), so it does not impose a new destructive debounce
	// window. If a one-shot manual intent is still pending, carry its generation
	// forward so its execution metadata is consumed by the coalesced run rather
	// than becoming stranded on an older generation.
	if _, err := tx.Exec(`UPDATE profile_sync_state SET
		desired_generation = MAX(desired_generation, ?),
		pending_manual_generation = CASE
			WHEN pending_manual_generation IS NULL THEN NULL
			ELSE ?
		END,
		state = CASE
			WHEN state = ? THEN state
			WHEN last_success_generation IS NULL THEN ?
			ELSE ?
		END
		WHERE profile_id = ?`,
		gen, gen, StateError, StateInitializing, StateSyncing, profileID); err != nil {
		return nil, err
	}

	// Mark older unexecuted prune requests stale where their policy_hash
	// differs (§7.2, §14.3). previewed / approval_required / pending (queued,
	// unexecuted) are invalidated by a policy change; running/retrying requests
	// are serialized behind this commit's lock and keep their authorization.
	if _, err := tx.Exec(`UPDATE prune_requests SET state = ?, updated_at = ?
		WHERE profile_id = ? AND policy_hash <> ? AND state IN (?, ?, ?)`,
		PruneStateStale, nowStr, profileID, newHash,
		PruneStatePreviewed, PruneStateApprovalRequired, PruneStatePending); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &PolicyCommitResult{Changed: true, PolicyHash: newHash, Generation: gen,
		MatcherWarnings: len(snap.Warnings), RefreshState: policy.RefreshPending}, nil
}

type policySourceRow struct {
	source     string
	hash       string
	generation int64
}

// MarkPolicyRefreshRunning marks the current committed policy refresh running
// for a policy_hash.
func (d *DB) MarkPolicyRefreshRunning(profileID, policyHash string) error {
	_, err := d.Exec(`UPDATE profile_ignore_policy SET refresh_state = ? WHERE profile_id = ? AND policy_hash = ?`,
		PolicyRefreshRunning, profileID, policyHash)
	return err
}

// EventEvidence is the durable policy-order evidence for a refresh.
type EventEvidence struct {
	// DeletePaths are managed paths proven to be ordinary local deletions
	// (not policy suppression). They stay active so ordinary reconcile removes
	// them remotely (§9.2).
	DeletePaths map[string]bool
}

// ClassifyDisappearances decides, for active managed paths absent from the
// eligible scan, whether each is a proven ordinary deletion or a suppression
// (§9). A path is a proven ordinary deletion when:
//
//  1. a durable destructive pending event was recorded with a known policy
//     context predating the current committed policy hash (delete observed
//     while active under the prior policy), OR
//  2. the current committed policy does not ignore the path (it disappeared
//     for a non-policy reason while the policy is stable).
//
// Otherwise the disappearance is classified as suppression (fail-safe; §9.3):
// false suppression is recoverable, an unproven destructive delete is not.
func (d *DB) ClassifyDisappearances(profileID, committedPolicyHash string, activePaths map[string]bool, snap *policy.Snapshot) (*EventEvidence, error) {
	pending, err := d.ListPending(profileID)
	if err != nil {
		return nil, err
	}
	ev := &EventEvidence{DeletePaths: map[string]bool{}}

	// Collect every active ledger path that is absent from the eligible scan.
	ledger, err := d.ManifestAllState(profileID, ManifestActive)
	if err != nil {
		return nil, err
	}
	disappeared := map[string]bool{}
	for _, e := range ledger {
		if !activePaths[e.RelPath] {
			disappeared[e.RelPath] = true
		}
	}
	if len(disappeared) == 0 {
		return ev, nil
	}

	// Proven delete via durable pre-policy destructive event (§9.2).
	for _, e := range pending {
		if e.EventKind != EventDelete && e.EventKind != EventRename && e.EventKind != EventOther {
			continue
		}
		if !disappeared[e.Path] {
			continue
		}
		if !e.PolicyContextKnown {
			continue
		}
		// The delete was recorded under a policy hash that differs from the
		// current committed hash, so it predates the current policy. The path
		// was active under the prior policy when it disappeared.
		if e.ObservedPolicyHash != nil && *e.ObservedPolicyHash != committedPolicyHash {
			ev.DeletePaths[e.Path] = true
			continue
		}
		// Recorded under the same (current) hash: the path disappeared while
		// this policy was already in effect and is not ignored by it.
		if e.ObservedPolicyHash != nil && *e.ObservedPolicyHash == committedPolicyHash &&
			!snap.Excluded(e.Path, false) {
			ev.DeletePaths[e.Path] = true
		}
	}

	// Stable-policy disappearance: the current policy does not ignore the path,
	// so it vanished for a non-policy reason (§9.1, §9.3).
	for p := range disappeared {
		if !snap.Excluded(p, false) {
			ev.DeletePaths[p] = true
		}
	}
	return ev, nil
}

// ApplyManagedRefresh classifies the managed ledger against the current
// committed policy and generation (§10.2). It applies the state transitions:
//
//	active + still eligible                 -> active
//	active + now ignored                    -> suppressed
//	proven delete (no longer eligible)      -> stays active (ordinary reconcile deletes it)
//	suppressed + eligible again + exists    -> active
//	suppressed + still ignored              -> suppressed
//
// provenDeletes are active paths with durable delete/rename evidence recorded
// while the path was active under a prior policy (§9.2). They are excluded from
// suppression so ordinary reconciliation removes them. All other active rows
// absent from the eligible scan become suppressed (fail-safe; §9.3).
func (d *DB) ApplyManagedRefresh(profileID, policyHash string, generation int64, activePaths map[string]bool, provenDeletes map[string]bool) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Active rows that remain eligible stay active; active rows now ignored
	// become suppressed, except proven ordinary deletions.
	var placeholders []string
	args := make([]any, 0, len(activePaths)+len(provenDeletes)+4)
	args = append(args, ManifestSuppressed, policyHash, generation, profileID, ManifestActive)
	for p := range activePaths {
		placeholders = append(placeholders, "?")
		args = append(args, p)
	}
	for p := range provenDeletes {
		placeholders = append(placeholders, "?")
		args = append(args, p)
	}
	suppress := `UPDATE manifest SET state = ?, suppressed_policy_hash = ?, suppressed_generation = ?
		WHERE profile_id = ? AND state = ? AND rel_path NOT IN (` + strings.Join(placeholders, ",") + `)`
	if _, err := tx.Exec(suppress, args...); err != nil {
		return err
	}

	// Suppressed rows that are eligible again and still exist on disk
	// reactivate; suppressed rows still ignored stay suppressed.
	for p := range activePaths {
		if _, err := tx.Exec(`UPDATE manifest SET state = ?, suppressed_policy_hash = NULL, suppressed_generation = NULL
			WHERE profile_id = ? AND rel_path = ? AND state = ?`,
			ManifestActive, profileID, p, ManifestSuppressed); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) MarkPolicyRefreshReady(profileID, policyHash string) error {
	_, err := d.Exec(`UPDATE profile_ignore_policy SET refresh_state = ?, refreshed_policy_hash = ?
		WHERE profile_id = ? AND policy_hash = ?`,
		PolicyRefreshReady, policyHash, profileID, policyHash)
	return err
}

// MarkPolicyRefreshError marks a policy refresh error for a policy_hash.
func (d *DB) MarkPolicyRefreshError(profileID, policyHash string, errMsg string) error {
	_, err := d.Exec(`UPDATE profile_ignore_policy SET refresh_state = ? WHERE profile_id = ? AND policy_hash = ?`,
		PolicyRefreshError, profileID, policyHash)
	return err
}

// PolicyRefreshReadyForHash reports whether the committed policy refresh is
// ready for the given hash (§8.2). It returns false when no policy exists.
func (d *DB) PolicyRefreshReadyForHash(profileID, policyHash string) (bool, error) {
	var state string
	err := d.QueryRow(`SELECT refresh_state FROM profile_ignore_policy
		WHERE profile_id = ? AND policy_hash = ? AND refresh_state = ?`,
		profileID, policyHash, PolicyRefreshReady).Scan(&state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CommitPolicyForNewProfile persists the initial policy snapshot captured during
// profile creation atomically with the profile rows (§6.1). The profile must
// already exist (CreateProfile ran) so the worker cannot run it with an
// accidental empty policy.
func (d *DB) CommitPolicyForNewProfile(profileID string, snap *policy.Snapshot) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now().Format(timeFmt)
	var gen int64
	if err := tx.QueryRow(`SELECT source_generation FROM profile_runtime WHERE profile_id = ?`, profileID).Scan(&gen); err != nil {
		return err
	}
	hash := snap.Hash()
	if _, err := tx.Exec(`INSERT INTO profile_ignore_policy
		(profile_id, policy_source, policy_hash, committed_generation, committed_at, refresh_state, matcher_warning_count)
		VALUES (?, ?, ?, ?, ?, 'pending', ?)`,
		profileID, PolicySourceGitignore, hash, gen, now, len(snap.Warnings)); err != nil {
		return err
	}
	for i, f := range snap.Files {
		if _, err := tx.Exec(`INSERT INTO profile_ignore_snapshot_files
			(profile_id, policy_hash, relative_path, scope_dir, content, content_order)
			VALUES (?, ?, ?, ?, ?, ?)`, profileID, hash, f.RelativePath, f.ScopeDir, f.Content, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}
