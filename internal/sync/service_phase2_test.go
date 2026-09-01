package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-sync/internal/config"
	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

func newFakeRclone(t *testing.T) (*rcexec.Rclone, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rclone-args.log")
	bin := filepath.Join(dir, "rclone.sh")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" >> "$FAKE_RCLONE_LOG"
command=""
dry=""
for arg in "$@"; do
  case "$arg" in
    mkdir|copy|copyto|sync|check|deletefile|lsjson) command="$arg" ;;
    --dry-run) dry="yes" ;;
  esac
done
if [ "$command" = "sync" ] && [ "$dry" = "yes" ] && [ -n "${FAKE_RCLONE_MUTATE:-}" ]; then
  printf 'changed-after-dry-run' > "$FAKE_RCLONE_MUTATE"
fi
if [ "$command" = "sync" ] && [ "$dry" = "" ] && [ -n "${FAKE_RCLONE_MUTATE_LIVE:-}" ]; then
  printf 'changed-during-live-sync' > "$FAKE_RCLONE_MUTATE_LIVE"
fi
case "${FAKE_RCLONE_MODE:-}:$command:$dry" in
  fail-check:check:) printf 'check failed\n' >&2; exit 7 ;;
  fail-sync:sync:) printf 'sync failed\n' >&2; exit 1 ;;
  fail-dry:sync:yes) printf 'dry-run failed\n' >&2; exit 1 ;;
  fail-live:sync:) printf 'sync failed\n' >&2; exit 1 ;;
  fail-delete:deletefile:) printf 'delete failed\n' >&2; exit 1 ;;
  duplicate:lsjson:) printf '[{"Path":"duplicate.md","Name":"duplicate.md","IsDir":false},{"Path":"duplicate.md","Name":"duplicate.md","IsDir":false}]\n' ;;
  *:lsjson:) printf '[]\n' ;;
  dry-counts:sync:yes) printf 'Transferred: 2 / 2\nDeleted: 3 (files)\n' ;;
  dry-malformed:sync:yes) printf 'not a dry-run summary\n' ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RCLONE_LOG", logPath)
	return &rcexec.Rclone{Binary: bin, ConfigPath: filepath.Join(dir, "config")}, logPath
}

func newServiceFixture(t *testing.T) (*Service, *state.DB, *state.Profile, string) {
	t.Helper()
	db := openSyncTestDB(t)
	src := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	p := &state.Profile{
		ID: "sync-phase2", ProfileUUID: "sync-phase2-uuid", Type: "generic",
		SourcePath: src, RemoteName: "example-remote", RemoteFolderID: "example-folder",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 5,
	}
	if err := db.CreateProfileWithPolicy(p, &policy.Snapshot{}); err != nil {
		t.Fatal(err)
	}
	r, logPath := newFakeRclone(t)
	return New(r, db), db, p, logPath
}

func openSyncTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readFakeRcloneLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVerifyVariantsPropagateFilesFromAndOptions(t *testing.T) {
	svc, _, p, logPath := newServiceFixture(t)
	if err := os.WriteFile(filepath.Join(p.SourcePath, "a.md"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc.RcloneConfig = config.RcloneConfig{
		GlobalArgs: []string{"--global-test=value"},
		VerifyArgs: []string{"--verify-test=value"},
	}

	if err := svc.VerifyCheck(context.Background(), p); err != nil {
		t.Fatalf("VerifyCheck: %v", err)
	}
	if err := svc.VerifyFull(context.Background(), p); err != nil {
		t.Fatalf("VerifyFull: %v", err)
	}
	log := readFakeRcloneLog(t, logPath)
	for _, want := range []string{"check", "--one-way", "--size-only", "--global-test=value", "--verify-test=value", "--files-from"} {
		if !strings.Contains(log, want) {
			t.Fatalf("rclone log missing %q:\n%s", want, log)
		}
	}
}

func TestDryRunHandlesCountsMalformedOutputAndEmptySource(t *testing.T) {
	svc, _, p, logPath := newServiceFixture(t)
	t.Setenv("FAKE_RCLONE_MODE", "dry-counts")
	got, err := svc.DryRunSyncProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{})
	if err != nil {
		t.Fatalf("DryRunSyncProtected: %v", err)
	}
	if got.ToCopy != 2 || got.ToDelete != 3 {
		t.Fatalf("dry-run result = %+v, want copy=2/delete=3", got)
	}

	t.Setenv("FAKE_RCLONE_MODE", "dry-malformed")
	got, err = svc.DryRunSyncProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{})
	if err != nil {
		t.Fatalf("malformed dry-run: %v", err)
	}
	if got.ToCopy != 0 || got.ToDelete != 0 {
		t.Fatalf("malformed dry-run result = %+v, want zero counts", got)
	}
	if !strings.Contains(readFakeRcloneLog(t, logPath), "--files-from") {
		t.Fatal("empty source must still pass a files-from list")
	}
}

func TestSyncAndVerifyFailuresAreReturned(t *testing.T) {
	tests := []struct {
		name string
		mode string
		call func(*Service, *state.Profile) error
		want string
	}{
		{name: "verify", mode: "fail-check", call: func(s *Service, p *state.Profile) error {
			return s.VerifyCheck(context.Background(), p)
		}, want: "verify check"},
		{name: "full-sync", mode: "fail-sync", call: func(s *Service, p *state.Profile) error {
			return func() error {
				_, err := s.FullSyncProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}, nil)
				return err
			}()
		}, want: "full sync"},
		{name: "delete", mode: "fail-delete", call: func(s *Service, p *state.Profile) error {
			return s.DeleteRemotePaths(context.Background(), p, []string{"a.md"})
		}, want: "delete a.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, p, _ := newServiceFixture(t)
			t.Setenv("FAKE_RCLONE_MODE", tt.mode)
			if err := tt.call(svc, p); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestDeleteRemotePathsRejectsUnsafeTargetsBeforeRclone(t *testing.T) {
	svc, _, p, logPath := newServiceFixture(t)
	for _, target := range []string{"../outside", ".knowledge-derived/MANIFEST.json"} {
		t.Run(target, func(t *testing.T) {
			if err := svc.DeleteRemotePaths(context.Background(), p, []string{target}); err == nil {
				t.Fatal("unsafe target was accepted")
			}
			if log, err := os.ReadFile(logPath); err == nil && len(log) != 0 {
				t.Fatalf("rclone must not run for rejected target: %s", log)
			}
		})
	}
}

func TestSyncFailsClosedWhenPolicySourceCannotBeScanned(t *testing.T) {
	svc, _, p, _ := newServiceFixture(t)
	p.SourcePath = filepath.Join(t.TempDir(), "missing")
	if _, err := svc.DryRunSyncProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}); err == nil {
		t.Fatal("missing policy source must fail closed")
	}
}
