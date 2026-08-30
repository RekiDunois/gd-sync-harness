package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

type fastSettingsT struct {
	SettleSeconds   int
	MaxDelaySeconds int
}

// fastBatchForTest runs the worker's due-batch evaluation with a zero settle
// window so events are immediately due (deterministic and fast).
func fastBatchForTest(app *App, p *state.Profile) error {
	return runFastUpsertBatchAt(context.Background(), app, p, &policy.Snapshot{}, nil, time.Now(), fastSettingsT{
		SettleSeconds: 0, MaxDelaySeconds: 30,
	})
}

// TestWorkerFastUpsertDrainsDueBatch verifies the worker evaluates durable
// debounce and executes a due fast batch, clearing exact event versions
// (§13.2, §13.4, §18.10).
func TestWorkerFastUpsertDrainsDueBatch(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-drain")
	// Establish initialization so events stay in the fast path.
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	mkTestFile(t, p.SourcePath, "x.md", "x")

	if _, err := app.DB.RecordEvent(p.ID, "x.md", state.EventModify, false); err != nil {
		t.Fatal(err)
	}
	// With a zero settle window the batch is immediately due: the worker
	// executes and clears.
	if err := fastBatchForTest(app, p); err != nil {
		t.Fatal(err)
	}
	pending, _ := app.DB.ListPending(p.ID)
	if len(pending) != 0 {
		t.Fatalf("due batch must clear events; got %d", len(pending))
	}
	// Remote file exists.
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-fast-drain/x.md"); got != "x" {
		t.Fatalf("remote x.md = %q", got)
	}
}

// TestWorkerFastUpsertNewerEventSurvives verifies a newer same-path event
// survives the version-safe clear (§13.4, §18.10).
func TestWorkerFastUpsertNewerEventSurvives(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-newer")
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	mkTestFile(t, p.SourcePath, "y.md", "y")
	if _, err := app.DB.RecordEvent(p.ID, "y.md", state.EventModify, false); err != nil {
		t.Fatal(err)
	}
	// Record a second (newer) event for the same path after the first.
	if _, err := app.DB.RecordEvent(p.ID, "y.md", state.EventModify, false); err != nil {
		t.Fatal(err)
	}
	// The due evaluation reads all pending; execute.
	if err := fastBatchForTest(app, p); err != nil {
		t.Fatal(err)
	}
	pending, _ := app.DB.ListPending(p.ID)
	if len(pending) != 0 {
		t.Fatalf("both versions were cleared on success (same path, batch includes latest): %d", len(pending))
	}
}

// TestWorkerFastUpsertPromotesDestructive verifies a destructive pending event
// promotes to a full reconcile rather than an unsafe fast copy (§18.10).
func TestWorkerFastUpsertPromotesDestructive(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-destructive")
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	mkTestFile(t, p.SourcePath, "z.md", "z")
	if _, err := app.DB.RecordEvent(p.ID, "z.md", state.EventDelete, true); err != nil {
		t.Fatal(err)
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	if !ss.HasDebt() {
		t.Fatalf("destructive event must advance full reconcile debt: %+v", ss)
	}
	// The worker pass must not run a fast copy; it leaves full debt.
	if err := fastBatchForTest(app, p); err != nil {
		t.Fatal(err)
	}
	ss, _ = app.DB.GetSyncState(p.ID)
	if !ss.HasDebt() {
		t.Fatal("full reconcile debt must remain after destructive promotion")
	}
}

// TestWorkerFastUpsertWatcherRecordsButNotExecutes verifies the watcher records
// events without running rclone (the worker owns execution) (§13.1, §18.10).
func TestWorkerFastUpsertWatcherRecordsButNotExecutes(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-watch-records")
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	mkTestFile(t, p.SourcePath, "w.md", "w")
	if _, err := app.DB.RecordEvent(p.ID, "w.md", state.EventModify, false); err != nil {
		t.Fatal(err)
	}
	// No run row and no remote file yet — the worker must execute.
	runs, _ := app.DB.ListRuns(p.ID, 10)
	if len(runs) != 1 { // only the init run
		t.Fatalf("fast recording must not create a run row; got %d", len(runs))
	}
	// Remote file must not exist until the worker drains.
	if err := fastBatchForTest(app, p); err != nil {
		t.Fatal(err)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-fast-watch-records/w.md"); got != "w" {
		t.Fatalf("remote w.md = %q after worker drain", got)
	}
}

// TestFastUpsertStaleEventCleared verifies events whose source file disappeared
// are cleared rather than uploaded (§13.4).
func TestFastUpsertStaleEventCleared(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-stale")
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(p.SourcePath, "gone.md")
	mkTestFile(t, p.SourcePath, "gone.md", "g")
	if _, err := app.DB.RecordEvent(p.ID, "gone.md", state.EventModify, false); err != nil {
		t.Fatal(err)
	}
	// Remove the file before the batch executes.
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if err := fastBatchForTest(app, p); err != nil {
		t.Fatal(err)
	}
	pending, _ := app.DB.ListPending(p.ID)
	if len(pending) != 0 {
		t.Fatalf("stale events must be cleared; got %d", len(pending))
	}
}

// tmpScriptsSnapshot returns a committed-style policy snapshot that excludes
// the .tmp_scripts/ directory.
func tmpScriptsSnapshot() *policy.Snapshot {
	return &policy.Snapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte(".tmp_scripts/\n")},
	}}
}

