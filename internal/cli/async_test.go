package cli

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/remote"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

// asyncTestApp builds an App wired to a local mock rclone remote with a full
// test database. The remote is backend-agnostic (local), so reconcile-now-style
// execution works without a Google account. Ownership validation requires a
// sidecar; tests that need real reconcile skip ownership by calling
// executeReconcileAttempt directly with a validated profile.
func asyncTestApp(t *testing.T) (*App, string) {
	t.Helper()
	bin, err := osexec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not installed")
	}
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	remoteRoot := t.TempDir()
	// This rclone build treats the `local` backend root as the process CWD
	// (config `root =` is written but not honored at runtime). Ensure rclone's
	// implicit root points at a throwaway temp dir so tests never pollute the
	// package directory; t.Chdir restores the original CWD on test cleanup.
	t.Chdir(remoteRoot)
	mustRun(t, bin, "--config", conf, "config", "create", "mock", "local")
	mustRun(t, bin, "--config", conf, "config", "update", "mock", "root="+remoteRoot)
	r := exec.NewRclone(bin, conf)

	db, err := state.Open(filepath.Join(t.TempDir(), "async.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	app := &App{
		DB: db, Rclone: r,
		Sync: sync.New(r, db), Reconciler: sync.NewReconciler(sync.New(r, db)),
		Remote: remote.New(r, db), scheduler: newSyncScheduler(),
	}
	return app, remoteRoot
}

