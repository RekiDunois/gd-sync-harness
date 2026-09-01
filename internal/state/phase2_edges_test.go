package state

import (
	"database/sql"
	"errors"
	"testing"

	"knowledge-sync/internal/policy"
)

func TestSettingsRuntimeAndDurableSnapshot(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "runtime")
	if value, err := db.GetSetting("missing"); err != nil || value != "" {
		t.Fatalf("missing setting = %q, err=%v", value, err)
	}
	if err := db.SetSetting("tool", "example-tool"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting("tool", "example-tool-v2"); err != nil {
		t.Fatal(err)
	}
	if value, err := db.GetSetting("tool"); err != nil || value != "example-tool-v2" {
		t.Fatalf("setting = %q, err=%v", value, err)
	}
	if err := db.UnsetSetting("tool"); err != nil {
		t.Fatal(err)
	}

	if err := db.EnsureRuntime(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWatcherStatus(p.ID, WatcherRunning); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkFastSuccess(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetLastError(p.ID, "temporary failure"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkReconcileSuccess(p.ID); err != nil {
		t.Fatal(err)
	}
	runtime, err := db.GetRuntime(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.WatcherStatus != WatcherRunning || runtime.LastFastSuccess == nil || runtime.LastReconcileSuccess == nil || runtime.LastError != nil || runtime.ReconcileRequested {
		t.Fatalf("runtime = %+v", runtime)
	}

	snapshot, err := db.LoadDurableSnapshot(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Profile == nil || snapshot.SyncState == nil || snapshot.Runtime == nil || snapshot.Policy == nil {
		t.Fatalf("incomplete durable snapshot = %+v", snapshot)
	}
}

func TestManifestCountsAllAndDelete(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "manifest-edges")
	for _, entry := range []ManifestEntry{
		{ProfileID: p.ID, RelPath: "active.md", Size: 1, State: ManifestActive},
		{ProfileID: p.ID, RelPath: "suppressed.md", Size: 2, State: ManifestSuppressed},
	} {
		if err := db.ManifestUpsert(entry); err != nil {
			t.Fatal(err)
		}
	}
	active, suppressed, err := db.ManifestCounts(p.ID)
	if err != nil || active != 1 || suppressed != 1 {
		t.Fatalf("manifest counts = %d/%d, err=%v", active, suppressed, err)
	}
	all, err := db.ManifestAll(p.ID)
	if err != nil || len(all) != 2 {
		t.Fatalf("manifest all = %+v, err=%v", all, err)
	}
	if err := db.ManifestDelete(p.ID, "active.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ManifestGet(p.ID, "active.md"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted manifest lookup = %v", err)
	}
}

func TestPolicyRefreshAndManagedLedgerTransitions(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "policy-edges")
	snap := &policy.Snapshot{Files: []policy.File{{RelativePath: ".gitignore", Content: []byte("ignored.md\n")}}}
	result, err := db.CommitIgnoreSnapshot(p.ID, snap, false)
	if err != nil || !result.Changed {
		t.Fatalf("commit policy = %+v, err=%v", result, err)
	}
	loaded, err := db.GetCommittedSnapshot(p.ID)
	if err != nil || loaded.Hash() != snap.Hash() {
		t.Fatalf("loaded policy = %+v, err=%v", loaded, err)
	}
	if err := db.MarkPolicyRefreshRunning(p.ID, result.PolicyHash); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPolicyRefreshReady(p.ID, result.PolicyHash); err != nil {
		t.Fatal(err)
	}
	ready, err := db.PolicyRefreshReadyForHash(p.ID, result.PolicyHash)
	if err != nil || !ready {
		t.Fatalf("policy ready = %v, err=%v", ready, err)
	}
	if err := db.MarkPolicyRefreshError(p.ID, result.PolicyHash, "refresh failed"); err != nil {
		t.Fatal(err)
	}
	ready, err = db.PolicyRefreshReadyForHash(p.ID, result.PolicyHash)
	if err != nil || ready {
		t.Fatalf("policy error still ready = %v, err=%v", ready, err)
	}

	for _, path := range []string{"active.md", "deleted.md", "suppressed.md"} {
		if err := db.ManifestUpsert(ManifestEntry{ProfileID: p.ID, RelPath: path, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ManifestMarkSuppressed(p.ID, "suppressed.md", result.PolicyHash, result.Generation); err != nil {
		t.Fatal(err)
	}
	if err := db.ApplyManagedRefresh(p.ID, result.PolicyHash, result.Generation, map[string]bool{"active.md": true, "deleted.md": true}, map[string]bool{"deleted.md": true}); err != nil {
		t.Fatal(err)
	}
	active, suppressed, err := db.ManifestCounts(p.ID)
	if err != nil || active != 2 || suppressed != 1 {
		t.Fatalf("ledger after refresh = %d/%d, err=%v", active, suppressed, err)
	}

	other := phase2Profile(t, db, "new-policy")
	if err := db.CommitPolicyForNewProfile(other.ID, snap); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetCommittedSnapshot(other.ID); err != nil || got.Hash() != snap.Hash() {
		t.Fatalf("new profile policy = %+v, err=%v", got, err)
	}
}

func TestExactPendingClearsAndExplicitPolicyContext(t *testing.T) {
	db := openTestDB(t)
	p := phase2Profile(t, db, "pending-edges")
	policyHash := (&policy.Snapshot{}).Hash()
	if _, err := db.RecordEventWithPolicy(p.ID, "a.md", EventModify, false, policyHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordEventWithPolicy(p.ID, "b.md", EventModify, false, policyHash); err != nil {
		t.Fatal(err)
	}
	events, err := db.ListPending(p.ID)
	if err != nil || len(events) != 2 || !events[0].PolicyContextKnown {
		t.Fatalf("pending events = %+v, err=%v", events, err)
	}
	if err := db.ClearPendingPaths(p.ID, []string{"a.md"}); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearPendingPaths(p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearPendingEvents(p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.ClearPendingThroughGeneration(p.ID, events[1].SourceGeneration); err != nil {
		t.Fatal(err)
	}
	count, err := db.CountPending(p.ID)
	if err != nil || count != 0 {
		t.Fatalf("pending count after exact clears = %d, err=%v", count, err)
	}
}

func TestRemoteStateUpsertRoundTrip(t *testing.T) {
	db := openTestDB(t)
	remote := &Remote{
		RemoteName: "example-remote", Backend: "drive", LastQuotaCheck: "2026-01-01T00:00:00Z",
		TotalBytes: 100, UsedBytes: 25, FreeBytes: 75, QuotaStatus: QuotaOK,
	}
	if err := db.UpsertRemote(remote); err != nil {
		t.Fatal(err)
	}
	remote.FreeBytes = 0
	remote.QuotaStatus = QuotaFull
	if err := db.UpsertRemote(remote); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetRemote(remote.RemoteName)
	if err != nil {
		t.Fatal(err)
	}
	if got.Backend != "drive" || got.FreeBytes != 0 || got.QuotaStatus != QuotaFull {
		t.Fatalf("remote state = %+v", got)
	}
}
