package state

import (
	"testing"
)

// TestSubmitManualReconcileAdvancesGeneration verifies manual intent advances
// the durable generation by at least one opportunity and records the one-shot
// metadata (§10.2).
func TestSubmitManualReconcileAdvancesGeneration(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{ID: "manual", ProfileUUID: "u-manual", Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 42}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	before, _ := db.GetSyncState(p.ID)
	gen, err := db.SubmitManualReconcile(p.ID, ManualReconcileIntent{AllowDeletes: 7, BypassDebounce: true})
	if err != nil {
		t.Fatal(err)
	}
	if gen <= before.DesiredGeneration {
		t.Fatalf("submitted generation %d must exceed prior %d", gen, before.DesiredGeneration)
	}
	pm, err := db.ReadPendingManual(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pm.Consumed || pm.Generation != gen || pm.AllowDeletes != 7 || !pm.BypassDebounce {
		t.Fatalf("pending manual = %+v", pm)
	}
}

// TestManualReplaceOlderAndNonManualPreserves verifies a newer manual request
// replaces older unconsumed manual metadata, and non-manual intent never clears
// pending manual options (§10.4).
func TestManualReplaceOlderAndNonManualPreserves(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{ID: "merge", ProfileUUID: "u-merge", Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 100}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	g1, _ := db.SubmitManualReconcile(p.ID, ManualReconcileIntent{AllowDeletes: 5})
	// A filesystem-style request must not erase the pending manual option.
	if err := db.RequestReconcile(p.ID); err != nil {
		t.Fatal(err)
	}
	pm, _ := db.ReadPendingManual(p.ID)
	if pm.Consumed || pm.Generation != g1 || pm.AllowDeletes != 5 {
		t.Fatalf("pending manual after filesystem event = %+v", pm)
	}
	// A newer manual request replaces the older unconsumed options.
	g2, _ := db.SubmitManualReconcile(p.ID, ManualReconcileIntent{AllowDeletes: 9})
	if g2 <= g1 {
		t.Fatalf("newer manual generation %d must exceed %d", g2, g1)
	}
	pm, _ = db.ReadPendingManual(p.ID)
	if pm.Generation != g2 || pm.AllowDeletes != 9 {
		t.Fatalf("pending manual after newer request = %+v", pm)
	}
}

// TestScheduledPreservesManualMetadata verifies scheduled intent advances the
// generation without touching pending manual metadata (§10.3).
func TestScheduledPreservesManualMetadata(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{ID: "sched", ProfileUUID: "u-sched", Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 100}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	g, _ := db.SubmitManualReconcile(p.ID, ManualReconcileIntent{AllowDeletes: 3})
	if _, err := db.SubmitScheduledReconcile(p.ID); err != nil {
		t.Fatal(err)
	}
	pm, _ := db.ReadPendingManual(p.ID)
	if pm.Consumed || pm.Generation != g || pm.AllowDeletes != 3 {
		t.Fatalf("scheduled must preserve pending manual: %+v", pm)
	}
}

// TestClaimConsumesManualOverrideAtomically verifies the manual override is
// written into the claimed run's audit fields and consumed atomically with the
// claim (§11.2), and a retry does not inherit it (§11.3).
func TestClaimConsumesManualOverrideAtomically(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{ID: "audit", ProfileUUID: "u-audit", Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 42}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	gen, _ := db.SubmitManualReconcile(p.ID, ManualReconcileIntent{AllowDeletes: 77})

	run, res, err := db.ClaimRun(p.ID, "audit-run-1")
	if err != nil || res != ClaimOK {
		t.Fatalf("claim: res=%v err=%v", res, err)
	}
	if run.EffectiveMaxDelete != 77 {
		t.Fatalf("effective delete = %d, want 77", run.EffectiveMaxDelete)
	}
	if run.ManualDeleteOverride == nil || *run.ManualDeleteOverride != 77 {
		t.Fatalf("manual override audit = %v, want 77", run.ManualDeleteOverride)
	}
	if run.TargetGeneration != gen {
		t.Fatalf("run target = %d, want %d", run.TargetGeneration, gen)
	}
	// The override is consumed: nothing pending remains.
	pm, _ := db.ReadPendingManual(p.ID)
	if !pm.Consumed {
		t.Fatalf("manual override must be consumed on claim: %+v", pm)
	}

	// Commit failure, then automatic retry must use the persistent budget, not
	// the consumed manual override. Reopen the gate to simulate the retry being
	// due.
	if err := db.CommitRunFailure(p.ID, run.ID, RunFailure{Code: "net", Classification: RetryRetryable, Message: "temp"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReopenSyncGate(p.ID); err != nil {
		t.Fatal(err)
	}
	run2, res2, err := db.ClaimRun(p.ID, "audit-run-2")
	if err != nil || res2 != ClaimOK {
		t.Fatalf("retry claim: res=%v err=%v", res2, err)
	}
	if run2.EffectiveMaxDelete != 42 {
		t.Fatalf("retry effective delete = %d, want persistent budget 42", run2.EffectiveMaxDelete)
	}
	if run2.ManualDeleteOverride != nil {
		t.Fatalf("retry must not inherit manual override: %v", run2.ManualDeleteOverride)
	}
}

// TestDefaultBudgetProducesNoManualOverride verifies a normal claim records the
// persistent budget and no fake manual override (§18.8).
func TestDefaultBudgetProducesNoManualOverride(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{ID: "default", ProfileUUID: "u-default", Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 25}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	// Direct debt claim without manual submission.
	run, res, err := db.ClaimRun(p.ID, "default-run")
	if err != nil || res != ClaimOK {
		t.Fatalf("claim: res=%v err=%v", res, err)
	}
	if run.EffectiveMaxDelete != 25 {
		t.Fatalf("effective delete = %d, want 25", run.EffectiveMaxDelete)
	}
	if run.ManualDeleteOverride != nil {
		t.Fatalf("default claim must not carry a manual override: %v", run.ManualDeleteOverride)
	}
}

// TestMergeSequenceOneRunSatisfiesSeveralWaiters verifies manual + filesystem +
// scheduled coalesce into one claimed run capturing the latest target, so older
// submitted-generation waiters can be satisfied by the later run (§10.4,
// §18.7).
func TestMergeSequenceOneRunSatisfiesSeveralWaiters(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{ID: "coalesce", ProfileUUID: "u-coalesce", Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 100}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	gManual, _ := db.SubmitManualReconcile(p.ID, ManualReconcileIntent{AllowDeletes: 6})
	if err := db.RequestReconcile(p.ID); err != nil {
		t.Fatal(err)
	}
	gSched, err := db.SubmitScheduledReconcile(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := db.GetSyncState(p.ID)
	if ss.DesiredGeneration != gSched {
		t.Fatalf("desired = %d, want scheduled %d", ss.DesiredGeneration, gSched)
	}
	if gSched < gManual {
		t.Fatalf("scheduled generation %d must not regress manual %d", gSched, gManual)
	}
	// One claim captures the latest target.
	run, res, err := db.ClaimRun(p.ID, "coalesce-run")
	if err != nil || res != ClaimOK {
		t.Fatalf("claim: res=%v err=%v", res, err)
	}
	if run.TargetGeneration != gSched {
		t.Fatalf("run target = %d, want %d", run.TargetGeneration, gSched)
	}
	// Committing success through the run target satisfies both waiters (manual
	// generation and scheduled generation).
	if err := db.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	ss, _ = db.GetSyncState(p.ID)
	if ss.LastSuccessGeneration == nil || *ss.LastSuccessGeneration < gManual || *ss.LastSuccessGeneration < gSched {
		t.Fatalf("last success = %v, must satisfy manual %d and scheduled %d", ss.LastSuccessGeneration, gManual, gSched)
	}
	if ss.HasDebt() {
		t.Fatal("no debt should remain after the coalesced run")
	}
}
