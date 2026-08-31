package cli

import (
	"context"
	"strings"
	"testing"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

func TestPruneBatchDeletesAndClassifiesMissing(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "prune-batch")
	mkTestFile(t, p.SourcePath, "b.md", "hello")
	mkTestFile(t, p.SourcePath, "c.md", "hello")
	mkTestFile(t, p.SourcePath, "d.md", "hello")
	writeSidecarForTest(t, app, p)

	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("initial pass: %v", err)
	}
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("b.md\nc.md\nd.md\n")},
	}}
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("refresh pass: %v", err)
	}
	if sup, _ := app.DB.ManifestSuppressedCount(p.ID); sup != 3 {
		t.Fatalf("suppressed = %d, want 3", sup)
	}

	req, err := app.DB.CreatePrunePreview(p.ID, "prune-batch-req")
	if err != nil {
		t.Fatal(err)
	}
	if req.CandidateCount != 3 {
		t.Fatalf("candidate count = %d, want 3", req.CandidateCount)
	}
	if _, err := app.DB.AuthorizePrune(req.RequestID, 3); err != nil {
		t.Fatal(err)
	}

	// Simulate one target disappearing after authorization but before the worker
	// executes. The batch existence pass must classify it as missing, while the
	// remaining targets are deleted together through rclone delete.
	res := app.Rclone.Run(context.Background(), "deletefile", p.RemoteName+":"+p.RemoteDisplayPath+"/c.md")
	if res.Err != nil {
		t.Fatalf("remove remote target before prune: %v: %s", res.Err, res.StderrTrimmed())
	}

	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("prune worker pass: %v", err)
	}
	done, err := app.DB.GetPruneRequest(req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != state.PruneStateCompleted {
		t.Fatalf("prune state = %s, want completed", done.State)
	}
	if done.DeletedCount != 2 || done.MissingCount != 1 {
		t.Fatalf("prune summary deleted/missing = %d/%d, want 2/1", done.DeletedCount, done.MissingCount)
	}
	if sup, _ := app.DB.ManifestSuppressedCount(p.ID); sup != 0 {
		t.Fatalf("suppressed manifest rows after prune = %d, want 0", sup)
	}
	for _, rel := range []string{"b.md", "c.md", "d.md"} {
		if got := readRemoteFile(t, app.Rclone, p.RemoteName, p.RemoteDisplayPath+"/"+rel); got != "" {
			t.Fatalf("remote %s = %q, want absent", rel, got)
		}
	}
}

func TestPruneRemovesOnlyEmptyTargetAncestorDirs(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "prune-empty-dirs")
	mkTestFile(t, p.SourcePath, "ignored/deep/drop.md", "drop")
	mkTestFile(t, p.SourcePath, "mixed/drop.tmp", "drop")
	mkTestFile(t, p.SourcePath, "mixed/keep.md", "keep")
	writeSidecarForTest(t, app, p)

	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("initial pass: %v", err)
	}
	if err := app.DB.EnsurePolicyRow(p.ID, 1); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("ignored/\n*.tmp\n")},
	}}
	if _, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("refresh pass: %v", err)
	}
	if sup, _ := app.DB.ManifestSuppressedCount(p.ID); sup != 2 {
		t.Fatalf("suppressed = %d, want 2", sup)
	}

	req, err := app.DB.CreatePrunePreview(p.ID, "prune-empty-dirs-req")
	if err != nil {
		t.Fatal(err)
	}
	if req.CandidateCount != 2 {
		t.Fatalf("candidate count = %d, want 2", req.CandidateCount)
	}
	if _, err := app.DB.AuthorizePrune(req.RequestID, 2); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerPass(nil, app, "", nil); err != nil {
		t.Fatalf("prune worker pass: %v", err)
	}

	rootDirs := listRemoteDirs(t, app, p, "")
	if strings.Contains(rootDirs, "ignored/") {
		t.Fatalf("ignored target-only directory survived prune: %q", rootDirs)
	}
	if !strings.Contains(rootDirs, "mixed/") {
		t.Fatalf("non-empty mixed directory was removed: %q", rootDirs)
	}
	if got := readRemoteFile(t, app.Rclone, p.RemoteName, p.RemoteDisplayPath+"/mixed/keep.md"); got != "keep" {
		t.Fatalf("non-target mixed/keep.md = %q, want keep", got)
	}
	if got := readRemoteFile(t, app.Rclone, p.RemoteName, p.RemoteDisplayPath+"/mixed/drop.tmp"); got != "" {
		t.Fatalf("target mixed/drop.tmp = %q, want absent", got)
	}
}

func listRemoteDirs(t *testing.T, app *App, p *state.Profile, rel string) string {
	t.Helper()
	remotePath := p.RemoteName + ":" + p.RemoteDisplayPath
	if rel != "" {
		remotePath += "/" + rel
	}
	res := app.Rclone.Run(context.Background(), "lsf", "--dirs-only", remotePath)
	if res.Err != nil {
		t.Fatalf("list remote dirs %s: %v: %s", rel, res.Err, res.StderrTrimmed())
	}
	return string(res.Stdout)
}

func TestValidatePruneTargetPathRejectsEscapes(t *testing.T) {
	for _, rel := range []string{"", "/absolute", "../outside", "dir/../outside", "line\nbreak"} {
		if err := validatePruneTargetPath(rel); err == nil {
			t.Fatalf("validatePruneTargetPath(%q) unexpectedly succeeded", rel)
		}
	}
	for _, rel := range []string{"a.md", "sub/b.md", "#literal.md", " leading-space.md"} {
		if err := validatePruneTargetPath(rel); err != nil {
			t.Fatalf("validatePruneTargetPath(%q): %v", rel, err)
		}
	}
}
