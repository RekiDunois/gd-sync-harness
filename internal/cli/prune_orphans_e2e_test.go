package cli

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"testing"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

func TestIgnoredRemoteOrphanDiscoveryEndToEnd(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "prune-orphans")
	writeSidecarForTest(t, app, p)

	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("initial pass: %v", err)
	}
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte(".q2_transactions/\n*.tmp\n")},
	}}
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("policy refresh: %v", err)
	}
	pol, err := app.DB.GetCommittedPolicy(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pol.RefreshState != state.PolicyRefreshReady {
		t.Fatalf("refresh state = %s, want ready", pol.RefreshState)
	}

	putRemoteTestFile(t, app, p, ".q2_transactions/_commit_log.jsonl", "legacy")
	putRemoteTestFile(t, app, p, "manual-keep.md", "keep")

	ctx := context.Background()
	paths, err := discoverIgnoredRemoteOrphans(ctx, app, p, snap)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".q2_transactions/_commit_log.jsonl"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("discovered paths = %#v, want %#v", paths, want)
	}

	req, err := app.DB.CreatePrunePreviewFromUnmanagedPaths(p.ID, "orphan-prune-1", pol.PolicyHash, paths)
	if err != nil {
		t.Fatal(err)
	}
	if req.CandidateCount != 1 {
		t.Fatalf("candidate count = %d, want 1", req.CandidateCount)
	}
	if _, err := app.DB.AuthorizePrune(req.RequestID, 10); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("prune worker pass: %v", err)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-prune-orphans/.q2_transactions/_commit_log.jsonl"); got != "" {
		t.Fatalf("ignored orphan still exists: %q", got)
	}
	if got := readRemoteFile(t, app.Rclone, "mock", "mirror-prune-orphans/manual-keep.md"); got != "keep" {
		t.Fatalf("unignored unmanaged remote file changed: %q", got)
	}
}

func TestOrphanPreviewDropsPathThatBecameManaged(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "prune-orphan-race")
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("legacy.tmp\n")},
	}}
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	pol, err := app.DB.GetCommittedPolicy(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.DB.MarkPolicyRefreshReady(p.ID, pol.PolicyHash); err != nil {
		t.Fatal(err)
	}
	if err := app.DB.ManifestUpsert(state.ManifestEntry{
		ProfileID: p.ID, RelPath: "legacy.tmp", State: state.ManifestActive,
	}); err != nil {
		t.Fatal(err)
	}

	req, err := app.DB.CreatePrunePreviewFromUnmanagedPaths(p.ID, "orphan-race-1", pol.PolicyHash, []string{"legacy.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if req.CandidateCount != 0 {
		t.Fatalf("candidate count = %d, want 0 after path became managed", req.CandidateCount)
	}
}

func TestMissingSuppressedTargetClearsManifestOwnership(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "prune-missing-ledger")
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
	req, err := app.DB.CreatePrunePreview(p.ID, "missing-ledger-1")
	if err != nil {
		t.Fatal(err)
	}
	res := app.Rclone.Run(context.Background(), "deletefile", p.RemoteName+":"+p.RemoteDisplayPath+"/a.md")
	if res.Err != nil {
		t.Fatalf("pre-delete remote a.md: %v: %s", res.Err, res.StderrTrimmed())
	}
	if _, err := app.DB.AuthorizePrune(req.RequestID, 10); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("prune missing target: %v", err)
	}
	if _, err := app.DB.ManifestGet(p.ID, "a.md"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("manifest lookup err = %v, want sql.ErrNoRows", err)
	}
}

func putRemoteTestFile(t *testing.T, app *App, p *state.Profile, relPath, content string) {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "remote-test-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	res := app.Rclone.Run(context.Background(), "copyto", tmp.Name(), p.RemoteName+":"+p.RemoteDisplayPath+"/"+relPath)
	if res.Err != nil {
		t.Fatalf("put remote %s: %v: %s", relPath, res.Err, res.StderrTrimmed())
	}
}