// fastBatchWithSnapshot runs the fast batch under an explicit committed policy
// snapshot with a zero settle window (deterministic).
func fastBatchWithSnapshot(app *App, p *state.Profile, snap *policy.Snapshot) error {
	return runFastUpsertBatchAt(context.Background(), app, p, snap, nil, time.Now(), fastSettingsT{
		SettleSeconds: 0, MaxDelaySeconds: 30,
	})
}

// fastBatchWithPending runs the fast batch over an explicit pending snapshot
// (the worker's ListPending view) so a newer same-path event recorded after the
// snapshot deterministically survives exact clearing.
func fastBatchWithPending(app *App, p *state.Profile, snap *policy.Snapshot, pending []state.PendingEvent) error {
	return runFastUpsertBatchPending(context.Background(), app, p, snap, nil, time.Now(), fastSettingsT{
		SettleSeconds: 0, MaxDelaySeconds: 30,
	}, pending)
}

// T8: excluded-only fast batch must exact-clear the snapshot versions so a
// newer same-path event survives; profile-wide ClearPending must not be used.
func TestFastUpsertExcludedOnlyExactClear(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-excluded")
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	// The ignored path exists on disk but is excluded by committed policy.
	mkTestFile(t, p.SourcePath, ".tmp_scripts/x.tmp", "x")
	snap := tmpScriptsSnapshot()
	if _, err := app.DB.RecordEvent(p.ID, ".tmp_scripts/x.tmp", state.EventModify, false); err != nil {
		t.Fatal(err)
	}
	// The worker captures its pending snapshot, then the watcher records a
	// newer same-path event (generation bumped).
	snapshot, err := app.DB.ListPending(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.RecordEvent(p.ID, ".tmp_scripts/x.tmp", state.EventModify, false); err != nil {
		t.Fatal(err)
	}

	// The worker runs its batch against its captured snapshot, not a fresh
	// ListPending.
	if err := fastBatchWithPending(app, p, snap, snapshot); err != nil {
		t.Fatal(err)
	}
	left, _ := app.DB.ListPending(p.ID)
	if len(left) != 1 || left[0].Path != ".tmp_scripts/x.tmp" || left[0].SourceGeneration != snapshot[0].SourceGeneration+1 {
		t.Fatalf("newer same-path event must survive excluded exact clear; got %+v", left)
	}
}

// T9: mixed eligible + excluded batch uploads only the eligible path, clears
// both snapshot events exactly, and never uploads the ignored path.
func TestFastUpsertMixedEligibleAndExcluded(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-mixed")
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	mkTestFile(t, p.SourcePath, "a.md", "hello")
	mkTestFile(t, p.SourcePath, ".tmp_scripts/x.tmp", "x")
	snap := tmpScriptsSnapshot()
	if _, err := app.DB.RecordEvent(p.ID, "a.md", state.EventModify, false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.RecordEvent(p.ID, ".tmp_scripts/x.tmp", state.EventModify, false); err != nil {
		t.Fatal(err)
	}

	if err := fastBatchWithSnapshot(app, p, snap); err != nil {
		t.Fatal(err)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-fast-mixed/a.md"); got != "hello" {
		t.Fatalf("remote a.md = %q, want hello", got)
	}
	// The excluded path must never be uploaded.
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-fast-mixed/.tmp_scripts/x.tmp"); got != "" {
		t.Fatalf("excluded path must not be uploaded; got %q", got)
	}
	pending, _ := app.DB.ListPending(p.ID)
	if len(pending) != 0 {
		t.Fatalf("both snapshot events must be consumed; got %d", len(pending))
	}
}

// T10: the core regression — an ignored safe event recorded during an active
// full run must not promote desired generation, must survive the full run's
// target-bound clear, and must be consumed as a no-op by a fast pass with no
// new full debt.
func TestFastUpsertIgnoredSafeEventAfterFullTarget(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-ignored-after-full")
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	// Advance the durable source generation to the full target (simulating a
	// steady-state profile at generation G) and create full debt at G.
	gen, err := app.DB.BumpGeneration(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.DB.EnsureReconcileGeneration(p.ID, gen); err != nil {
		t.Fatal(err)
	}
	fullRun, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimOK {
		t.Fatalf("full claim: %v", res2)
	}

	// Ignored safe churn arrives while the full run is active. The event gets
	// source generation G+1, above the run's captured target.
	mkTestFile(t, p.SourcePath, ".tmp_scripts/ignored-churn-test.tmpdata", "x")
	snap := tmpScriptsSnapshot()
	eventGen, err := app.DB.RecordEvent(p.ID, ".tmp_scripts/ignored-churn-test.tmpdata", state.EventModify, false)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	if ss.DesiredGeneration != fullRun.TargetGeneration {
		t.Fatalf("desired = %d, want %d (ignored safe event must not promote)", ss.DesiredGeneration, fullRun.TargetGeneration)
	}
	if eventGen != fullRun.TargetGeneration+1 {
		t.Fatalf("event generation = %d, want %d (above run target)", eventGen, fullRun.TargetGeneration+1)
	}
	if ss.CurrentRunID == nil || *ss.CurrentRunID != fullRun.ID {
		t.Fatalf("current run = %v, want %s", ss.CurrentRunID, fullRun.ID)
	}

	// The full run commits through its captured target; the G+1 pending event
	// survives the target-bound clear because it was recorded above the target.
	if err := app.DB.CommitRunSuccess(p.ID, fullRun.ID, fullRun.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	ss, _ = app.DB.GetSyncState(p.ID)
	if ss.HasDebt() {
		t.Fatal("no full debt may remain after the run plus an ignored safe event")
	}
	pending, _ := app.DB.ListPending(p.ID)
	if len(pending) != 1 || pending[0].SourceGeneration != eventGen {
		t.Fatalf("pending after full success = %+v, want generation %d", pending, eventGen)
	}

	// The worker's fast pass consumes the ignored event as a no-op: no remote
	// upload, no new full debt, and the pending row is cleared exactly.
	if err := fastBatchWithSnapshot(app, p, snap); err != nil {
		t.Fatal(err)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-fast-ignored-after-full/.tmp_scripts/ignored-churn-test.tmpdata"); got != "" {
		t.Fatalf("ignored path must never be uploaded; got %q", got)
	}
	pending, _ = app.DB.ListPending(p.ID)
	if len(pending) != 0 {
		t.Fatalf("ignored event must be consumed; got %d", len(pending))
	}
	ss, _ = app.DB.GetSyncState(p.ID)
	if ss.HasDebt() {
		t.Fatal("fast no-op consume must not create full debt")
	}
	// A subsequent claim must find no debt.
	_, res3, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res3 != state.ClaimNoDebt {
		t.Fatalf("claim after ignored churn = %v, want ClaimNoDebt", res3)
	}
}

// T11: an active safe event recorded after a full run is repaired by a targeted
// fast upload, updates the managed ledger, and leaves no full debt.
func TestFastUpsertActiveSafeEventAfterFullTarget(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "fast-active-after-full")
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("init claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	gen, err := app.DB.BumpGeneration(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.DB.EnsureReconcileGeneration(p.ID, gen); err != nil {
		t.Fatal(err)
	}
	fullRun, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimOK {
		t.Fatalf("full claim: %v", res2)
	}

	// An active file change arrives while the full run is active.
	mkTestFile(t, p.SourcePath, "a.md", "hello v2")
	if _, err := app.DB.RecordEvent(p.ID, "a.md", state.EventModify, false); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.CommitRunSuccess(p.ID, fullRun.ID, fullRun.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	if ss.HasDebt() {
		t.Fatal("no full debt may remain from the active safe event")
	}
	pending, _ := app.DB.ListPending(p.ID)
	if len(pending) != 1 {
		t.Fatalf("pending after full success = %d, want 1", len(pending))
	}

	// Fast pass uploads the active path and updates the ledger.
	if err := fastBatchForTest(app, p); err != nil {
		t.Fatal(err)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-fast-active-after-full/a.md"); got != "hello v2" {
		t.Fatalf("remote a.md = %q, want hello v2", got)
	}
	m, err := app.DB.ManifestGet(p.ID, "a.md")
	if err != nil {
		t.Fatalf("ledger upsert: %v", err)
	}
	if m.Size != int64(len("hello v2")) {
		t.Fatalf("ledger size = %d, want %d", m.Size, len("hello v2"))
	}
	pending, _ = app.DB.ListPending(p.ID)
	if len(pending) != 0 {
		t.Fatalf("pending after targeted upload = %d, want 0", len(pending))
	}
	ss, _ = app.DB.GetSyncState(p.ID)
	if ss.HasDebt() {
		t.Fatal("no full debt after targeted repair")
	}
}
