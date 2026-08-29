package cli

import (
	"context"

	"knowledge-sync/internal/sched"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

// syncScheduler provides remote-level bounded concurrency (§20.2). It is
// populated lazily by the app. Fast upserts get high priority (10); full
// reconciliations get low priority (1).
type syncScheduler struct {
	sched *sched.Scheduler
}

func newSyncScheduler() *syncScheduler {
	return &syncScheduler{sched: sched.New(2)}
}

// submitFast runs a fast upsert through the remote scheduler with high priority.
func (s *syncScheduler) submitFast(ctx context.Context, remote string, fn func(ctx context.Context) error) error {
	done := s.sched.Submit(ctx, sched.Job{Remote: remote, Priority: 10, Run: fn})
	return <-done
}

// submitReconcile runs a full reconciliation through the remote scheduler with
// low priority (long operations must not starve small fast upserts).
func (s *syncScheduler) submitReconcile(ctx context.Context, remote string, fn func(ctx context.Context) error) error {
	done := s.sched.Submit(ctx, sched.Job{Remote: remote, Priority: 1, Run: fn})
	return <-done
}

// upsertForProfile runs a fast upsert for a profile, respecting remote concurrency.
func (a *App) upsertForProfile(ctx context.Context, p *state.Profile, files []string) error {
	svc := a.Sync
	return a.scheduler.submitFast(ctx, p.RemoteName, func(c context.Context) error {
		return svc.FastUpsert(c, p, files)
	})
}

// reconcileForProfile runs a full reconciliation, respecting remote concurrency.
func (a *App) reconcileForProfile(ctx context.Context, p *state.Profile, options sync.SyncOptions) (*sync.PreflightResult, error) {
	rec := a.Reconciler
	var pre *sync.PreflightResult
	var runErr error
	a.scheduler.submitReconcile(ctx, p.RemoteName, func(c context.Context) error {
		pre, runErr = rec.Reconcile(c, p, options)
		return runErr
	})
	return pre, runErr
}

var _ = state.WatcherStopped
