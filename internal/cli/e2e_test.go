package cli

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/remote"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

// TestSyncNowAndReconcileEndToEnd exercises the fast-path + reconcile flow
// against a local mock rclone remote (backend-agnostic), validating the
// manifest barrier and delete detection without a Google account.
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
		Remote: remote.New(r, db), LockDir: filepath.Join(t.TempDir(), "locks"), scheduler: newSyncScheduler(db),
	}

	// Write a file locally.
	mkTestFile(t, src, "a.md", "hello")
	mkTestFile(t, src, "sub/b.md", "world")

	// sync-now: fast upsert of changed files (manifest is empty → all changed).
	if err := runSyncNow(app, p); err != nil {
		t.Fatalf("sync-now: %v", err)
	}
	if n, _ := db.ManifestCount(p.ID); n != 2 {
		t.Fatalf("manifest count = %d, want 2", n)
	}
	if got := readRemoteFile(t, r, "mock", "mirror/a.md"); got != "hello" {
		t.Fatalf("mirror/a.md = %q", got)
	}

	// sync-now again → up to date (no changes).
	if err := runSyncNow(app, p); err != nil {
		t.Fatalf("second sync-now: %v", err)
	}

	// Modify a.md; sync-now should upsert just it.
	mkTestFile(t, src, "a.md", "hello v2")
	if err := runSyncNow(app, p); err != nil {
		t.Fatalf("third sync-now: %v", err)
	}
	if got := readRemoteFile(t, r, "mock", "mirror/a.md"); got != "hello v2" {
		t.Fatalf("mirror/a.md after modify = %q", got)
	}

	// Delete a local file; sync-now must upgrade to full reconciliation (§18.1.6).
	// Reconcile requires sidecar ownership validation which doesn't exist here,
	// so it should fail closed (acceptance: fail-closed behavior).
	if err := os.Remove(filepath.Join(src, "sub/b.md")); err != nil {
		t.Fatal(err)
	}
	err = runSyncNow(app, p)
	if err == nil {
		t.Fatal("expected sync-now to fail closed on missing sidecar during reconcile upgrade")
	}
	if !strings.Contains(err.Error(), "ownership fail") && !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("reconcile upgrade correctly failed closed: %v", err)
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
