package state

import (
	"testing"

	"knowledge-sync/internal/policy"
)

func TestCommitIgnoreSnapshotAfterManualReconcilesCreatesDebt(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "policy-after-manual")
	if err := db.EnsurePolicyRow("policy-after-manual", 1); err != nil {
		t.Fatal(err)
	}

	// Initialize generation 1, then complete two manual reconciliations. Manual
	// reconciliation advances desired/last-success generations without changing
	// the committed policy generation.
	run, claim, err := db.ClaimRun("policy-after-manual", "initial")
	if err != nil || claim != ClaimOK {
		t.Fatalf("initial claim: result=%v err=%v", claim, err)
	}
	if err := db.CommitRunSuccess("policy-after-manual", run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"manual-1", "manual-2"} {
		submitted, err := db.SubmitManualReconcile("policy-after-manual", ManualReconcileIntent{BypassDebounce: true})
		if err != nil {
			t.Fatal(err)
		}
		run, claim, err = db.ClaimRun("policy-after-manual", id)
		if err != nil || claim != ClaimOK {
			t.Fatalf("manual claim %d: result=%v err=%v", i+1, claim, err)
		}
		if run.TargetGeneration != submitted {
			t.Fatalf("manual target %d = %d, want submitted %d", i+1, run.TargetGeneration, submitted)
		}
		if err := db.CommitRunSuccess("policy-after-manual", run.ID, run.TargetGeneration); err != nil {
			t.Fatal(err)
		}
	}

	before, err := db.GetSyncState("policy-after-manual")
	if err != nil {
		t.Fatal(err)
	}
	if before.DesiredGeneration != 3 || before.LastSuccessGeneration == nil || *before.LastSuccessGeneration != 3 {
		t.Fatalf("pre-policy generations = desired %d last %v, want 3/3", before.DesiredGeneration, before.LastSuccessGeneration)
	}

	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("sub/\n")},
	}}
	committed, err := db.CommitIgnoreSnapshot("policy-after-manual", snap, false)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Generation != 4 {
		t.Fatalf("policy generation = %d, want 4", committed.Generation)
	}

	after, err := db.GetSyncState("policy-after-manual")
	if err != nil {
		t.Fatal(err)
	}
	if after.DesiredGeneration != 4 || !after.HasDebt() {
		t.Fatalf("post-policy state = desired %d last %v debt=%t, want desired 4 with debt", after.DesiredGeneration, after.LastSuccessGeneration, after.HasDebt())
	}
	rt, err := db.GetRuntime("policy-after-manual")
	if err != nil {
		t.Fatal(err)
	}
	if rt.SourceGeneration != 4 {
		t.Fatalf("source generation = %d, want 4", rt.SourceGeneration)
	}

	run, claim, err = db.ClaimRun("policy-after-manual", "policy-refresh")
	if err != nil || claim != ClaimOK {
		t.Fatalf("policy refresh claim: result=%v err=%v", claim, err)
	}
	if run.TargetGeneration != 4 {
		t.Fatalf("policy refresh target = %d, want 4", run.TargetGeneration)
	}
}

func TestCommitIgnoreSnapshotCarriesPendingManualIntentForward(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "policy-pending-manual")
	if err := db.EnsurePolicyRow("policy-pending-manual", 1); err != nil {
		t.Fatal(err)
	}

	run, claim, err := db.ClaimRun("policy-pending-manual", "initial")
	if err != nil || claim != ClaimOK {
		t.Fatalf("initial claim: result=%v err=%v", claim, err)
	}
	if err := db.CommitRunSuccess("policy-pending-manual", run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}

	manualGen, err := db.SubmitManualReconcile("policy-pending-manual", ManualReconcileIntent{AllowDeletes: 7, BypassDebounce: true})
	if err != nil {
		t.Fatal(err)
	}
	if manualGen != 2 {
		t.Fatalf("manual generation = %d, want 2", manualGen)
	}

	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("cache/\n")},
	}}
	committed, err := db.CommitIgnoreSnapshot("policy-pending-manual", snap, false)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Generation != 3 {
		t.Fatalf("policy generation = %d, want 3", committed.Generation)
	}
	pending, err := db.ReadPendingManual("policy-pending-manual")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Consumed || pending.Generation != 3 || pending.AllowDeletes != 7 || !pending.BypassDebounce {
		t.Fatalf("pending manual after policy commit = %+v, want generation 3 with metadata preserved", pending)
	}
}
