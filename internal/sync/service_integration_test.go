package sync

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"

	"knowledge-sync/internal/exec"
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

func TestFullSyncDeletes(t *testing.T) {
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

	if err := svc.FullSync(ctx, p, SyncOptions{}); err != nil {
		t.Fatalf("initial full sync: %v", err)
	}
	for _, f := range []string{"keep.md", "gone.md", "sub/x.md"} {
		if readRemote(t, r, "mock", "mirror/"+f) == "" {
			t.Fatalf("expected %s on remote", f)
		}
	}

	if err := os.Remove(filepath.Join(src, "gone.md")); err != nil {
		t.Fatal(err)
	}

	dry, err := svc.DryRunSync(ctx, p, SyncOptions{})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.ToDelete != 1 {
		t.Fatalf("dry-run deleted = %d, want 1", dry.ToDelete)
	}

	if err := svc.FullSync(ctx, p, SyncOptions{}); err != nil {
		t.Fatalf("full sync after delete: %v", err)
	}
	if readRemote(t, r, "mock", "mirror/gone.md") != "" {
		t.Fatal("gone.md should be removed from remote")
	}
	if readRemote(t, r, "mock", "mirror/keep.md") != "k" {
		t.Fatal("keep.md should remain")
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

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
