package state

import "fmt"

// MarkPruneTargetsResultBatch records terminal outcomes for a set of immutable
// prune targets in one SQLite transaction. It is idempotent for targets that
// are already terminal, so a worker can safely retry after an interrupted
// remote batch. Missing suppressed targets also release their manifest
// ownership in the same transaction.
func (d *DB) MarkPruneTargetsResultBatch(requestID string, relPaths []string, result string) error {
	if len(relPaths) == 0 {
		return nil
	}
	if result != PruneTargetDeleted && result != PruneTargetMissing {
		return fmt.Errorf("invalid prune target batch result %q", result)
	}

	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var profileID string
	if err := tx.QueryRow(`SELECT profile_id FROM prune_requests WHERE request_id = ?`, requestID).Scan(&profileID); err != nil {
		return err
	}

	now := Now().Format(timeFmt)
	var changed int64
	for _, relPath := range relPaths {
		res, err := tx.Exec(`UPDATE prune_targets SET
			state = ?, attempt_count = attempt_count + 1, last_error = NULL, updated_at = ?
			WHERE request_id = ? AND rel_path = ? AND state NOT IN (?, ?)`,
			result, now, requestID, relPath, PruneTargetDeleted, PruneTargetMissing)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed += n
		if result == PruneTargetMissing && n > 0 {
			// A missing unmanaged orphan has no manifest row. A missing
			// suppressed managed object does; clear only suppressed ownership
			// so an unexpected active row can never be removed here.
			if _, err := tx.Exec(`DELETE FROM manifest
				WHERE profile_id = ? AND rel_path = ? AND state = ?`,
				profileID, relPath, ManifestSuppressed); err != nil {
				return err
			}
		}
	}

	if changed > 0 {
		switch result {
		case PruneTargetDeleted:
			if _, err := tx.Exec(`UPDATE prune_requests SET
				deleted_count = deleted_count + ?, updated_at = ? WHERE request_id = ?`,
				changed, now, requestID); err != nil {
				return err
			}
		case PruneTargetMissing:
			if _, err := tx.Exec(`UPDATE prune_requests SET
				missing_count = missing_count + ?, updated_at = ? WHERE request_id = ?`,
				changed, now, requestID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