func asyncTestProfile(t *testing.T, app *App, id string) *state.Profile {
	t.Helper()
	src := t.TempDir()
	mkTestFile(t, src, "a.md", "hello")
	p := &state.Profile{
		ID: id, ProfileUUID: id + "-uuid", Type: "generic",
		SourcePath: src, RemoteName: "mock", RemoteFolderID: "mock-folder",
		RemoteDisplayPath: "mirror-" + id, Enabled: true, MaxDelete: 100, MaxFileSize: 0,
	}
	if err := app.DB.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProfileAddRecordsDurableDebt verifies that creating a profile commits
// durable reconciliation intent without running a transfer (§27.1, §27.2).
func TestProfileAddRecordsDurableDebt(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p1")

	ss, err := app.DB.GetSyncState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ss.HasDebt() {
		t.Fatalf("expected reconciliation debt after profile create; ss=%+v", ss)
	}
	if ss.IsInitialized() {
		t.Fatal("new profile must not be initialized")
	}
	if ss.State != state.StateInitializing {
		t.Fatalf("new profile state = %s, want initializing", ss.State)
	}

	// Claim must succeed and capture the desired generation atomically.
	run, res, err := app.DB.ClaimRun(p.ID, newRunID())
	if err != nil {
		t.Fatal(err)
	}
	if res != state.ClaimOK {
		t.Fatalf("claim result = %v, want OK", res)
	}
	if run.TargetGeneration != ss.DesiredGeneration {
		t.Fatalf("run target = %d, want %d", run.TargetGeneration, ss.DesiredGeneration)
	}
	if run.Kind != state.RunKindInitial {
		t.Fatalf("run kind = %s, want initial", run.Kind)
	}

	// A second claim must be rejected while the first run is active.
	_, res2, err := app.DB.ClaimRun(p.ID, newRunID())
	if err != nil {
		t.Fatal(err)
	}
	if res2 != state.ClaimActiveRun {
		t.Fatalf("second claim result = %v, want ClaimActiveRun", res2)
	}
}

// TestDurableDebtSurvivesWorkerRestart verifies the worker orphans inherited
// running attempts and keeps generation debt for a fresh attempt (§27.3).
func TestDurableDebtSurvivesWorkerRestart(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p2")

	run, res, err := app.DB.ClaimRun(p.ID, newRunID())
	if err != nil || res != state.ClaimOK {
		t.Fatalf("claim: res=%v err=%v", res, err)
	}

	// Simulate worker death: a new worker pass calls OrphanCurrentRun.
	if err := app.DB.OrphanCurrentRun(p.ID); err != nil {
		t.Fatal(err)
	}
	ss, err := app.DB.GetSyncState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ss.CurrentRunID != nil {
		t.Fatal("current_run_id should be cleared after orphan")
	}
	if !ss.HasDebt() {
		t.Fatal("generation debt must survive worker restart")
	}
	old, err := app.DB.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != state.RunFailed || old.ErrorCode != state.WorkerInterruptedCode {
		t.Fatalf("orphaned run = %s/%s, want failed/worker_interrupted", old.Status, old.ErrorCode)
	}
	if old.ErrorClassification != state.RetryRetryable {
		t.Fatalf("worker_interrupted must be retryable, got %s", old.ErrorClassification)
	}
	if ss.ConsecutiveFailures != 0 {
		t.Fatalf("worker_interrupted must not inflate transport backoff; consecutive=%d", ss.ConsecutiveFailures)
	}

	// A fresh claim must be possible and succeed.
	run2, res2, err := app.DB.ClaimRun(p.ID, newRunID())
	if err != nil || res2 != state.ClaimOK {
		t.Fatalf("re-claim after orphan: res=%v err=%v", res2, err)
	}
	if run2.ID == run.ID {
		t.Fatal("new attempt must have a new run id")
	}
}

// TestTargetGenerationCannotExceed verifies a run cannot claim success for a
// newer generation it did not reconcile (§8.1, §27.5).
func TestTargetGenerationCannotExceed(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p3")

	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}

	// Advance desired generation while the run is active.
	if err := app.DB.RequestReconcile(p.ID); err != nil {
		t.Fatal(err)
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	if ss.DesiredGeneration <= run.TargetGeneration {
		t.Fatalf("expected desired to advance past %d", run.TargetGeneration)
	}

	// Committing success through the run's captured target leaves the newer
	// generation unsatisfied (debt remains).
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	ss, _ = app.DB.GetSyncState(p.ID)
	if !ss.HasDebt() {
		t.Fatal("newer desired generation must remain pending after run commits only through its target")
	}
	if ss.State != state.StateSyncing {
		t.Fatalf("state = %s, want syncing (initialized but newer debt pending)", ss.State)
	}
	if ss.LastSuccessGeneration == nil || *ss.LastSuccessGeneration != run.TargetGeneration {
		t.Fatalf("last_success_generation = %v, want %d", ss.LastSuccessGeneration, run.TargetGeneration)
	}
}

// TestCommitRunFailureDurableRetryGate verifies retryable failures persist
// backoff timing and terminal failures close the automatic gate (§18.2, §18.3,
// §27.9, §27.10).
func TestCommitRunFailureDurableRetryGate(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p4")

	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}
	if err := app.DB.CommitRunFailure(p.ID, run.ID, state.RunFailure{
		Code: "net", Classification: state.RetryRetryable, Message: "temporary network failure",
	}); err != nil {
		t.Fatal(err)
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	if ss.State != state.StateError {
		t.Fatalf("state = %s, want error", ss.State)
	}
	if ss.RetryClassification == nil || *ss.RetryClassification != state.RetryRetryable {
		t.Fatalf("retry classification = %v", ss.RetryClassification)
	}
	if ss.NextRetryAt == nil {
		t.Fatal("next_retry_at must be durable for retryable errors")
	}
	if ss.ConsecutiveFailures != 1 {
		t.Fatalf("consecutive failures = %d, want 1", ss.ConsecutiveFailures)
	}
	if ss.LastError == nil || *ss.LastError != "temporary network failure" {
		t.Fatalf("last error not persisted: %v", ss.LastError)
	}

	// Claim while in retry backoff must be gate-blocked.
	_, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimGateBlocked {
		t.Fatalf("claim during backoff = %v, want ClaimGateBlocked", res2)
	}

	// Explicit sync reopens the gate.
	if err := app.DB.ReopenSyncGate(p.ID); err != nil {
		t.Fatal(err)
	}
	run2, res3, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res3 != state.ClaimOK {
		t.Fatalf("claim after reopen = %v, want OK", res3)
	}

	// Terminal failure closes the gate permanently until explicit reopen.
	if err := app.DB.CommitRunFailure(p.ID, run2.ID, state.RunFailure{
		Code: "auth", Classification: state.RetryTerminal, Message: "authentication failed",
	}); err != nil {
		t.Fatal(err)
	}
	ss, _ = app.DB.GetSyncState(p.ID)
	if ss.RetryClassification == nil || *ss.RetryClassification != state.RetryTerminal {
		t.Fatalf("terminal classification not durable: %v", ss.RetryClassification)
	}
	_, res4, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res4 != state.ClaimGateBlocked {
		t.Fatalf("claim after terminal = %v, want ClaimGateBlocked", res4)
	}
}

// TestReadyOnlyAfterDebtSatisfied verifies readiness is never reported while a
// newer desired generation remains unsatisfied (§15, §27.8).
func TestReadyOnlyAfterDebtSatisfied(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p5")

	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	if ss.State != state.StateReady {
		t.Fatalf("state = %s, want ready after full debt satisfied", ss.State)
	}
	if !ss.IsInitialized() || ss.InitializedAt == nil || ss.LastSuccessAt == nil {
		t.Fatal("initialized/last_success_at must be set after first success")
	}
	if ss.LastError != nil || ss.LastErrorCode != nil || ss.RetryClassification != nil {
		t.Fatal("current error/retry state must be cleared on success")
	}
}

// TestDeleteIntentBlocksClaims verifies durable deletion intent blocks new
// claims and finalizes only after active ownership ends (§19, §27.16).
func TestDeleteIntentBlocksClaims(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p6")

	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}

	if err := app.DB.RequestProfileDeletion(p.ID); err != nil {
		t.Fatal(err)
	}
	// While the active run exists, no new claim may start; deletion waits.
	_, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimActiveRun {
		t.Fatalf("claim during deleting = %v, want ClaimActiveRun", res2)
	}
	// A deletion request after claim: deleting profiles list should include it.
	ps, err := app.DB.DeletingProfiles()
	if err != nil || len(ps) != 1 {
		t.Fatalf("deleting profiles = %d (%v)", len(ps), err)
	}

	// Worker finalization is skipped while a run is active; once the run ends,
	// finalization tombstones the profile.
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	if err := finalizeDeletions(app); err != nil {
		t.Fatal(err)
	}
	pp, _ := app.DB.GetProfile(p.ID)
	if !pp.Tombstoned {
		t.Fatal("profile must be tombstoned after active run ends and deletion finalizes")
	}
	if pp.DeletionRequestedAt != nil {
		t.Fatal("deletion_requested_at must be cleared after finalize")
	}
}

