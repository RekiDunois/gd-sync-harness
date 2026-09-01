package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"knowledge-sync/internal/policy"
)

func phase2Profile(t *testing.T, db *DB, id string) *Profile {
	t.Helper()
	p := &Profile{
		ID: id, ProfileUUID: id + "-uuid", Type: "generic",
		SourcePath: filepath.Join(t.TempDir(), "source"), RemoteName: "example-remote",
		RemoteFolderID: "example-folder", RemoteDisplayPath: "mirror/" + id,
		Enabled: true, MaxDelete: 7,
	}
	if err := db.CreateProfileWithPolicy(p, &policy.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDestructiveIntentDebounceAndPromotion(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "debounce")
	if err := db.EnsureSyncState(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordEvent(p.ID, "a.md", EventModify, false); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := db.ScheduleDestructiveReconcile(p.ID, 3, now); err != nil {
		t.Fatal(err)
	}
	ss, err := db.GetSyncState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ss.ReconcileNotBeforeAt == nil || ss.ReconcileDeadlineAt == nil {
		t.Fatalf("debounce window = %+v", ss)
	}
	if _, result, err := db.ClaimRun(p.ID, "deferred-run"); err != nil || result != ClaimDeferred {
		t.Fatalf("claim during debounce: result=%v err=%v, want ClaimDeferred", result, err)
	}

	if err := db.PromoteToFullReconcile(p.ID, 4, now); err != nil {
		t.Fatal(err)
	}
	pending, err := db.ListPending(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("promotion must collapse detailed events: %+v", pending)
	}
	ss, _ = db.GetSyncState(p.ID)
	if ss.DesiredGeneration != 4 || !ss.HasDebt() {
		t.Fatalf("state after promotion = %+v", ss)
	}
}

func TestProfileEnableUpdateAndDeletionLifecycle(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "lifecycle")
	other := phase2Profile(t, db, "other")
	other.Tombstoned = true
	if err := db.UpdateProfileFields(other); err != nil {
		t.Fatal(err)
	}
	if err := db.TombstoneProfile(other.ID); err != nil {
		t.Fatal(err)
	}

	if err := db.SetProfileEnabled(p.ID, false); err != nil {
		t.Fatal(err)
	}
	cs, err := db.GetCompilerState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cs.DerivedState != "blocked_disabled" {
		t.Fatalf("disabled derived state = %q", cs.DerivedState)
	}
	if err := db.SetProfileEnabled(p.ID, true); err != nil {
		t.Fatal(err)
	}
	cs, _ = db.GetCompilerState(p.ID)
	if cs.DerivedState != "pending" {
		t.Fatalf("re-enabled derived state = %q", cs.DerivedState)
	}

	p.SourcePath = filepath.Join(t.TempDir(), "updated")
	p.RemoteDisplayPath = "updated-mirror"
	p.MaxDelete = 9
	if err := db.UpdateProfileFields(p); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetProfile(p.ID)
	if err != nil || got.SourcePath != p.SourcePath || got.MaxDelete != 9 {
		t.Fatalf("updated profile = %+v, err=%v", got, err)
	}

	if err := db.RequestProfileDeletion(p.ID); err != nil {
		t.Fatal(err)
	}
	deleting, err := db.DeletingProfiles()
	if err != nil || len(deleting) != 1 || deleting[0].ID != p.ID {
		t.Fatalf("deleting profiles = %+v, err=%v", deleting, err)
	}
	if err := db.CancelProfileDeletion(p.ID); err != nil {
		t.Fatal(err)
	}
	deleting, _ = db.DeletingProfiles()
	if len(deleting) != 0 {
		t.Fatalf("cancelled deletion remains queued: %+v", deleting)
	}
	if err := db.SetProfileEnabled("missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing enable error = %v", err)
	}

	active, err := db.ActiveProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != p.ID {
		t.Fatalf("active profiles = %+v", active)
	}
}

