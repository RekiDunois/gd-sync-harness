package cli

import (
	"context"
	"log"
	"os"
	"time"

	"knowledge-sync/internal/filter"
	"knowledge-sync/internal/live"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// fastSettings are the durable debounce windows (§13.2). They mirror the
// watcher's historical in-memory values but are evaluated by the worker from
// durable pending_events.first_seen/last_seen so a watcher restart never
// changes work eligibility.
var fastSettings = struct {
	SettleSeconds   int
	MaxDelaySeconds int
}{SettleSeconds: 3, MaxDelaySeconds: 30}

// runFastUpsertBatch evaluates the durable pending-event queue for a profile
// and executes a due fast upsert when no full-reconciliation debt exists
// (§13.2, §13.3). The caller holds the profile lock and remote lease. It loads
// the committed policy snapshot so eligibility rechecks use the owned durable
// policy (§12.3).
func runFastUpsertBatch(ctx context.Context, app *App, p *state.Profile, lg *log.Logger) error {
	snap, err := app.DB.GetCommittedSnapshot(p.ID)
	if err != nil {
		return err
	}
	if snap == nil {
		snap = &policy.Snapshot{}
	}
	return runFastUpsertBatchOwned(ctx, app, p, snap, lg)
}

// runFastUpsertBatchOwned is the worker-owned fast batch under the committed
// policy snapshot (§12.3). Successful fast uploads upsert the managed ledger
// active row before clearing the exact pending event versions (§10.4).
func runFastUpsertBatchOwned(ctx context.Context, app *App, p *state.Profile, snap *policy.Snapshot, lg *log.Logger) error {
	return runFastUpsertBatchAt(ctx, app, p, snap, lg, time.Now(), fastSettings)
}

// runFastUpsertBatchAt is runFastUpsertBatch with an injectable clock and
// settings for deterministic tests.
func runFastUpsertBatchAt(ctx context.Context, app *App, p *state.Profile, snap *policy.Snapshot, lg *log.Logger, now time.Time, settings struct {
	SettleSeconds   int
	MaxDelaySeconds int
}) error {
	lg = workerLog(lg)
	// Full reconciliation debt always wins over a fast batch (§13.3).
	ss, err := app.DB.GetSyncState(p.ID)
	if err != nil {
		return err
	}
	if ss != nil && ss.HasDebt() {
		return nil
	}
	if ss != nil && ss.CurrentRunID != nil {
		return nil
	}
	pending, err := app.DB.ListPending(p.ID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	// Destructive/uncertain pending events promote to a full reconcile rather
	// than an unsafe fast copy (§18.10).
	destructive, err := app.DB.HasDestructivePending(p.ID)
	if err != nil {
		return err
	}
	if destructive {
		rt, err := app.DB.GetRuntime(p.ID)
		if err != nil {
			return err
		}
		return app.DB.PromoteToFullReconcile(p.ID, rt.SourceGeneration, state.Now())
	}

	// Due-batch evaluation from durable timestamps.
	first := time.Time{}
	last := time.Time{}
	for _, e := range pending {
		ft, err := time.Parse(stateTimeLayout, e.FirstSeen)
		if err != nil {
			ft = now
		}
		lt, err := time.Parse(stateTimeLayout, e.LastSeen)
		if err != nil {
			lt = now
		}
		if first.IsZero() || ft.Before(first) {
			first = ft
		}
		if last.IsZero() || lt.After(last) {
			last = lt
		}
	}
	due := now.Sub(last) >= time.Duration(settings.SettleSeconds)*time.Second ||
		now.Sub(first) >= time.Duration(settings.MaxDelaySeconds)*time.Second
	if !due {
		return nil
	}

	// Recheck eligibility per event using the committed policy matcher (§12.3).
	eng := filter.FromPolicy(p.ID, p.MaxFileSize, snap)
	var eligible []state.PendingEvent
	for _, e := range pending {
		if e.EventKind == state.EventModify || e.EventKind == state.EventCreate {
			if _, err := os.Stat(joinSource(p.SourcePath, e.Path)); err != nil {
				continue
			}
			if excluded, _ := eng.ExcludedDir(e.Path, false); excluded {
				continue
			}
		}
		eligible = append(eligible, e)
	}
	if len(eligible) == 0 {
		// All events became stale (file gone/excluded); clear them.
		return app.DB.ClearPending(p.ID)
	}

	// Publish live fast-upsert activity.
	app.activities().start(p.ID, live.ActivityFastUpsert, nil)
	defer app.activities().finish(p.ID)
	app.activities().setPhase(p.ID, state.PhaseUploading)
	publishFast := func() {
		if app.LiveServer != nil {
			app.LiveServer.PublishActivity(p.ID, app.activities().snapshot(p.ID))
		}
	}
	publishFast()

	// Execute through the remote lease held by the caller.
	paths := make([]string, 0, len(eligible))
	for _, e := range eligible {
		paths = append(paths, e.Path)
	}
	lg.Printf("fast upsert %s: %d file(s)", p.ID, len(paths))
	if err := app.Sync.FastUpsert(ctx, p, paths); err != nil {
		// On failure the pending events stay durable; a later pass retries
		// idempotently (§13.4, failure mode).
		_ = app.DB.SetLastError(p.ID, err.Error())
		return err
	}
	// Ledger barrier (§10.4): record durable managed ownership BEFORE clearing
	// the exact event versions. A crash between upload and ledger upsert leaves
	// the event pending so a retry repairs the ledger idempotently.
	for _, rel := range paths {
		if _, err := os.Stat(joinSource(p.SourcePath, rel)); err != nil {
			continue
		}
		fi, err := os.Stat(joinSource(p.SourcePath, rel))
		if err != nil {
			continue
		}
		if err := app.DB.ManifestUpsert(state.ManifestEntry{
			ProfileID: p.ID, RelPath: rel, Size: fi.Size(), ModTime: fi.ModTime().Unix(),
		}); err != nil {
			return err
		}
	}
	// Clear only the exact event versions included in the batch; newer
	// same-path events survive (§13.4).
	if err := app.DB.ClearPendingEvents(p.ID, eligible); err != nil {
		return err
	}
	if err := app.DB.MarkFastSuccess(p.ID); err != nil {
		return err
	}
	lg.Printf("fast upsert %s: success", p.ID)
	return nil
}
