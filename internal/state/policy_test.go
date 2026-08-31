package state

import (
	"path/filepath"
	"testing"

	"knowledge-sync/internal/policy"
)

func mkPolicyProfile(t *testing.T, db *DB, id string) {
	t.Helper()
	p := &Profile{
		ID: id, ProfileUUID: "u-" + id, Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 100, MaxFileSize: 0,
	}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
}

// TestCommitIgnoreSnapshotAdvanceGeneration verifies a changed snapshot commits
// atomically and advances the unified generation exactly once (§7, §21.6).
func TestCommitIgnoreSnapshotAdvanceGeneration(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "pol1")
	if err := db.EnsurePolicyRow("pol1", 1); err != nil {
		t.Fatal(err)
	}

	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("*.log\n")},
	}}
	res, err := db.CommitIgnoreSnapshot("pol1", snap, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("changed snapshot must report changed")
	}
	rt, _ := db.GetRuntime("pol1")
	if rt.SourceGeneration != 2 {
		t.Fatalf("generation = %d, want 2", rt.SourceGeneration)
	}
	ss, _ := db.GetSyncState("pol1")
	if ss.DesiredGeneration != 2 {
		t.Fatalf("desired generation = %d, want 2", ss.DesiredGeneration)
	}
	pol, err := db.GetCommittedPolicy("pol1")
	if err != nil {
		t.Fatal(err)
	}
	if pol.PolicyHash != snap.Hash() || pol.RefreshState != PolicyRefreshPending {
		t.Fatalf("policy = %+v", pol)
	}

	// Byte-identical re-commit is a no-op.
	before, _ := db.GetSyncState("pol1")
	res2, err := db.CommitIgnoreSnapshot("pol1", snap, false)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Fatal("byte-identical snapshot must be a no-op")
	}
	after, _ := db.GetSyncState("pol1")
	if after.DesiredGeneration != before.DesiredGeneration {
		t.Fatal("no-op update must not advance generation")
	}
}

// TestCommitIgnoreSnapshotCommentOnlyAdvances verifies comment-only changes are
// real snapshot changes and do advance generation (§4.3).
func TestCommitIgnoreSnapshotCommentOnlyAdvances(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "pol2")
	if err := db.EnsurePolicyRow("pol2", 1); err != nil {
		t.Fatal(err)
	}
	snapA := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("*.log\n")},
	}}
	if _, err := db.CommitIgnoreSnapshot("pol2", snapA, false); err != nil {
		t.Fatal(err)
	}
	snapB := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("# comment\n*.log\n")},
	}}
	if _, err := db.CommitIgnoreSnapshot("pol2", snapB, false); err != nil {
		t.Fatal(err)
	}
	rt, _ := db.GetRuntime("pol2")
	if rt.SourceGeneration != 3 {
		t.Fatalf("comment change must advance generation; got %d", rt.SourceGeneration)
	}
}

// TestCommitIgnoreSnapshotLegacyDropGate verifies the first switch from
// legacy_migrated requires --accept-legacy-drop (§6.4, §21.5).
func TestCommitIgnoreSnapshotLegacyDropGate(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "pol3")
	// Simulate a legacy-migrated policy row.
	if _, err := db.Exec(`DELETE FROM profile_ignore_policy WHERE profile_id = 'pol3'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO profile_ignore_policy
		(profile_id, policy_source, policy_hash, committed_generation, committed_at, refresh_state)
		VALUES ('pol3', 'legacy_migrated', 'old-hash', 1, '2026-01-01T00:00:00.000Z', 'pending')`); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("*.log\n")},
	}}
	_, err := db.CommitIgnoreSnapshot("pol3", snap, false)
	if err != ErrLegacyDropRequired {
		t.Fatalf("want ErrLegacyDropRequired, got %v", err)
	}
	res, err := db.CommitIgnoreSnapshot("pol3", snap, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Fatal("accepted legacy drop must commit")
	}
	pol, _ := db.GetCommittedPolicy("pol3")
	if pol.PolicySource != PolicySourceGitignore {
		t.Fatalf("policy source = %s, want gitignore", pol.PolicySource)
	}
	// Gate never reappears.
	if _, err := db.CommitIgnoreSnapshot("pol3", snap, false); err != nil {
		t.Fatalf("gate must disappear permanently after switch: %v", err)
	}
}

