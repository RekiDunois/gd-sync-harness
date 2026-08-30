package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const remoteLeaseDuration = 5 * time.Minute

// AcquireRemoteLease persists a waiting operation and promotes it atomically
// when it is both eligible by priority and below the per-remote concurrency
// limit. The database row makes the limit effective across processes.
func (d *DB) AcquireRemoteLease(ctx context.Context, remote string, priority, maxConcurrent, ownerPID int, id string) error {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	now := Now()
	if _, err := d.Exec(`INSERT INTO remote_operation_leases
		(id, remote_name, priority, owner_pid, state, created_at, lease_until)
		VALUES (?, ?, ?, ?, 'waiting', ?, ?)`, id, remote, priority, ownerPID,
		now.Format(timeFmt), now.Add(remoteLeaseDuration).Format(timeFmt)); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = d.ReleaseRemoteLease(id)
			return err
		}
		claimed, err := d.tryClaimRemoteLease(remote, id, maxConcurrent)
		if err != nil {
			_ = d.ReleaseRemoteLease(id)
			return err
		}
		if claimed {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = d.ReleaseRemoteLease(id)
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (d *DB) tryClaimRemoteLease(remote, id string, maxConcurrent int) (bool, error) {
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := Now()
	if _, err := tx.Exec(`DELETE FROM remote_operation_leases
		WHERE state = 'running' AND lease_until < ?`, now.Format(timeFmt)); err != nil {
		return false, err
	}
	var running int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM remote_operation_leases
		WHERE remote_name = ? AND state = 'running'`, remote).Scan(&running); err != nil {
		return false, err
	}
	if running >= maxConcurrent {
		return false, nil
	}
	var first string
	if err := tx.QueryRow(`SELECT id FROM remote_operation_leases
		WHERE remote_name = ? AND state = 'waiting'
		ORDER BY priority DESC, created_at, id LIMIT 1`, remote).Scan(&first); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if first != id {
		return false, nil
	}
	_, err = tx.Exec(`UPDATE remote_operation_leases SET state = 'running', lease_until = ? WHERE id = ?`,
		now.Add(remoteLeaseDuration).Format(timeFmt), id)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DB) RenewRemoteLease(id string) error {
	res, err := d.Exec(`UPDATE remote_operation_leases SET lease_until = ? WHERE id = ? AND state = 'running'`,
		Now().Add(remoteLeaseDuration).Format(timeFmt), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("remote lease %q is no longer running", id)
	}
	return nil
}

func (d *DB) ReleaseRemoteLease(id string) error {
	_, err := d.Exec(`DELETE FROM remote_operation_leases WHERE id = ?`, id)
	return err
}
