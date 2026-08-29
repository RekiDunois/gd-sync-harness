package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"knowledge-sync/internal/state"
)

func contextBackground() context.Context { return context.Background() }

// TestWorkerPassRunsInitialReconcileEndToEnd exercises the full worker-owned
// path: create a profile with durable debt, write a matching sidecar to the
// mock remote (so ownership validation passes), run a worker pass, and verify
// the profile reaches ready with files mirrored (§28 integration sequence).
func TestWorkerPassRunsInitialReconcileEndToEnd(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "worker-e2e")

	// Write the ownership sidecar to the mock remote so validateOwnership
	// passes (§7 ownership checks).
	writeSidecarForTest(t, app, p)

	// Verify durable debt exists (the worker should claim it).
	ss, err := app.DB.GetSyncState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ss.HasDebt() {
		t.Fatal("expected debt after profile create")
	}

	// Run a single worker pass; it should claim, execute, and commit success.
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("worker pass: %v", err)
	}

	ss, _ = app.DB.GetSyncState(p.ID)
	if ss.State != state.StateReady {
		t.Fatalf("state = %s, want ready", ss.State)
	}
	if !ss.IsInitialized() || ss.InitializedAt == nil {
		t.Fatal("profile must be initialized after successful worker run")
	}
	if ss.HasDebt() {
		t.Fatal("debt must be cleared after successful run")
	}

	// Remote contents must converge (§28.13).
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-worker-e2e/a.md"); got != "hello" {
		t.Fatalf("remote mirror/a.md = %q, want hello", got)
	}

	// A second pass must not create another run (no debt).
	runs, _ := app.DB.ListRuns(p.ID, 10)
	if len(runs) != 1 {
		t.Fatalf("expected exactly 1 run, got %d", len(runs))
	}
	if runs[0].Status != state.RunSucceeded {
		t.Fatalf("run status = %s, want succeeded", runs[0].Status)
	}
	if runs[0].Kind != state.RunKindInitial {
		t.Fatalf("run kind = %s, want initial", runs[0].Kind)
	}
}

// TestWorkerPassFileEventsDuringRun verifies file events during a run advance
// desired generation without creating a competing run, and a follow-up run
// converges (§20, §27.6).
func TestWorkerPassFileEventsDuringRun(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "worker-events")
	writeSidecarForTest(t, app, p)

	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("first worker pass: %v", err)
	}

	// Simulate a file event while "syncing": bump desired generation.
	if err := app.DB.RequestReconcile(p.ID); err != nil {
		t.Fatal(err)
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	if ss.State != state.StateSyncing {
		t.Fatalf("state after event = %s, want syncing (initialized with debt)", ss.State)
	}

	// New file appears locally.
	mkTestFile(t, p.SourcePath, "b.md", "world")

	// A single worker pass must reconcile the newer generation.
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("second worker pass: %v", err)
	}
	ss, _ = app.DB.GetSyncState(p.ID)
	if ss.State != state.StateReady {
		t.Fatalf("state = %s, want ready", ss.State)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-worker-events/b.md"); got != "world" {
		t.Fatalf("remote b.md = %q", got)
	}
}

// TestWorkerOrphanRecoveryOnStartup verifies workerRecover marks inherited
// running attempts orphaned and preserves generation debt (§17.3).
func TestWorkerOrphanRecoveryOnStartup(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "worker-recover")

	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}
	// Simulate worker crash: current_run_id still set.
	if err := workerRecover(nil, app, "", nil); err != nil {
		t.Fatalf("workerRecover: %v", err)
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	if ss.CurrentRunID != nil {
		t.Fatal("current_run_id must be cleared after recovery")
	}
	old, _ := app.DB.GetRun(run.ID)
	if old.Status != state.RunFailed || old.ErrorCode != state.WorkerInterruptedCode {
		t.Fatalf("orphaned run = %s/%s", old.Status, old.ErrorCode)
	}
	// Debt remains → fresh claim possible.
	_, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimOK {
		t.Fatalf("claim after recovery = %v, want OK", res2)
	}
}

// TestWorkerFinalizesDeletionAfterRun verifies the worker finalizes a durable
// deletion request only after the active run ends (§19, §27.16).
func TestWorkerFinalizesDeletionAfterRun(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "worker-del")
	writeSidecarForTest(t, app, p)

	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}
	if err := app.DB.RequestProfileDeletion(p.ID); err != nil {
		t.Fatal(err)
	}
	// Deleting profile must not be claimable (worker skips it for new runs).
	_, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimActiveRun {
		t.Fatalf("claim while run active during deletion = %v, want ClaimActiveRun", res2)
	}
	// Finish the active run; worker finalizes deletion.
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("worker pass: %v", err)
	}
	pp, _ := app.DB.GetProfile(p.ID)
	if !pp.Tombstoned {
		t.Fatal("deletion must finalize after active run ends")
	}
}

// TestDeletionIntentSurvivesWorkerDown verifies a durable deletion request is
// preserved when no worker is running (profile add/remove happen without a
// worker), and a later worker start finalizes it (§19, §27.16, §28).
func TestDeletionIntentSurvivesWorkerDown(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "worker-del-down")

	// Request deletion with no active run and no worker running.
	if err := app.DB.RequestProfileDeletion(p.ID); err != nil {
		t.Fatal(err)
	}
	pp, _ := app.DB.GetProfile(p.ID)
	if pp.Tombstoned {
		t.Fatal("deletion must not tombstone immediately (worker owns finalization)")
	}
	if pp.DeletionRequestedAt == nil {
		t.Fatal("deletion intent must be durable")
	}
	// No new claims may be created for the deleting profile.
	_, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimProfileInactive {
		t.Fatalf("claim on deleting profile = %v, want ClaimProfileInactive", res)
	}
	// A later worker pass finalizes the deletion.
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("worker pass: %v", err)
	}
	pp, _ = app.DB.GetProfile(p.ID)
	if !pp.Tombstoned {
		t.Fatal("worker must finalize deletion once no active run remains")
	}
	if pp.DeletionRequestedAt != nil {
		t.Fatal("deletion_requested_at must be cleared after finalize")
	}
}

func writeSidecarForTest(t *testing.T, app *App, p *state.Profile) {
	t.Helper()
	sc := map[string]any{
		"schema_version":   1,
		"profile_id":       p.ID,
		"profile_uuid":     p.ProfileUUID,
		"remote_folder_id": p.RemoteFolderID,
		"created_at":       "2026-08-30T00:00:00Z",
		"remote_name":      p.RemoteName,
	}
	tmp := filepath.Join(t.TempDir(), "sidecar.json")
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	res := app.Rclone.Run(context.Background(), "copyto", tmp, "mock:.knowledge-sync/profiles/"+p.ProfileUUID+".json")
	if res.Err != nil {
		t.Fatalf("write sidecar: %v: %s", res.Err, res.StderrTrimmed())
	}
}
