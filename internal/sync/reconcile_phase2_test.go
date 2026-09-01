package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

func TestReconcileRejectsDeleteBudgetBeforeLiveSync(t *testing.T) {
	svc, _, p, logPath := newServiceFixture(t)
	p.MaxDelete = 1
	if err := os.WriteFile(filepath.Join(p.SourcePath, "a.md"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RCLONE_MODE", "dry-counts")
	r := NewReconciler(svc)
	pre, err := r.ReconcileProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}, []string{"old.md", "older.md"})
	if !errors.Is(err, ErrDeleteBudgetExceeded) {
		t.Fatalf("error = %v, want delete budget error", err)
	}
	if pre == nil || pre.ToDelete != 3 {
		t.Fatalf("preflight = %+v, want parsed delete count", pre)
	}
	if log := readFakeRcloneLog(t, logPath); !containsLine(log, "--dry-run") {
		t.Fatalf("expected only dry-run before budget rejection:\n%s", log)
	}
}

func TestReconcileRejectsUnstableSourceAfterDryRun(t *testing.T) {
	svc, _, p, _ := newServiceFixture(t)
	path := filepath.Join(p.SourcePath, "a.md")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RCLONE_MUTATE", path)
	r := NewReconciler(svc)
	_, err := r.ReconcileProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}, nil)
	if !errors.Is(err, ErrSourceUnstable) {
		t.Fatalf("error = %v, want source-unstable error", err)
	}
}

func TestReconcileRejectsRemoteDuplicatesBeforeDryRun(t *testing.T) {
	svc, _, p, logPath := newServiceFixture(t)
	t.Setenv("FAKE_RCLONE_MODE", "duplicate")
	_, err := NewReconciler(svc).ReconcileProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "remote duplicates detected") {
		t.Fatalf("error = %v, want duplicate rejection", err)
	}
	log := readFakeRcloneLog(t, logPath)
	if !containsLine(log, "lsjson") || containsLine(log, "--dry-run") {
		t.Fatalf("duplicate rejection must stop before dry-run:\n%s", log)
	}
}

func TestReconcileRejectsDuplicateDirectoriesBeforeDryRun(t *testing.T) {
	svc, _, p, logPath := newServiceFixture(t)
	t.Setenv("FAKE_RCLONE_MODE", "duplicate-dirs")
	_, err := NewReconciler(svc).ReconcileProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "remote duplicates detected") {
		t.Fatalf("error = %v, want duplicate-directory rejection", err)
	}
	log := readFakeRcloneLog(t, logPath)
	if containsLine(log, "--dry-run") {
		t.Fatalf("duplicate-directory rejection must stop before dry-run:\n%s", log)
	}
}

func TestReconcileDeletesProvenPathsAndSucceedsWithoutDeletes(t *testing.T) {
	for _, test := range []struct {
		name       string
		deletes    []string
		wantDelete bool
	}{
		{name: "zero", deletes: nil},
		{name: "proven", deletes: []string{"gone.md"}, wantDelete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, _, p, logPath := newServiceFixture(t)
			if _, err := NewReconciler(svc).ReconcileProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}, test.deletes); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			log := readFakeRcloneLog(t, logPath)
			if containsLine(log, "deletefile") != test.wantDelete {
				t.Fatalf("deletefile presence = %v, want %v:\n%s", containsLine(log, "deletefile"), test.wantDelete, log)
			}
		})
	}
}

func TestReconcileMarksDirtyWhenSourceChangesDuringLiveSync(t *testing.T) {
	svc, db, p, _ := newServiceFixture(t)
	path := filepath.Join(p.SourcePath, "a.md")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RCLONE_MUTATE_LIVE", path)
	if _, err := NewReconciler(svc).ReconcileProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ss, err := db.GetSyncState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ss.HasDebt() {
		t.Fatalf("source change during live sync must leave debt: %+v", ss)
	}
}

func TestReconcilePropagatesFailuresAtOperationBoundaries(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		deletes []string
		want    string
	}{
		{name: "dry-run", mode: "fail-dry", want: "preflight dry-run"},
		{name: "live-sync", mode: "fail-live", want: "full sync"},
		{name: "delete", mode: "fail-delete", deletes: []string{"gone.md"}, want: "delete gone.md"},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc, _, p, _ := newServiceFixture(t)
			t.Setenv("FAKE_RCLONE_MODE", test.mode)
			_, err := NewReconciler(svc).ReconcileProtected(context.Background(), p, &policy.Snapshot{}, SyncOptions{}, test.deletes)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestReconcileProgressReportsPhasesAndStats(t *testing.T) {
	svc, _, p, _ := newServiceFixture(t)
	if err := os.WriteFile(filepath.Join(p.SourcePath, "a.md"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	var phases []string
	_, err := NewReconciler(svc).ReconcileProtectedProgress(
		context.Background(), p, &policy.Snapshot{}, SyncOptions{}, nil,
		func(rcexec.ProgressStats) {},
		func(phase string) { phases = append(phases, phase) },
	)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(phases) < 2 || phases[0] != state.PhasePlanning || phases[1] != state.PhaseUploading {
		t.Fatalf("phases = %v, want planning then uploading", phases)
	}
}

func containsLine(log, want string) bool {
	for _, line := range strings.Split(log, "\n") {
		if line == want {
			return true
		}
	}
	return false
}