func TestSyncRunTelemetryAndOrphanListing(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "telemetry")
	run, result, err := db.ClaimRun(p.ID, "telemetry-run")
	if err != nil || result != ClaimOK {
		t.Fatalf("claim: result=%v err=%v", result, err)
	}
	for _, phase := range []string{PhaseScanning, PhaseUploading} {
		if err := db.UpdateRunPhase(p.ID, run.ID, phase); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateRunFilesDiscovered(p.ID, run.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateRunHeartbeat(p.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	item := "a.md"
	stats := ProgressSnapshot{
		FilesCompleted: 2, BytesCompleted: 20, BytesTotal: 30,
		ChecksCompleted: 1, ChecksTotal: 3, ItemsListed: 4, ErrorsCount: 1,
		SpeedBytesPerSecond: 12, CurrentItem: &item, CurrentItemBytes: 4,
		CurrentItemSize: 10, ActiveTransfers: 1,
	}
	if err := db.UpdateRunStats(p.ID, run.ID, stats, false); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateRunStats(p.ID, run.ID, stats, true); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseUploading || got.FilesDiscovered != 3 || got.FilesCompleted != 2 || got.CurrentItem == nil || *got.CurrentItem != item {
		t.Fatalf("run telemetry = %+v", got)
	}
	runs, err := db.ListRuns(p.ID, 0)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err=%v", runs, err)
	}

	if err := db.OrphanRuns(p.ID, WorkerInterruptedCode); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetRun(run.ID)
	if got.Status != RunFailed || got.ErrorCode != WorkerInterruptedCode {
		t.Fatalf("orphaned run = %+v", got)
	}

	p2 := phase2Profile(t, db, "orphan-current")
	run2, result, err := db.ClaimRun(p2.ID, "orphan-current-run")
	if err != nil || result != ClaimOK {
		t.Fatalf("second claim: result=%v err=%v", result, err)
	}
	if err := db.OrphanCurrentRun(p2.ID); err != nil {
		t.Fatal(err)
	}
	state2, err := db.GetSyncState(p2.ID)
	if err != nil || state2.CurrentRunID != nil {
		t.Fatalf("state after current-run orphan = %+v, err=%v", state2, err)
	}
	got, err = db.GetRun(run2.ID)
	if err != nil || got.ErrorCode != WorkerInterruptedCode {
		t.Fatalf("current-run orphan = %+v, err=%v", got, err)
	}
}

func TestLimitedRetryClassificationEventuallyTerminates(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "limited-retry")
	for attempt := 1; attempt <= 4; attempt++ {
		run, result, err := db.ClaimRun(p.ID, "limited-run-"+string(rune('0'+attempt)))
		if err != nil || result != ClaimOK {
			t.Fatalf("attempt %d claim: result=%v err=%v", attempt, result, err)
		}
		if err := db.CommitRunFailure(p.ID, run.ID, RunFailure{
			Code: "unknown", Classification: RetryRetryableLimited, Message: "unclassified",
		}); err != nil {
			t.Fatal(err)
		}
		if attempt < 4 {
			// Move the deterministic test clock past the limited retry schedule.
			if _, err := db.Exec(`UPDATE profile_sync_state SET next_retry_at = ? WHERE profile_id = ?`, time.Now().UTC().Add(-time.Second).Format(timeFmt), p.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	ss, err := db.GetSyncState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ss.RetryClassification == nil || *ss.RetryClassification != RetryTerminal || ss.LastErrorCode == nil || *ss.LastErrorCode != "unknown_error_limit" {
		t.Fatalf("terminal limited retry state = %+v", ss)
	}
}

func TestCompilerFailureAndDerivedRecoveryTransitions(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "compiler-state")
	binding := DerivedBindingFingerprint(p.RemoteName, p.RemoteFolderID)
	if err := db.StartCompilerRun(p.ID, "compiler-run", "generation-1", "v1", 1, "policy", "contract"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishCompilerRun(p.ID, "compiler-run", "generation-1", "snapshot", "policy", "contract", binding, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkCompilerClean(p.ID, "clean-op"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishCompilerClean(p.ID); err != nil {
		t.Fatal(err)
	}

	if err := db.StartCompilerRun(p.ID, "failed-compiler-run", "generation-2", "v1", 1, "policy", "contract"); err != nil {
		t.Fatal(err)
	}
	if err := db.FailCompilerRun(p.ID, "failed-compiler-run", "compile failed"); err != nil {
		t.Fatal(err)
	}

	derived, claimed, err := db.ClaimDerivedRun(p.ID, "derived-run", binding)
	if err != nil || !claimed || derived == nil {
		t.Fatalf("derived claim: run=%+v claimed=%v err=%v", derived, claimed, err)
	}
	if err := db.UpdateDerivedRunPhase(p.ID, derived.ID, "check"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDerivedRunFailure(p.ID, derived.ID, derived.TargetKey, "temporary", RetryRetryable, "remote unavailable"); err != nil {
		t.Fatal(err)
	}
	cs, err := db.GetCompilerState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cs.DerivedState != "failed" || cs.DerivedRetryClassification == nil || *cs.DerivedRetryClassification != RetryRetryable {
		t.Fatalf("derived failure state = %+v", cs)
	}

	if err := db.StartCompilerRun(p.ID, "compiler-run-2", "generation-2", "v1", 1, "policy", "contract"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishCompilerRun(p.ID, "compiler-run-2", "generation-2", "snapshot-2", "policy", "contract", binding, 1, 0); err != nil {
		t.Fatal(err)
	}
	derived, claimed, err = db.ClaimDerivedRun(p.ID, "derived-run-2", binding)
	if err != nil || !claimed {
		t.Fatalf("second derived claim: run=%+v claimed=%v err=%v", derived, claimed, err)
	}
	if err := db.RecoverDerivedRuns(); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT status, error_code FROM compiler_derived_runs WHERE id = ?`, "derived-run-2")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("recovered derived run not found")
	}
	var status, code string
	if err := rows.Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != DerivedRunInterrupted || code != WorkerInterruptedCode {
		t.Fatalf("recovered derived run = %s/%s", status, code)
	}
}

func TestValidateIDRejectsUnsafeNames(t *testing.T) {
	for _, test := range []struct {
		id  string
		bad bool
	}{
		{id: "valid-id"}, {id: "a1"}, {id: "", bad: true},
		{id: "-starts-with-dash", bad: true}, {id: "Upper", bad: true},
		{id: "has space", bad: true},
	} {
		err := ValidateID(test.id)
		if test.bad && err == nil {
			t.Errorf("ValidateID(%q) accepted unsafe value", test.id)
		}
		if !test.bad && err != nil {
			t.Errorf("ValidateID(%q) = %v", test.id, err)
		}
	}
}

func TestAcquireRemoteLeaseCancellationReleasesWaitingRow(t *testing.T) {
	db := openTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	if err := db.AcquireRemoteLease(context.Background(), "example-remote", 1, 1, 1, "running"); err != nil {
		t.Fatal(err)
	}
	cancel()
	err := db.AcquireRemoteLease(ctx, "example-remote", 1, 1, 1, "waiting")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lease cancellation error = %v", err)
	}
	if err := db.ReleaseRemoteLease("running"); err != nil {
		t.Fatal(err)
	}
	if err := db.RenewRemoteLease("running"); err == nil {
		t.Fatal("renewing a released lease must fail")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM remote_operation_leases`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cancelled lease row remains: %d", count)
	}
}
