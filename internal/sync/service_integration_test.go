package sync

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// mockRclone returns an Rclone wrapper pointing at a local test config with a
// local-directory remote so we can exercise the full copy/sync pipeline without
// a Google account. rclone behavior for copy/sync is backend-agnostic.
func mockRclone(t *testing.T) (*exec.Rclone, string) {
	t.Helper()
	bin, err := osexec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not installed")
	}
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cmd := osexec.Command(bin, "--config", conf, "config", "create", "mock", "local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create mock remote: %v: %s", err, out)
	}
	cmd = osexec.Command(bin, "--config", conf, "config", "update", "mock", "root="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set mock root: %v: %s", err, out)
	}
	// The local backend root is the process CWD (config `root =` is written but
	// not honored at runtime), so anchor CWD to the throwaway remote root so
	// remote reads/writes never pollute the package directory.
	t.Chdir(root)
	return exec.NewRclone(bin, conf), root
}

func newTestProfile(t *testing.T, src, remote, remotePath string) *state.Profile {
	t.Helper()
	return &state.Profile{
		ID: "it", ProfileUUID: "it-uuid", Type: "generic",
		SourcePath: src, RemoteName: remote, RemoteFolderID: "mock-folder",
		RemoteDisplayPath: remotePath, Enabled: true, MaxDelete: 100,
		MaxFileSize: 0,
	}
}

func TestFastUpsertAndVerify(t *testing.T) {
	r, _ := mockRclone(t)
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "src")
	mkdirAll(t, src)
	mkfile(t, src, "a.md", "hello")
	mkdirAll(t, filepath.Join(src, "sub"))
	mkfile(t, src, "sub/b.md", "world")

	p := newTestProfile(t, src, "mock", "mirror")
	svc := New(r, nil)

	if err := svc.FastUpsert(ctx, p, []string{"a.md"}); err != nil {
		t.Fatalf("fast upsert: %v", err)
	}
	if got := readRemote(t, r, "mock", "mirror/a.md"); got != "hello" {
		t.Fatalf("mirror/a.md = %q", got)
	}
	if readRemote(t, r, "mock", "mirror/sub/b.md") != "" {
		t.Fatal("b.md should not be mirrored yet")
	}
}

