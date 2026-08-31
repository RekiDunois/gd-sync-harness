package state

import (
	"database/sql"
	"fmt"
	"path"
	"sort"
	"strings"
)

// CreatePrunePreviewFromUnmanagedPaths freezes an explicitly discovered set of
// remote paths into the existing durable prune request model. The caller is
// responsible for discovering paths under the profile's remote root and for
// proving they match the committed ignore policy. This method provides the
// durable boundary: it re-validates the expected ready policy hash and excludes
// any path that has become managed before the request is committed.
func (d *DB) CreatePrunePreviewFromUnmanagedPaths(profileID, requestID, expectedPolicyHash string, candidates []string) (*PruneRequest, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	pol, err := getCommittedPolicyTx(tx, profileID)
	if err != nil {
		return nil, err
	}
	if pol.RefreshState != PolicyRefreshReady || pol.RefreshedPolicyHash == nil ||
		*pol.RefreshedPolicyHash != pol.PolicyHash || pol.PolicyHash != expectedPolicyHash {
		return nil, fmt.Errorf("policy changed or refresh is not ready; repeat orphan discovery")
	}

	paths := normalizePruneCandidatePaths(candidates)
	filtered := paths[:0]
	for _, relPath := range paths {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM manifest WHERE profile_id = ? AND rel_path = ?`,
			profileID, relPath).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			filtered = append(filtered, relPath)
		}
	}
	paths = filtered

	digest := digestPaths(paths)
	now := Now().Format(timeFmt)
	var maxDelete int
	if err := tx.QueryRow(`SELECT max_delete FROM profiles WHERE id = ?`, profileID).Scan(&maxDelete); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}

	// Keep the same single-current-preview semantics as ordinary prune preview.
	if _, err := tx.Exec(`UPDATE prune_requests SET state = ?, updated_at = ?
		WHERE profile_id = ? AND state IN (?, ?)`,
		PruneStateSuperseded, now, profileID, PruneStatePreviewed, PruneStateApprovalRequired); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(`INSERT INTO prune_requests
		(request_id, profile_id, policy_hash, state, candidate_count, candidate_digest,
		 default_max_delete, deleted_count, missing_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`,
		requestID, profileID, pol.PolicyHash, PruneStatePreviewed, len(paths), digest, maxDelete, now, now); err != nil {
		return nil, err
	}
	for _, relPath := range paths {
		if _, err := tx.Exec(`INSERT INTO prune_targets (request_id, rel_path, state, updated_at)
			VALUES (?, ?, ?, ?)`, requestID, relPath, PruneTargetPending, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &PruneRequest{
		RequestID: requestID, ProfileID: profileID, PolicyHash: pol.PolicyHash,
		State: PruneStatePreviewed, CandidateCount: len(paths), CandidateDigest: digest,
		DefaultMaxDelete: maxDelete, CreatedAt: now, UpdatedAt: now,
	}, nil
}

func normalizePruneCandidatePaths(candidates []string) []string {
	set := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		relPath := strings.TrimSpace(strings.ReplaceAll(candidate, "\\", "/"))
		if relPath == "" || strings.HasPrefix(relPath, "/") {
			continue
		}
		relPath = path.Clean(relPath)
		if relPath == "." || relPath == ".." || strings.HasPrefix(relPath, "../") {
			continue
		}
		set[relPath] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for relPath := range set {
		out = append(out, relPath)
	}
	sort.Strings(out)
	return out
}