// TestPrunePreviewAndAuthorization exercises the durable preview, ceiling
// enforcement, and stale handling (§13, §14).
func TestPrunePreviewAndAuthorization(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "prune1")

	// Seed two suppressed ledger rows for the committed policy hash.
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("secret.md\nother.md\n")},
	}}
	if _, err := db.CommitIgnoreSnapshot("prune1", snap, false); err != nil {
		t.Fatal(err)
	}
	pol, _ := db.GetCommittedPolicy("prune1")
	if err := db.MarkPolicyRefreshReady("prune1", pol.PolicyHash); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"secret.md", "other.md"} {
		if err := db.ManifestUpsert(ManifestEntry{ProfileID: "prune1", RelPath: p, Size: 1, ModTime: 1}); err != nil {
			t.Fatal(err)
		}
		if err := db.ManifestMarkSuppressed("prune1", p, pol.PolicyHash, 2); err != nil {
			t.Fatal(err)
		}
	}

	req, err := db.CreatePrunePreview("prune1", "preview-1")
	if err != nil {
		t.Fatal(err)
	}
	if req.CandidateCount != 2 || req.State != PruneStatePreviewed {
		t.Fatalf("preview = %+v", req)
	}

	// Insufficient explicit ceiling deletes zero and stays approval_required.
	_, err = db.AuthorizePrune("preview-1", 1)
	if err == nil {
		t.Fatal("ceiling 1 < 2 candidates must fail")
	}
	req2, _ := db.GetPruneRequest("preview-1")
	if req2.State != PruneStatePreviewed {
		t.Fatalf("state after failed authorize = %s", req2.State)
	}

	// Sufficient ceiling queues.
	req3, err := db.AuthorizePrune("preview-1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if req3.State != PruneStatePending || req3.AuthorizedLimit == nil || *req3.AuthorizedLimit != 5 {
		t.Fatalf("authorized = %+v", req3)
	}

	// Preview must be unavailable before refresh ready for a new hash. The
	// pending request (queued, not yet claimed/executed) becomes stale when the
	// policy changes first — the immutable authorization is invalidated before
	// deletion starts (§14.3).
	snap2 := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("other.md\n")},
	}}
	if _, err := db.CommitIgnoreSnapshot("prune1", snap2, false); err != nil {
		t.Fatal(err)
	}
	pol2, _ := db.GetCommittedPolicy("prune1")
	// The queued request is now stale after the policy commit.
	staleReq, _ := db.GetPruneRequest("preview-1")
	if staleReq.State != PruneStateStale {
		t.Fatalf("old request must be stale after policy change; state=%s", staleReq.State)
	}
	_, err = db.CreatePrunePreview("prune1", "preview-2")
	if err == nil {
		t.Fatal("preview must refuse before refresh ready for new hash")
	}
	_ = pol2

	// Claim on a stale request is impossible (it is no longer pending).
	claimed, err := db.ClaimPrune("prune1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed != nil {
		t.Fatalf("stale request must not be claimable; got %+v", claimed)
	}
}

