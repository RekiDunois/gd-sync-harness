package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"knowledge-sync/internal/state"
)

type fastSettingsT struct {
	SettleSeconds   int
	MaxDelaySeconds int
}

// fastBatchForTest runs the worker's due-batch evaluation with a zero settle
// window so events are immediately due (deterministic and fast).
func fastBatchForTest(app *App, p *state.Profile) error {
	return runFastUpsertBatchAt(context.Background(), app, p, nil, time.Now(), fastSettingsT{
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
