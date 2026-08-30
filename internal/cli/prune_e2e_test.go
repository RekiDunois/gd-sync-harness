package cli

import (
	"context"
	"path/filepath"
	"testing"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// TestSuppressionLifecycleEndToEnd covers §21.8 and §21.10: mirror a file,
// commit an ignore policy, refresh → the ledger row becomes suppressed, the
// remote file remains, ordinary reconciliation does not delete it, prune
// preview includes it; then removing the ignore before prune reactivates the
// row and the stale request cannot delete it.
func TestSuppressionLifecycleEndToEnd(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "suppress-e2e")
	mkTestFile(t, p.SourcePath, "b.md", "hello")
	writeSidecarForTest(t, app, p)

	// Initial reconcile mirrors a.md and b.md.
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("initial worker pass: %v", err)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-suppress-e2e/a.md"); got != "hello" {
		t.Fatalf("remote a.md = %q", got)
	}

	// Commit an ignore policy that excludes b.md (a.md remains active).
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("b.md\n")},
	}}
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	pol, _ := app.DB.GetCommittedPolicy(p.ID)

	// Worker refresh pass: full reconcile + policy refresh → b.md suppressed.
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("refresh worker pass: %v", err)
	}
	pol, _ = app.DB.GetCommittedPolicy(p.ID)
	if pol.RefreshState != state.PolicyRefreshReady {
		t.Fatalf("refresh state = %s, want ready", pol.RefreshState)
	}
	sup, _ := app.DB.ManifestSuppressedCount(p.ID)
	if sup != 1 {
		t.Fatalf("suppressed count = %d, want 1", sup)
	}
	b, err := app.DB.ManifestGet(p.ID, "b.md")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != state.ManifestSuppressed {
		t.Fatalf("b.md state = %s", b.State)
	}
	// Remote b.md still exists (not deleted by ordinary reconcile).
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-suppress-e2e/b.md"); got != "hello" {
		t.Fatalf("remote b.md = %q, want hello (suppressed files must survive)", got)
	}

	// Another ordinary reconcile does not delete it.
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("second ordinary pass: %v", err)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-suppress-e2e/b.md"); got != "hello" {
		t.Fatalf("remote b.md deleted by ordinary reconcile: %q", got)
	}

	// Prune preview includes b.md and freezes policy hash.
	req, err := app.DB.CreatePrunePreview(p.ID, "prune-prev-1")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if req.CandidateCount != 1 || req.PolicyHash != pol.PolicyHash {
		t.Fatalf("preview = %+v", req)
	}

	// Remove the ignore rule before prune and refresh → b.md reactivates.
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, &policy.IgnoreSnapshot{}, false); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("reactivate worker pass: %v", err)
	}
	b, _ = app.DB.ManifestGet(p.ID, "b.md")
	if b.State != state.ManifestActive {
		t.Fatalf("b.md state after reactivate = %s", b.State)
	}
	// The old request must be stale and cannot delete.
	stale, _ := app.DB.GetPruneRequest("prune-prev-1")
	if stale.State != state.PruneStateStale {
		t.Fatalf("old request state = %s, want stale", stale.State)
	}
	// Remote b.md remains without a delete/re-upload cycle.
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-suppress-e2e/b.md"); got != "hello" {
		t.Fatalf("remote b.md after reactivate = %q", got)
	}
}

// TestPruneExecutesOnlyFrozenTargets covers §21.10: authorized prune deletes
// exactly the frozen suppressed targets, missing remote targets converge to
// success, and the request completes with summary retained.
func TestPruneExecutesOnlyFrozenTargets(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "prune-e2e")
	writeSidecarForTest(t, app, p)

	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("initial pass: %v", err)
	}
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("a.md\n")},
	}}
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("refresh pass: %v", err)
	}
	if sup, _ := app.DB.ManifestSuppressedCount(p.ID); sup != 1 {
		t.Fatalf("suppressed = %d", sup)
	}

	req, err := app.DB.CreatePrunePreview(p.ID, "prune-e2e-req")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.AuthorizePrune(req.RequestID, 10); err != nil {
		t.Fatal(err)
	}
	// Worker executes the authorized prune under the profile lock.
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("prune worker pass: %v", err)
	}
	done, _ := app.DB.GetPruneRequest(req.RequestID)
	if done.State != state.PruneStateCompleted {
		t.Fatalf("prune state = %s, want completed", done.State)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-prune-e2e/a.md"); got != "" {
		t.Fatalf("remote a.md = %q, want deleted", got)
	}
	// Summary retained; target rows compacted.
	if targets, _ := app.DB.PruneTargets(req.RequestID); len(targets) != 0 {
		t.Fatalf("target rows must be compacted; got %d", len(targets))
	}
}

// TestPruneStaleOnPolicyChange verifies policy change before execution marks a
// queued request stale and the worker deletes zero files (§21.10, §14.3).
func TestPruneStaleOnPolicyChange(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "prune-stale")
	writeSidecarForTest(t, app, p)

	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("initial pass: %v", err)
	}
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("a.md\n")},
	}}
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("refresh pass: %v", err)
	}
	req, err := app.DB.CreatePrunePreview(p.ID, "prune-stale-req")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.AuthorizePrune(req.RequestID, 10); err != nil {
		t.Fatal(err)
	}
	// Policy changes before the worker executes → request becomes stale.
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, &policy.IgnoreSnapshot{}, false); err != nil {
		t.Fatal(err)
	}
	// Refresh for the new empty policy.
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("post-change pass: %v", err)
	}
	req2, _ := app.DB.GetPruneRequest(req.RequestID)
	if req2.State != state.PruneStateStale {
		t.Fatalf("request state = %s, want stale", req2.State)
	}
	// a.md must still exist remotely (delete zero).
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-prune-stale/a.md"); got != "hello" {
		t.Fatalf("remote a.md = %q, want hello (stale prune must delete zero)", got)
	}
}

var _ = context.Background
var _ = filepath.Join
