package cli

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/remote"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

// TestSyncNowAndReconcileEndToEnd exercises the worker-owned fast-path + reconcile
// flow against a local mock rclone remote (backend-agnostic), validating that
// sync-now only records durable events and the worker performs the transfer.
func TestSyncNowAndReconcileEndToEnd(t *testing.T) {
	bin, err := osexec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not installed")
	}
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	remoteRoot := t.TempDir()
	mustRun(t, bin, "--config", conf, "config", "create", "mock", "local")
	mustRun(t, bin, "--config", conf, "config", "update", "mock", "root="+remoteRoot)
	r := exec.NewRclone(bin, conf)
	// The local backend root is the process CWD; anchor it so remote reads and
	// writes land under the throwaway remote root.
	t.Chdir(remoteRoot)

	db, err := state.Open(filepath.Join(t.TempDir(), "e2e.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	src := t.TempDir()
	p := &state.Profile{
		ID: "e2e", ProfileUUID: "e2e-uuid", Type: "generic",
		SourcePath: src, RemoteName: "mock", RemoteFolderID: "mock-folder",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 100, MaxFileSize: 0,
	}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}

	app := &App{
		DB: db, Rclone: r,
		Sync: sync.New(r, db), Reconciler: sync.NewReconciler(sync.New(r, db)),
		Remote: remote.New(r, db), LockDir: filepath.Join(t.TempDir(), "locks"),
	}

	// Write files locally.
	mkTestFile(t, src, "a.md", "hello")
	mkTestFile(t, src, "sub/b.md", "world")

	// Establish initialization: a full reconcile is the only way to set
	// last_success_generation, so uninitialized profiles upgrade sync-now to a
	// full reconcile. Simulate a completed full run so the fast path becomes
	// eligible.
	run, res, err := db.ClaimRun(p.ID, "init-run")
	if err != nil || res != state.ClaimOK {
		t.Fatalf("init claim: res=%v err=%v", res, err)
	}
	if err := db.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		t.Fatal(err)
	}

	// sync-now records durable events; the worker executes the fast upsert.
	// With an empty manifest all files are changed → events recorded.
	if err := runSyncNow(app, p); err != nil {
		t.Fatalf("sync-now: %v", err)
	}
	pending, err := db.ListPending(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("sync-now must record 2 durable events, got %d", len(pending))
	}

	// The worker drains the due fast batch (lease-free path through a direct
	// call, since the test holds no remote lease). Wait past the settle window.
	time.Sleep(4 * time.Second)
	if err := drainFastBatchForTest(app, p); err != nil {
		t.Fatalf("worker fast batch: %v", err)
	}
	if got := readRemoteFile(t, r, "mock", "mirror/a.md"); got != "hello" {
		t.Fatalf("mirror/a.md = %q", got)
	}
	if n, _ := db.ManifestCount(p.ID); n != 0 {
		// The fast path does not rewrite the manifest; a full reconcile does.
		t.Logf("manifest count after fast upsert = %d (fast path is manifest-barrier-free)", n)
	}
	pending, _ = db.ListPending(p.ID)
	if len(pending) != 0 {
		t.Fatalf("fast batch must clear pending events, got %d", len(pending))
	}

	// Modify a.md; sync-now records another event and the worker upserts.
	mkTestFile(t, src, "a.md", "hello v2")
	if err := runSyncNow(app, p); err != nil {
		t.Fatalf("second sync-now: %v", err)
	}
	time.Sleep(4 * time.Second)
	if err := drainFastBatchForTest(app, p); err != nil {
		t.Fatalf("second fast batch: %v", err)
	}
	if got := readRemoteFile(t, r, "mock", "mirror/a.md"); got != "hello v2" {
		t.Fatalf("mirror/a.md after modify = %q", got)
	}
}

// drainFastBatchForTest runs the worker's fast-event evaluation directly so the
// test exercises the worker-owned execution without a live worker loop.
func drainFastBatchForTest(app *App, p *state.Profile) error {
	return runFastUpsertBatch(context.Background(), app, p, nil)
}

func mustRun(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := osexec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("rclone %v: %v: %s", args, err, out)
	}
}

func mkTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readRemoteFile(t *testing.T, r *exec.Rclone, remote, path string) string {
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
