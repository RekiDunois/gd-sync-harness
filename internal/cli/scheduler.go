package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"time"

	"knowledge-sync/internal/state"
)

// syncScheduler provides remote-level bounded concurrency (§20.2). It is
// populated lazily by the app. Fast upserts get high priority (10); full
// reconciliations get low priority (1).
type syncScheduler struct {
	db *state.DB
}

func newSyncScheduler(db ...*state.DB) *syncScheduler {
	s := &syncScheduler{}
	if len(db) > 0 {
		s.db = db[0]
	}
	return s
}

// submitFast runs a fast upsert through the remote scheduler with high priority.
func (s *syncScheduler) submitFast(ctx context.Context, remote string, fn func(ctx context.Context) error) error {
	if s.db == nil {
		return fn(ctx)
	}
	leaseID := leaseID()
	if err := s.db.AcquireRemoteLease(ctx, remote, 10, 2, os.Getpid(), leaseID); err != nil {
		return err
	}
	stopRenewal := startLeaseRenewal(ctx, s.db, leaseID)
	defer stopRenewal()
	defer s.db.ReleaseRemoteLease(leaseID)
	return fn(ctx)
}

func startLeaseRenewal(ctx context.Context, db *state.DB, id string) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = db.RenewRemoteLease(id)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// upsertForProfile runs a fast upsert for a profile, respecting remote concurrency.
func (a *App) upsertForProfile(ctx context.Context, p *state.Profile, files []string) error {
	svc := a.Sync
	return a.scheduler.submitFast(ctx, p.RemoteName, func(c context.Context) error {
		return svc.FastUpsert(c, p, files)
	})
}

func leaseID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "lease-fallback"
	}
	return hex.EncodeToString(b)
}

var _ = state.WatcherStopped