// TestExplicitSyncReopensEligibilityWithoutExecutingInCLI verifies the sync
// command path only changes durable control-plane state (§18.4, §27.10).
func TestExplicitSyncReopensEligibilityWithoutExecutingInCLI(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p7")

	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}
	if err := app.DB.CommitRunFailure(p.ID, run.ID, state.RunFailure{
		Code: "auth", Classification: state.RetryTerminal, Message: "authentication failed",
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := app.DB.GetSyncState(p.ID)

	// Simulate `knowledge-sync sync`: reopen gate + advance desired.
	if err := app.DB.ReopenSyncGate(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.RequestReconcile(p.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := app.DB.GetSyncState(p.ID)
	if after.DesiredGeneration <= before.DesiredGeneration {
		t.Fatal("sync request must advance desired generation")
	}
	if after.State == state.StateError {
		t.Fatal("sync request must reopen the gate (no longer error-blocked)")
	}

	// A claim is now possible; the CLI itself never executed a transfer.
	_, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimOK {
		t.Fatalf("claim after sync request = %v, want OK", res2)
	}
}

// TestWatcherDestructiveAdvancesDesired verifies filesystem/destructive events
// advance the durable desired generation (§20).
func TestWatcherDestructiveAdvancesDesired(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p8")

	before, _ := app.DB.GetSyncState(p.ID)
	if err := app.DB.RequestReconcile(p.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := app.DB.GetSyncState(p.ID)
	if after.DesiredGeneration != before.DesiredGeneration+1 {
		t.Fatalf("desired = %d, want %d", after.DesiredGeneration, before.DesiredGeneration+1)
	}
}

// TestMigrationBackfill verifies the migration backfills existing profiles from
// trustworthy durable success evidence and schedules reconciliation otherwise
// (§23.1, §27.18).
// TestWaitForReadyAcrossRetryable verifies the waiter crosses retryable
// attempts and returns on ready (§27.12, §27.13).
func TestWaitForReadyAcrossRetryable(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p9")

	done := make(chan error, 1)
	go func() {
		done <- waitForReady(app, p.ID)
	}()

	time.Sleep(300 * time.Millisecond)
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}
	// Retryable failure: waiter keeps waiting.
	if err := app.DB.CommitRunFailure(p.ID, run.ID, state.RunFailure{
		Code: "net", Classification: state.RetryRetryable, Message: "temp",
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("waiter returned early on retryable failure: %v", err)
	default:
	}
	// Reopen and succeed.
	if err := app.DB.ReopenSyncGate(p.ID); err != nil {
		t.Fatal(err)
	}
	run2, res2, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res2 != state.ClaimOK {
		t.Fatalf("re-claim failed: %v", res2)
	}
	if err := app.DB.CommitRunSuccess(p.ID, run2.ID, run2.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("waiter returned error on ready: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return on ready")
	}
}

// TestWaitForReadyTerminalError verifies the waiter returns nonzero on terminal
// error without deleting the profile (§27.12).
func TestWaitForReadyTerminalError(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "p10")

	done := make(chan error, 1)
	go func() {
		done <- waitForReady(app, p.ID)
	}()
	time.Sleep(300 * time.Millisecond)
	run, res, _ := app.DB.ClaimRun(p.ID, newRunID())
	if res != state.ClaimOK {
		t.Fatalf("claim failed: %v", res)
	}
	if err := app.DB.CommitRunFailure(p.ID, run.ID, state.RunFailure{
		Code: "auth", Classification: state.RetryTerminal, Message: "authentication failed",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("waiter must return nonzero on terminal error")
		}
		if !strings.Contains(err.Error(), "terminal error") {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return on terminal error")
	}
	// Profile must still exist.
	if _, err := app.DB.GetProfile(p.ID); err != nil {
		t.Fatalf("profile must be kept on remote failure: %v", err)
	}
}
