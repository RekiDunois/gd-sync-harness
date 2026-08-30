package state

import (
	"testing"
)

// mkEventProfile creates a profile for event-semantics tests.
func mkEventProfile(t *testing.T, db *DB, id string) {
	t.Helper()
	p := &Profile{
		ID: id, ProfileUUID: "ev-" + id, Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 100, MaxFileSize: 0,
	}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
}

// claimAndSucceed drives a profile to initialized/ready with a committed
// success generation, mirroring the worker's claim+commit path.
func claimAndSucceed(t *testing.T, db *DB, id string) int64 {
	t.Helper()
	run, res, err := db.ClaimRun(id, newTestRunID())
	if err != nil || res != ClaimOK {
		t.Fatalf("claim %s: res=%v err=%v", id, res, err)
	}
	if err := db.CommitRunSuccess(id, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	return run.TargetGeneration
}

// newTestRunID returns a unique run id for tests.
func newTestRunID() string {
	return "run-test-" + Now().Format("150405.000000000")
}

// T1: initialized + idle + safe modify keeps source generation separate from
// desired and preserves durable path evidence without full debt.
func TestRecordEventSafeModifyIdleInitialized(t *testing.T) {
	db := openTestDB(t)
	mkEventProfile(t, db, "ev-t1")
	last := claimAndSucceed(t, db, "ev-t1")

	gen, err := db.RecordEvent("ev-t1", "a.md", EventModify, false)
	if err != nil {
		t.Fatal(err)
	}
	if gen != last+1 {
		t.Fatalf("source generation = %d, want %d", gen, last+1)
	}
	ss, _ := db.GetSyncState("ev-t1")
	if ss.DesiredGeneration != last {
		t.Fatalf("desired generation = %d, want %d (safe event must not promote)", ss.DesiredGeneration, last)
	}
	if ss.HasDebt() {
		t.Fatal("safe modify must not create full debt")
	}
	pending, _ := db.ListPending("ev-t1")
	if len(pending) != 1 || pending[0].Path != "a.md" || pending[0].SourceGeneration != gen {
		t.Fatalf("pending = %+v", pending)
	}
}

// T2: active full run + safe modify must not promote, must preserve prior
// pending evidence, and must leave the current run untouched.
func TestRecordEventSafeModifyDuringActiveRun(t *testing.T) {
	db := openTestDB(t)
	mkEventProfile(t, db, "ev-t2")
	claimAndSucceed(t, db, "ev-t2")

	// Establish durable full debt so a run is claimable.
	if err := db.EnsureReconcileGeneration("ev-t2", 105); err != nil {
		t.Fatal(err)
	}
	// Record a prior pending event so the run also has existing evidence.
	if _, err := db.RecordEvent("ev-t2", "prior.md", EventModify, false); err != nil {
		t.Fatal(err)
	}
	run, res, err := db.ClaimRun("ev-t2", newTestRunID())
	if err != nil || res != ClaimOK {
		t.Fatalf("claim for active run: res=%v err=%v", res, err)
	}

	gen, err := db.RecordEvent("ev-t2", "a.md", EventModify, false)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := db.GetSyncState("ev-t2")
	if ss.DesiredGeneration != 105 {
		t.Fatalf("desired generation = %d, want 105 (active run must not be raised)", ss.DesiredGeneration)
	}
	if ss.CurrentRunID == nil || *ss.CurrentRunID != run.ID {
		t.Fatalf("current run = %v, want %s", ss.CurrentRunID, run.ID)
	}
	pending, _ := db.ListPending("ev-t2")
	if len(pending) != 2 {
		t.Fatalf("prior pending evidence must survive; got %d events", len(pending))
	}
	byPath := map[string]PendingEvent{}
	for _, e := range pending {
		byPath[e.Path] = e
	}
	if e, ok := byPath["a.md"]; !ok || e.SourceGeneration != gen {
		t.Fatalf("a.md pending = %+v, want generation %d", byPath["a.md"], gen)
	}
	if _, ok := byPath["prior.md"]; !ok {
		t.Fatal("prior.md pending evidence must not be deleted by a safe event")
	}
}

// T3: existing full debt + safe modify keeps the debt watermark and preserves
// the new path evidence.
func TestRecordEventSafeModifyWithExistingFullDebt(t *testing.T) {
	db := openTestDB(t)
	mkEventProfile(t, db, "ev-t3")
	claimAndSucceed(t, db, "ev-t3")

	// Establish a destructive full debt at generation 105.
	if err := db.EnsureReconcileGeneration("ev-t3", 105); err != nil {
		t.Fatal(err)
	}
	ss0, _ := db.GetSyncState("ev-t3")
	if ss0.DesiredGeneration != 105 {
		t.Fatalf("setup desired = %d, want 105", ss0.DesiredGeneration)
	}

	gen, err := db.RecordEvent("ev-t3", "a.md", EventModify, false)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := db.GetSyncState("ev-t3")
	if ss.DesiredGeneration != 105 {
		t.Fatalf("desired generation = %d, want 105 (existing debt watermark unchanged)", ss.DesiredGeneration)
	}
	pending, _ := db.ListPending("ev-t3")
	if len(pending) != 1 || pending[0].Path != "a.md" || pending[0].SourceGeneration != gen {
		t.Fatalf("pending = %+v", pending)
	}
}

// T4: destructive events promote to full debt, set the debounce window, and
// collapse prior pending evidence.
func TestRecordEventDestructivePromotes(t *testing.T) {
	db := openTestDB(t)
	for _, kind := range []string{EventDelete, EventRename, EventOther} {
		id := "ev-t4-" + kind
		mkEventProfile(t, db, id)
		claimAndSucceed(t, db, id)

		if _, err := db.RecordEvent(id, "prior.md", EventModify, false); err != nil {
			t.Fatal(err)
		}
		gen, err := db.RecordEvent(id, "gone.md", kind, false)
		if err != nil {
			t.Fatal(err)
		}
		ss, _ := db.GetSyncState(id)
		if ss.DesiredGeneration != gen {
			t.Fatalf("%s: desired = %d, want %d", kind, ss.DesiredGeneration, gen)
		}
		if !ss.HasDebt() {
			t.Fatalf("%s: destructive event must create full debt", kind)
		}
		if ss.ReconcileNotBeforeAt == nil {
			t.Fatalf("%s: debounce not-before must be set", kind)
		}
		pending, _ := db.ListPending(id)
		if len(pending) != 0 {
			t.Fatalf("%s: promotion must collapse pending evidence; got %d", kind, len(pending))
		}
		rt, _ := db.GetRuntime(id)
		if !rt.ReconcileRequested {
			t.Fatalf("%s: reconcile_requested must be set", kind)
		}
	}
}

// T5: unknown event kind with full=false still fails closed to a full
// reconcile (defensive).
func TestRecordEventUnknownKindFailsClosed(t *testing.T) {
	db := openTestDB(t)
	mkEventProfile(t, db, "ev-t5")
	claimAndSucceed(t, db, "ev-t5")

	gen, err := db.RecordEvent("ev-t5", "x.md", "mystery", false)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := db.GetSyncState("ev-t5")
	if ss.DesiredGeneration != gen {
		t.Fatalf("desired = %d, want %d (unknown kind must fail closed)", ss.DesiredGeneration, gen)
	}
	if !ss.HasDebt() {
		t.Fatal("unknown kind must create full debt")
	}
}

// T6: bootstrap fallback — a never-initialized profile receiving only a safe
// event must still establish an initial/full baseline.
func TestRecordEventSafeBootstrapEstablishesFull(t *testing.T) {
	db := openTestDB(t)
	mkEventProfile(t, db, "ev-t6")
	// CreateProfile seeds desired=1 as the initial-sync epoch. Reset it to the
	// exact bootstrap shape (no baseline, no desired, no active run) to pin the
	// defensive fallback.
	if _, err := db.Exec(`UPDATE profile_sync_state SET desired_generation = 0 WHERE profile_id = ?`, "ev-t6"); err != nil {
		t.Fatal(err)
	}
	ss0, _ := db.GetSyncState("ev-t6")
	if ss0.LastSuccessGeneration != nil || ss0.DesiredGeneration != 0 {
		t.Fatalf("setup state = %+v", ss0)
	}
	gen, err := db.RecordEvent("ev-t6", "a.md", EventCreate, false)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := db.GetSyncState("ev-t6")
	if ss.DesiredGeneration != gen {
		t.Fatalf("bootstrap desired = %d, want %d", ss.DesiredGeneration, gen)
	}
	if !ss.HasDebt() {
		t.Fatal("bootstrap safe create must establish initial full debt")
	}
	// The initial intent collapses pending detail; a full baseline is required.
	pending, _ := db.ListPending("ev-t6")
	if len(pending) != 0 {
		t.Fatalf("bootstrap promotion must collapse pending; got %d", len(pending))
	}
}

// T7: initial run active + safe modify keeps the pending event without raising
// the initial run's target; a later fast pass repairs the path.
func TestRecordEventSafeModifyDuringInitialRun(t *testing.T) {
	db := openTestDB(t)
	mkEventProfile(t, db, "ev-t7")

	run, res, err := db.ClaimRun("ev-t7", newTestRunID())
	if err != nil || res != ClaimOK {
		t.Fatalf("initial claim: res=%v err=%v", res, err)
	}
	ss0, _ := db.GetSyncState("ev-t7")
	if ss0.DesiredGeneration != run.TargetGeneration {
		t.Fatalf("setup desired = %d, want %d", ss0.DesiredGeneration, run.TargetGeneration)
	}

	gen, err := db.RecordEvent("ev-t7", "a.md", EventModify, false)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := db.GetSyncState("ev-t7")
	if ss.DesiredGeneration != run.TargetGeneration {
		t.Fatalf("desired = %d, want %d (initial run target must not be raised)", ss.DesiredGeneration, run.TargetGeneration)
	}
	pending, _ := db.ListPending("ev-t7")
	if len(pending) != 1 || pending[0].Path != "a.md" || pending[0].SourceGeneration != gen {
		t.Fatalf("pending = %+v", pending)
	}

	// Initial run succeeds through its target; pending survives (target-bound
	// clear only removes events at or below the committed target).
	if err := db.CommitRunSuccess("ev-t7", run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	pending, _ = db.ListPending("ev-t7")
	if len(pending) != 1 || pending[0].Path != "a.md" {
		t.Fatalf("pending after initial success = %+v", pending)
	}
}

// TestIsSafeFastKind pins the classification used by RecordEvent and the fast
// consumer.
func TestIsSafeFastKind(t *testing.T) {
	for _, safe := range []string{EventCreate, EventModify} {
		if !IsSafeFastKind(safe) {
			t.Fatalf("%s must be safe", safe)
		}
	}
	for _, unsafe := range []string{EventDelete, EventRename, EventOther, "", "mystery"} {
		if IsSafeFastKind(unsafe) {
			t.Fatalf("%q must NOT be safe", unsafe)
		}
	}
}

// TestClearPendingEventsExactVersionSurvivesNewer is the DB-level deterministic
// race test (§11.4): a worker's snapshot version of an event is cleared without
// removing a newer same-path event recorded after the snapshot was taken.
func TestClearPendingEventsExactVersionSurvivesNewer(t *testing.T) {
	db := openTestDB(t)
	mkEventProfile(t, db, "ev-clear")

	if err := db.UpsertPendingEvent("ev-clear", ".tmp_scripts/x", EventModify, 101); err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.ListPending("ev-clear")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].SourceGeneration != 101 {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	// The watcher records a newer event for the same path AFTER the worker
	// captured its snapshot: the upsert dedupes the row and bumps generation.
	if err := db.UpsertPendingEvent("ev-clear", ".tmp_scripts/x", EventModify, 102); err != nil {
		t.Fatal(err)
	}

	// The worker clears only its exact snapshot versions.
	if err := db.ClearPendingEvents("ev-clear", snapshot); err != nil {
		t.Fatal(err)
	}

	left, err := db.ListPending("ev-clear")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Path != ".tmp_scripts/x" || left[0].SourceGeneration != 102 {
		t.Fatalf("newer event must survive exact clear; got %+v", left)
	}
}