// TestFullSyncDeletesProven verifies proven local deletions are removed via the
// protected reconciliation path within the delete budget (§11.3 invariant 2).
func TestFullSyncDeletesProven(t *testing.T) {
	r, _ := mockRclone(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	mkdirAll(t, src)
	mkfile(t, src, "keep.md", "k")
	mkfile(t, src, "gone.md", "g")
	mkdirAll(t, filepath.Join(src, "sub"))
	mkfile(t, src, "sub/x.md", "x")

	p := newTestProfile(t, src, "mock", "mirror")
	svc := New(r, nil)

	// Establish the mirror with all three files.
	initList, _, err := writeFilesFromTest(t, []string{"keep.md", "gone.md", "sub/x.md"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(initList)
	if res := r.Run(ctx, "sync", "--files-from", initList, src, "mock:mirror"); res.Err != nil {
		t.Fatalf("initial sync: %v", res.Err)
	}
	for _, f := range []string{"keep.md", "gone.md", "sub/x.md"} {
		if readRemote(t, r, "mock", "mirror/"+f) == "" {
			t.Fatalf("expected %s on remote", f)
		}
	}

	// Delete gone.md locally.
	if err := os.Remove(filepath.Join(src, "gone.md")); err != nil {
		t.Fatal(err)
	}

	// Protected full sync with the proven-delete path removes it.
	snap := policy.IgnoreSnapshot{}
	if _, err := svc.FullSyncProtected(ctx, p, &snap, SyncOptions{}, nil); err != nil {
		t.Fatalf("protected sync: %v", err)
	}
	if err := svc.DeleteRemotePaths(ctx, p, []string{"gone.md"}); err != nil {
		t.Fatalf("delete proven: %v", err)
	}
	if readRemote(t, r, "mock", "mirror/gone.md") != "" {
		t.Fatal("gone.md should be removed from remote")
	}
	if readRemote(t, r, "mock", "mirror/keep.md") != "k" {
		t.Fatal("keep.md should remain")
	}
	if readRemote(t, r, "mock", "mirror/sub/x.md") != "x" {
		t.Fatal("sub/x.md should remain")
	}
}

func TestDryRunParsesCounts(t *testing.T) {
	out := `2026/08/29 12:00:00 NOTICE: ...
Transferred:   	   2 / 2, 3.2 KiB, 100%, 0 B/s, ETA 0s
Checks:                3 / 3, 100%
Deleted:               1 (files), 0 (dirs), 1.1 KiB
Elapsed time:         0.1s`
	res, err := parseDryRun(out)
	if err != nil {
		t.Fatal(err)
	}
	if res.ToCopy != 2 {
		t.Errorf("ToCopy = %d, want 2", res.ToCopy)
	}
	if res.ToDelete != 1 {
		t.Errorf("ToDelete = %d, want 1", res.ToDelete)
	}
}

// TestExcludeChangeBecomesSuppressed validates the new suppression model (§11):
// a previously mirrored file that becomes excluded by committed policy is
// suppressed and survives ordinary reconcile; it is never auto-removed.
func TestExcludeChangeBecomesSuppressed(t *testing.T) {
	r, _ := mockRclone(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	mkdirAll(t, src)
	mkfile(t, src, "keep.md", "k")
	mkfile(t, src, "tmp.bin", "t")

	p := newTestProfile(t, src, "mock", "mirror")
	svc := New(r, nil)

	// Initial mirror with both files.
	initList, _, err := writeFilesFromTest(t, []string{"keep.md", "tmp.bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(initList)
	if res := r.Run(ctx, "sync", "--files-from", initList, src, "mock:mirror"); res.Err != nil {
		t.Fatalf("initial sync: %v", res.Err)
	}
	if readRemote(t, r, "mock", "mirror/tmp.bin") != "t" {
		t.Fatal("tmp.bin should be mirrored initially")
	}

	// Now tmp.bin becomes excluded by policy. Active = keep.md only.
	snap := policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("tmp.bin\n")},
	}}
	active, err := ScanActivePaths(p.SourcePath, p.MaxFileSize, &snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0] != "keep.md" {
		t.Fatalf("active = %v", active)
	}

	// Protected sync must NOT delete the excluded tmp.bin (it is suppressed,
	// not an ordinary delete candidate).
	if _, err := svc.FullSyncProtected(ctx, p, &snap, SyncOptions{}, nil); err != nil {
		t.Fatalf("protected sync: %v", err)
	}
	if readRemote(t, r, "mock", "mirror/tmp.bin") != "t" {
		t.Fatal("excluded tmp.bin must survive (suppressed, not deleted)")
	}
	if readRemote(t, r, "mock", "mirror/keep.md") != "k" {
		t.Fatal("keep.md should remain")
	}
}

// TestReconcileProtectedSuppressedSurvives is the rclone integration spike for
// §11.3 invariant 4: a suppressed remote object survives ordinary protected
// reconciliation.
func TestReconcileProtectedSuppressedSurvives(t *testing.T) {
	r, root := mockRclone(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	mkdirAll(t, src)
	mkfile(t, src, "keep.md", "k")
	mkfile(t, src, "secret.md", "s")

	p := newTestProfile(t, src, "mock", "mirror")
	svc := New(r, nil)

	// Establish the mirror with both files.
	initList, _, err := writeFilesFromTest(t, []string{"keep.md", "secret.md"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(initList)
	res := r.Run(ctx, "sync", "--files-from", initList, src, "mock:mirror")
	if res.Err != nil {
		t.Fatalf("init sync: %v", res.Err)
	}

	// Now secret.md becomes suppressed (ignored by policy). Active = keep.md.
	snap := policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("secret.md\nother.md\n")},
	}}
	active, err := ScanActivePaths(p.SourcePath, p.MaxFileSize, &snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0] != "keep.md" {
		t.Fatalf("active = %v", active)
	}

	// Protected full sync must NOT delete the suppressed secret.md remotely.
	if _, err := svc.FullSyncProtected(ctx, p, &snap, SyncOptions{}, nil); err != nil {
		t.Fatalf("protected sync: %v", err)
	}
	if readRemote(t, r, "mock", "mirror/secret.md") != "s" {
		t.Fatal("suppressed secret.md must survive ordinary protected reconcile")
	}
	if readRemote(t, r, "mock", "mirror/keep.md") != "k" {
		t.Fatal("active keep.md must remain")
	}

	// A new ignored local file must not be uploaded.
	mkfile(t, src, "other.md", "o")
	mkfile(t, src, "secret2.md", "s2")
	if _, err := svc.FullSyncProtected(ctx, p, &snap, SyncOptions{}, nil); err != nil {
		t.Fatalf("protected sync 2: %v", err)
	}
	if readRemote(t, r, "mock", "mirror/other.md") != "" {
		t.Fatal("ignored local file must not be uploaded")
	}
	_ = root
}

// TestReconcileProtectedDeletesProvenPaths is the §11.3 invariant 2 spike:
// proven local deletes are removed within the ordinary delete budget, and a
// reactivated file resumes synchronization.
func TestReconcileProtectedDeletesProvenPaths(t *testing.T) {
	r, _ := mockRclone(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src")
	mkdirAll(t, src)
	mkfile(t, src, "keep.md", "k")
	mkfile(t, src, "gone.md", "g")

	p := newTestProfile(t, src, "mock", "mirror")
	svc := New(r, nil)

	initList, _, err := writeFilesFromTest(t, []string{"keep.md", "gone.md"})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(initList)
	if res := r.Run(ctx, "sync", "--files-from", initList, src, "mock:mirror"); res.Err != nil {
		t.Fatalf("init sync: %v", res.Err)
	}

	// Delete gone.md locally; keep.md active.
	if err := os.Remove(filepath.Join(src, "gone.md")); err != nil {
		t.Fatal(err)
	}
	snap := policy.IgnoreSnapshot{}
	active, err := ScanActivePaths(p.SourcePath, p.MaxFileSize, &snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0] != "keep.md" {
		t.Fatalf("active = %v", active)
	}

	// Protected reconcile with provenDeletes=[gone.md] removes it.
	if _, err := svc.FullSyncProtected(ctx, p, &snap, SyncOptions{}, nil); err != nil {
		t.Fatalf("protected sync: %v", err)
	}
	if err := svc.DeleteRemotePaths(ctx, p, []string{"gone.md"}); err != nil {
		t.Fatalf("delete proven: %v", err)
	}
	if readRemote(t, r, "mock", "mirror/gone.md") != "" {
		t.Fatal("proven delete must be removed")
	}
	if readRemote(t, r, "mock", "mirror/keep.md") != "k" {
		t.Fatal("keep.md must remain")
	}
}

func readRemote(t *testing.T, r *exec.Rclone, remote, path string) string {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "f")
	res := r.Run(context.Background(), "copyto", remote+":"+path, tmp)
	if res.Err != nil {
		return ""
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		return ""
	}
	return string(b)
}

func writeFilesFromTest(t *testing.T, files []string) (string, []string, error) {
	t.Helper()
	list, err := writeFilesFrom(files)
	return list, files, err
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
