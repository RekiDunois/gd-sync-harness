package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"knowledge-sync/internal/flock"
	"knowledge-sync/internal/sidecar"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

type phase2ExitError int

func (e phase2ExitError) Error() string { return "fake rclone failure" }
func (e phase2ExitError) ExitCode() int { return int(e) }

func TestClassifyWorkerErrorsByRetrySafety(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantCode  string
		wantClass string
	}{
		{name: "temporary-rclone", err: phase2ExitError(5), wantCode: "rclone_temporary", wantClass: state.RetryRetryable},
		{name: "terminal-rclone", err: phase2ExitError(7), wantCode: "rclone_exit_7", wantClass: state.RetryTerminal},
		{name: "limited-rclone", err: phase2ExitError(1), wantCode: "rclone_uncategorized", wantClass: state.RetryRetryableLimited},
		{name: "unknown-exit", err: phase2ExitError(42), wantCode: "rclone_exit_42", wantClass: state.RetryRetryableLimited},
		{name: "source-unstable", err: sync.ErrSourceUnstable, wantCode: "source_unstable", wantClass: state.RetryRetryable},
		{name: "delete-budget", err: sync.ErrDeleteBudgetExceeded, wantCode: "delete_budget_exceeded", wantClass: state.RetryTerminal},
		{name: "canceled", err: context.Canceled, wantCode: "context_canceled", wantClass: state.RetryRetryable},
		{name: "ownership-temporary", err: &sidecar.ValidationError{Code: "ownership_unavailable", Temporary: true, Err: errors.New("transport")}, wantCode: "ownership_unavailable", wantClass: state.RetryRetryable},
		{name: "ownership-permanent", err: &sidecar.ValidationError{Code: "ownership_uuid_mismatch", Err: errors.New("mismatch")}, wantCode: "ownership_uuid_mismatch", wantClass: state.RetryTerminal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyError(tt.err, "fallback")
			if got.Code != tt.wantCode || got.Classification != tt.wantClass {
				t.Fatalf("classification = %+v, want %s/%s", got, tt.wantCode, tt.wantClass)
			}
		})
	}
}

func TestWorkerOwnershipFailureCommitsStructuredRunFailure(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "ownership-failure")
	if err := runWorkerPass(context.Background(), app, p.ID, nil); err != nil {
		t.Fatalf("worker pass: %v", err)
	}
	runs, err := app.DB.ListRuns(p.ID, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, err=%v", runs, err)
	}
	if runs[0].Status != state.RunFailed || runs[0].ErrorCode != "ownership_missing" {
		t.Fatalf("ownership failure run = %+v", runs[0])
	}
	if runs[0].ErrorClassification != state.RetryTerminal {
		t.Fatalf("ownership missing classification = %s", runs[0].ErrorClassification)
	}
}

func TestWorkerSkipsProfileLockContention(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "profile-lock")
	hold, err := flock.Acquire(app.LockDir, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	if err := runWorkerPass(context.Background(), app, p.ID, nil); err != nil {
		t.Fatalf("worker pass: %v", err)
	}
	runs, err := app.DB.ListRuns(p.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("lock contention must prevent a claim: %+v", runs)
	}
}

func TestWorkerContinuesAfterRemoteLeaseContention(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "remote-lease")
	for _, id := range []string{"held-lease-a", "held-lease-b"} {
		if err := app.DB.AcquireRemoteLease(context.Background(), p.RemoteName, 1, 2, 1, id); err != nil {
			t.Fatal(err)
		}
	}
	defer app.DB.ReleaseRemoteLease("held-lease-a")
	defer app.DB.ReleaseRemoteLease("held-lease-b")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := runWorkerPass(ctx, app, p.ID, nil); err != nil {
		t.Fatalf("worker pass should handle lease contention: %v", err)
	}
	runs, err := app.DB.ListRuns(p.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("lease contention must prevent a claim: %+v", runs)
	}
}

func TestFinalizePendingCompilerCleanRemovesLocalArtifacts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "compiler-clean")
	if err := app.DB.MarkCompilerClean(p.ID, "clean-operation"); err != nil {
		t.Fatal(err)
	}
	if err := finalizePendingCompilerClean(app, p); err != nil {
		t.Fatalf("finalize compiler clean: %v", err)
	}
	cs, err := app.DB.GetCompilerState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cs.LocalCleanState != "none" || cs.LocalCleanOperationID != nil {
		t.Fatalf("compiler state after clean = %+v", cs)
	}
}
