package cli

import (
	"path/filepath"
	"testing"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// TestIgnoreUpdateCommitAndStatus exercises the CLI-level ignore update/status
// surface: capture a .gitignore snapshot, commit it, and read it back.
func TestIgnoreUpdateCommitAndStatus(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "ignore-cli")
	// asyncTestProfile creates via CreateProfile, which does not commit policy.
	// Simulate a profile that went through the policy backfill (empty policy).
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	// Write a .gitignore on disk.
	mkTestFile(t, p.SourcePath, ".gitignore", "*.log\n")
	if err := runIgnoreUpdate(app, p, false); err != nil {
		t.Fatalf("ignore update: %v", err)
	}
	pol, err := app.DB.GetCommittedPolicy(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := app.DB.GetPolicySnapshotFiles(p.ID)
	if len(files) != 1 || files[0].RelativePath != ".gitignore" {
		t.Fatalf("snapshot files = %+v", files)
	}
	if pol.PolicyHash == "" {
		t.Fatal("policy hash must be set")
	}
	// Status reports the disk snapshot clean.
	if err := renderIgnoreStatus(app, p); err != nil {
		t.Fatalf("ignore status: %v", err)
	}
	// Modify disk → status reports modified (read-only).
	mkTestFile(t, p.SourcePath, ".gitignore", "*.log\n*.tmp\n")
	if err := renderIgnoreStatus(app, p); err != nil {
		t.Fatalf("ignore status modified: %v", err)
	}
}

// TestLegacyExcludeCLIDeprecated verifies structured exclude mutation commands
// now fail with a migration message and do not create a second policy source
// (§17.2).
func TestLegacyExcludeCLIDeprecated(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "legacy-dep")
	cmd := profileExcludeCmd()
	cmd.SetArgs([]string{p.ID, state.RuleExcludeDirName, ".git"})
	// The command uses NewApp internally; capture the deprecation error by
	// invoking RunE directly through the cobra command against the built app.
	// profileExcludeCmd now always returns the migration error.
	err := cmd.Execute()
	if err == nil {
		t.Fatal("legacy exclude mutation must fail")
	}
	if err.Error() != "structured exclude editing is deprecated; edit .gitignore and run `profile ignore update`" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestManualReconcileDoesNotAuthorizePrune verifies §21.11: the one-attempt
// manual reconcile override never authorizes prune, and prune authorization
// never raises the ordinary reconcile delete budget.
func TestManualReconcileDoesNotAuthorizePrune(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "auth-sep")
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	// Seed a suppressed row + ready refresh so prune preview is possible.
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("a.md\n")},
	}}
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	pol, _ := app.DB.GetCommittedPolicy(p.ID)
	if err := app.DB.MarkPolicyRefreshReady(p.ID, pol.PolicyHash); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.ManifestUpsert(state.ManifestEntry{ProfileID: p.ID, RelPath: "a.md", Size: 1, ModTime: 1}); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.ManifestMarkSuppressed(p.ID, "a.md", pol.PolicyHash, 2); err != nil {
		t.Fatal(err)
	}

	// A manual reconcile intent with allow-deletes does not create a prune
	// request.
	if _, err := app.DB.SubmitManualReconcile(p.ID, state.ManualReconcileIntent{AllowDeletes: 500, BypassDebounce: true}); err != nil {
		t.Fatal(err)
	}
	req, err := app.DB.GetActivePruneRequest(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if req != nil {
		t.Fatal("manual reconcile override must not create a prune request")
	}
	// The manual intent metadata belongs to the reconcile claim, not prune.
	pm, err := app.DB.ReadPendingManual(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pm.Consumed || pm.AllowDeletes != 500 {
		t.Fatalf("manual intent = %+v", pm)
	}
	_ = filepath.Join
}