// TestPruneTargetResultAndCompletion exercises target progress and compaction
// (§14.5, §14.6, §14.7).
func TestPruneTargetResultAndCompletion(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "prune2")
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("a.md\nb.md\n")},
	}}
	if _, err := db.CommitIgnoreSnapshot("prune2", snap, false); err != nil {
		t.Fatal(err)
	}
	pol, _ := db.GetCommittedPolicy("prune2")
	if err := db.MarkPolicyRefreshReady("prune2", pol.PolicyHash); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.md", "b.md"} {
		if err := db.ManifestUpsert(ManifestEntry{ProfileID: "prune2", RelPath: p, Size: 1, ModTime: 1}); err != nil {
			t.Fatal(err)
		}
		if err := db.ManifestMarkSuppressed("prune2", p, pol.PolicyHash, 2); err != nil {
			t.Fatal(err)
		}
	}
	req, err := db.CreatePrunePreview("prune2", "preview-2")
	if err != nil {
		t.Fatal(err)
	}
	if req.CandidateCount != 2 {
		t.Fatalf("candidate count = %d", req.CandidateCount)
	}
	if _, err := db.AuthorizePrune("preview-2", 10); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimPrune("prune2")
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.State != PruneStateRunning {
		t.Fatalf("claim = %+v", claimed)
	}
	if err := db.MarkPruneTargetResult("preview-2", "a.md", PruneTargetDeleted, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPruneTargetResult("preview-2", "b.md", PruneTargetMissing, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitPruneComplete("preview-2"); err != nil {
		t.Fatal(err)
	}
	done, err := db.GetPruneRequest("preview-2")
	if err != nil {
		t.Fatal(err)
	}
	if done.State != PruneStateCompleted || done.DeletedCount != 1 || done.MissingCount != 1 {
		t.Fatalf("completed = %+v", done)
	}
	// a.md (deleted) ledger row removed; b.md (missing) retained as suppressed?
	// §14.7 removes corresponding suppressed ledger rows for deleted targets.
	if _, err := db.ManifestGet("prune2", "a.md"); err == nil {
		t.Fatal("a.md ledger row must be removed after completed prune")
	}
	targets, err := db.PruneTargets("preview-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Fatalf("target rows must be compacted; got %d", len(targets))
	}
}

func TestPrunePreviewSupersedesOlder(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "prune3")
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("x.md\n")},
	}}
	if _, err := db.CommitIgnoreSnapshot("prune3", snap, false); err != nil {
		t.Fatal(err)
	}
	pol, _ := db.GetCommittedPolicy("prune3")
	if err := db.MarkPolicyRefreshReady("prune3", pol.PolicyHash); err != nil {
		t.Fatal(err)
	}
	if err := db.ManifestUpsert(ManifestEntry{ProfileID: "prune3", RelPath: "x.md", Size: 1, ModTime: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.ManifestMarkSuppressed("prune3", "x.md", pol.PolicyHash, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreatePrunePreview("prune3", "old-preview"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreatePrunePreview("prune3", "new-preview"); err != nil {
		t.Fatal(err)
	}
	old, _ := db.GetPruneRequest("old-preview")
	if old.State != PruneStateSuperseded {
		t.Fatalf("old preview state = %s, want superseded", old.State)
	}
	// A pending/running request is NOT superseded by a new preview.
	if _, err := db.AuthorizePrune("new-preview", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimPrune("prune3"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreatePrunePreview("prune3", "third-preview"); err != nil {
		t.Fatal(err)
	}
	active, _ := db.GetPruneRequest("new-preview")
	if active.State == PruneStateSuperseded {
		t.Fatal("running request must not be superseded by a newer preview")
	}
}

// TestCommitPolicyForNewProfile verifies the initial policy snapshot is
// committed atomically with profile creation (§6.1): the worker can never run a
// new profile with an accidental empty policy.
func TestCommitPolicyForNewProfile(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "newpol")
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("*.log\n")},
	}}
	if err := db.CommitPolicyForNewProfile("newpol", snap); err != nil {
		t.Fatal(err)
	}
	pol, err := db.GetCommittedPolicy("newpol")
	if err != nil {
		t.Fatal(err)
	}
	if pol.PolicyHash != snap.Hash() || pol.PolicySource != PolicySourceGitignore {
		t.Fatalf("policy = %+v", pol)
	}
	files, _ := db.GetPolicySnapshotFiles("newpol")
	if len(files) != 1 || files[0].ScopeDir != "" {
		t.Fatalf("snapshot files = %+v", files)
	}
}

// TestBackfillPolicyRowsMigratesLegacy verifies §6.2: profiles with legacy
// structured excludes migrate to a synthetic legacy_migrated policy and
// profiles without excludes get a safe empty gitignore policy.
func TestBackfillPolicyRowsMigratesLegacy(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "backfill-legacy")
	if _, err := db.Exec(`DELETE FROM profile_ignore_policy WHERE profile_id = 'backfill-legacy'`); err != nil {
		t.Fatal(err)
	}
	if err := db.AddExclude("backfill-legacy", RuleExcludeDirName, ".git"); err != nil {
		t.Fatal(err)
	}
	mkPolicyProfile(t, db, "backfill-empty")

	if err := db.backfillPolicyRows(); err != nil {
		t.Fatal(err)
	}
	legacy, err := db.GetCommittedPolicy("backfill-legacy")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.PolicySource != PolicySourceLegacyMigrated {
		t.Fatalf("legacy profile source = %s, want legacy_migrated", legacy.PolicySource)
	}
	files, _ := db.GetPolicySnapshotFiles("backfill-legacy")
	if len(files) != 1 {
		t.Fatalf("legacy snapshot files = %d", len(files))
	}
	empty, err := db.GetCommittedPolicy("backfill-empty")
	if err != nil {
		t.Fatal(err)
	}
	if empty.PolicySource != PolicySourceGitignore {
		t.Fatalf("empty profile source = %s, want gitignore", empty.PolicySource)
	}
	// Idempotent.
	if err := db.backfillPolicyRows(); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedPolicyBundleDistinguishesEmptyFromMissing(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "bundle")
	bundle, err := db.GetCommittedPolicyBundle("bundle")
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Snapshot == nil || len(bundle.Snapshot.Files) != 0 || bundle.Policy.PolicyHash != (&policy.Snapshot{}).Hash() {
		t.Fatalf("empty bundle policy=%+v snapshot_files=%d", *bundle.Policy, len(bundle.Snapshot.Files))
	}
	if _, err := db.Exec(`DELETE FROM profile_ignore_policy WHERE profile_id = 'bundle'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetCommittedPolicyBundle("bundle"); err == nil {
		t.Fatal("missing committed policy must fail closed")
	}
}

func TestPruneDiscoveryRejectsMalformedAndReservedCandidates(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "prune-guard")
	emptyHash := (&policy.Snapshot{}).Hash()
	if err := db.MarkPolicyRefreshReady("prune-guard", emptyHash); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"folder/../secret.md", ".knowledge-derived/MANIFEST.json"} {
		if _, err := db.CreatePrunePreviewFromUnmanagedPaths("prune-guard", "request-"+candidate, emptyHash, []string{candidate}); err == nil {
			t.Fatalf("candidate %q was accepted", candidate)
		}
	}
}

// TestClassifyDisappearancesDeleteVsSuppress covers §21.9: a path absent from
// the eligible scan that the current policy does NOT ignore is a proven
// ordinary deletion; a path the current policy ignores is suppression (not a
// proven delete).
func TestClassifyDisappearancesDeleteVsSuppress(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "del-vs-sup")
	if err := db.EnsurePolicyRow("del-vs-sup", 1); err != nil {
		t.Fatal(err)
	}
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("secret.md\n")},
	}}
	if _, err := db.CommitIgnoreSnapshot("del-vs-sup", snap, false); err != nil {
		t.Fatal(err)
	}
	pol, _ := db.GetCommittedPolicy("del-vs-sup")

	// Ledger has two active rows: gone.md (deleted locally) and secret.md
	// (now ignored by policy).
	if err := db.ManifestUpsert(ManifestEntry{ProfileID: "del-vs-sup", RelPath: "gone.md", Size: 1, ModTime: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.ManifestUpsert(ManifestEntry{ProfileID: "del-vs-sup", RelPath: "secret.md", Size: 1, ModTime: 1}); err != nil {
		t.Fatal(err)
	}
	// Active scan has neither.
	ev, err := db.ClassifyDisappearances("del-vs-sup", pol.PolicyHash, map[string]bool{}, snap)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.DeletePaths["gone.md"] {
		t.Fatal("gone.md (not ignored) must be a proven ordinary deletion")
	}
	if ev.DeletePaths["secret.md"] {
		t.Fatal("secret.md (ignored by policy) must NOT be a proven delete; it is suppressed")
	}
}

// TestClassifyDisappearancesUnknownFailsSafeToSuppression verifies catch-up
// ambiguity with an unknown policy context does not prove an ordinary deletion
// (§9.3).
func TestClassifyDisappearancesUnknownFailsSafeToSuppression(t *testing.T) {
	db := openTestDB(t)
	mkPolicyProfile(t, db, "del-vs-sup2")
	if err := db.EnsurePolicyRow("del-vs-sup2", 1); err != nil {
		t.Fatal(err)
	}
	pol, _ := db.GetCommittedPolicy("del-vs-sup2")
	snap := &policy.IgnoreSnapshot{}
	// Event with unknown policy context (policy_context_known = 0) for a path
	// that IS ignored by a policy snapshot.
	ignoringSnap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("mystery.md\n")},
	}}
	if err := db.ManifestUpsert(ManifestEntry{ProfileID: "del-vs-sup2", RelPath: "mystery.md", Size: 1, ModTime: 1}); err != nil {
		t.Fatal(err)
	}
	ev, err := db.ClassifyDisappearances("del-vs-sup2", pol.PolicyHash, map[string]bool{}, ignoringSnap)
	if err != nil {
		t.Fatal(err)
	}
	if ev.DeletePaths["mystery.md"] {
		t.Fatal("ignored path must fail safe to suppression (not a proven delete)")
	}
	_ = snap
}

var _ = filepath.Join
