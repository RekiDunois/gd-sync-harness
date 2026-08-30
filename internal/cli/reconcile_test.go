package cli

import (
	"testing"
	"time"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

// TestReconcileNowSubmitsIntentWithoutCLIExecution verifies the manual
// reconcile command path persists durable intent and does not run a transfer in
// the CLI process (§12.1, §18.9). With no worker running, the intent remains
// durable and a later worker claim consumes it.
func TestReconcileNowSubmitsIntentWithoutCLIExecution(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "manual-intent")

	before, _ := app.DB.GetSyncState(p.ID)
	gen, err := app.DB.SubmitManualReconcile(p.ID, state.ManualReconcileIntent{AllowDeletes: 33})
	if err != nil {
		t.Fatal(err)
	}
	if gen <= before.DesiredGeneration {
		t.Fatalf("submitted generation %d must exceed %d", gen, before.DesiredGeneration)
	}
	// Pending manual metadata is recorded (durable, unconsumed).
	pm, _ := app.DB.ReadPendingManual(p.ID)
	if pm.Consumed || pm.AllowDeletes != 33 {
		t.Fatalf("pending manual = %+v", pm)
	}
	// No run row exists yet: the worker must claim it.
	runs, _ := app.DB.ListRuns(p.ID, 10)
	if len(runs) != 0 {
		t.Fatalf("no run may exist before the worker claims; got %d", len(runs))
	}
}

// TestWorkerClaimsManualIntent verifies the worker's claim consumes the manual
// override into the run's audit fields and executes it (§18.9, §11.2).
func TestWorkerClaimsManualIntent(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "manual-claim")
	gen, err := app.DB.SubmitManualReconcile(p.ID, state.ManualReconcileIntent{AllowDeletes: 44, BypassDebounce: true})
	if err != nil {
		t.Fatal(err)
	}
	run, res, err := app.DB.ClaimRun(p.ID, newRunID())
	if err != nil || res != state.ClaimOK {
		t.Fatalf("claim: res=%v err=%v", res, err)
	}
	if run.TargetGeneration != gen {
		t.Fatalf("run target = %d, want %d", run.TargetGeneration, gen)
	}
	if run.EffectiveMaxDelete != 44 || run.ManualDeleteOverride == nil || *run.ManualDeleteOverride != 44 {
		t.Fatalf("run audit = %d/%v", run.EffectiveMaxDelete, run.ManualDeleteOverride)
	}
}

// TestGenerationWaiterRequiresSubmittedGeneration verifies the generation waiter
// does not succeed on an older ready state (§14.4, §18.9).
func TestGenerationWaiterRequiresSubmittedGeneration(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "gen-waiter")
	// Drive the profile to ready at generation 1 first.
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("first claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	// Submit a manual request at a higher generation.
	gen, err := app.DB.SubmitManualReconcile(p.ID, state.ManualReconcileIntent{AllowDeletes: 0})
	if err != nil {
		t.Fatal(err)
	}
	if gen <= *mustLSG(app, p.ID) {
		t.Fatalf("submitted generation %d must exceed last success", gen)
	}

	// Simulate a waiter that only watches the DB fallback (no socket).
	done := make(chan error, 1)
	go func() {
		done <- waitForGeneration(app, p.ID, gen)
	}()
	// Even though the profile is "ready" at an older generation, the waiter must
	// not return.
	select {
	case err := <-done:
		t.Fatalf("waiter returned early: %v", err)
	case <-time.After(1 * time.Second):
	}
	// Now the worker satisfies the newer generation.
	run2, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimOK {
		t.Fatalf("second claim: %v", res2)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run2.ID, run2.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter returned error: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("waiter did not return after the newer generation succeeded")
	}
}

func mustLSG(app *App, id string) *int64 {
	ss, _ := app.DB.GetSyncState(id)
	return ss.LastSuccessGeneration
}

// TestReconcileScheduledExitsAfterSubmit verifies scheduled reconcile persists
// intent and exits without waiting (§18.9).
func TestReconcileScheduledExitsAfterSubmit(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "sched-exit")
	before, _ := app.DB.GetSyncState(p.ID)
	gen, err := app.DB.SubmitScheduledReconcile(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gen <= before.DesiredGeneration {
		t.Fatalf("scheduled generation %d must exceed %d", gen, before.DesiredGeneration)
	}
	// No run row and no pending manual metadata.
	runs, _ := app.DB.ListRuns(p.ID, 10)
	if len(runs) != 0 {
		t.Fatalf("scheduled submit must not create a run: %d", len(runs))
	}
	pm, _ := app.DB.ReadPendingManual(p.ID)
	if !pm.Consumed {
		t.Fatalf("scheduled submit must not set manual metadata: %+v", pm)
	}
}

// TestObserverStreamGeneration verifies live snapshots reach the generation
// waiter via the socket (§14.3).
func TestObserverStreamGeneration(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "gen-stream")
	srv := startLiveServerForTest(t, app)
	app.LiveServer = srv
	gen, err := app.DB.SubmitManualReconcile(p.ID, state.ManualReconcileIntent{AllowDeletes: 0})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- waitForGeneration(app, p.ID, gen) }()
	time.Sleep(300 * time.Millisecond)

	// The worker claims and succeeds; publish the terminal snapshot via the
	// durable refresh so the socket observer sees last_success_generation.
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	app.liveReader().Refresh(p.ID)
	app.liveReader().SetActivityProvider(func(id string) *live.ActivityS { return nil })
	app.LiveServer.PublishDurableRefresh(p.ID)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("generation waiter error: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("generation waiter did not return")
	}
}
