package profile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/paths"
	"knowledge-sync/internal/remote"
	"knowledge-sync/internal/state"
)

func runCmd(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// mockDrive builds a service wired to a local mock rclone remote that simulates
// a Drive backend (type=drive won't work with local, so we fake backend via a
// passthrough: for testing we skip the drive-backend check by using a local
// remote directly in the service rather than ValidateRemote).
func mockDrive(t *testing.T) (*Service, *state.DB, *rcexec.Rclone, string) {
	t.Helper()
	bin, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not installed")
	}
	conf := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(conf, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cmd := runCmd(bin, "--config", conf, "config", "create", "mock", "local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create mock: %v: %s", err, out)
	}
	cmd = runCmd(bin, "--config", conf, "config", "update", "mock", "root="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set root: %v: %s", err, out)
	}
	r := rcexec.NewRclone(bin, conf)
	db, err := state.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rm := remote.New(r, db)
	return NewService(db, r, rm), db, r, root
}

func TestProfileAddWithMockRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}

	svc, db, _, _ := mockDrive(t)
	src := t.TempDir()
	mkdirTree(t, src, ".obsidian")
	writeFile(t, src, "note.md", "# hello")
	writeFile(t, src, "Private/secret.md", "secret")
	writeFile(t, src, ".obsidian/workspace.json", "{}")

	ctx := context.Background()
	// Use dry-run first (skips ValidateRemote drive check).
	p, err := svc.Add(ctx, AddOptions{
		ID: "obs1", SourcePath: src, RemoteName: "mock", RemotePath: "Knowledge Mirror/Notes",
		Type: "obsidian", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "obs1" {
		t.Fatalf("id = %s", p.ID)
	}

	// Non-dry-run requires Drive backend; ValidateRemote will reject local.
	// Test the validation rejection directly.
	_, err = svc.Add(ctx, AddOptions{
		ID: "obs1", SourcePath: src, RemoteName: "mock", RemotePath: "x",
		Type: "obsidian",
	})
	if err == nil {
		t.Fatal("expected error: mock is not a drive backend")
	}
	if !strings.Contains(err.Error(), "only google drive") {
		t.Fatalf("unexpected error: %v", err)
	}

	// The profile was created in dry-run? No — dry-run returns before creating.
	// Verify no profile row exists.
	if _, err := db.GetProfile("obs1"); err == nil {
		t.Fatal("dry-run should not persist profile")
	}
}

func mkdirTree(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
