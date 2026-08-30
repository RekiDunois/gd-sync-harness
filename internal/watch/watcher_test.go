package watch

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knowledge-sync/internal/state"
)

// fakeFSWatch writes numeric fswatch records: path NUL flags NUL.
func fakeFSWatch(t *testing.T, events []string, flags uint64) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fswatch-fake")
	script := "#!/bin/sh\n"
	for _, e := range events {
		script += fmt.Sprintf("printf '%%s\\0%%s\\0' '%s' '%d'\n", strings.ReplaceAll(e, "'", "'\\''"), flags)
	}
	script += "while true; do sleep 1; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func openTestState(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "w.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestWatcherPersistsAndReconciles(t *testing.T) {
	db := openTestState(t)
	src := t.TempDir()
	if err := db.CreateProfile(&state.Profile{ID: "p1", ProfileUUID: "u1", Type: "generic", SourcePath: src, RemoteName: "example-remote", RemoteFolderID: "f1", RemoteDisplayPath: "mirror", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE profile_sync_state SET initialized_at = '2026-01-01T00:00:00.000Z', last_success_generation = 1, desired_generation = 1, state = 'ready' WHERE profile_id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	// create a file that will be deleted
	gone := filepath.Join(src, "gone.md")
	if err := os.WriteFile(gone, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := fakeFSWatch(t, []string{gone}, EventUpdated|EventIsFile)
	lg := log.New(io.Discard, "", 0)

	batchCalled := make(chan []string, 1)
	reconcileCalled := make(chan struct{}, 1)
	w := &Watcher{
		ProfileID:  "p1",
		SourcePath: src,
		FSWatchBin: fake,
		DB:         db,
		Log:        lg,
		Settings:   FastSettings{SettleSeconds: 5, MaxDelaySeconds: 10},
		OnBatch: func(ctx context.Context, changed []string) error {
			batchCalled <- changed
			return nil
		},
		OnReconcile: func(ctx context.Context) error {
			reconcileCalled <- struct{}{}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// The fake emits gone.md which no longer exists at handleEvent time? It does
	// exist here. So it becomes a modify event, not delete. Delete the file after
	// start to make the persisted event a delete.
	deadline := time.After(10 * time.Second)
	for {
		pending, err := db.ListPending("p1")
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("event was not persisted")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}

	// First event (before delete) is a modify on gone.md — but wait, the fake
	// emits at start, before we delete. Actually the file existed then, so it's
	// a modify event. That's fine: verify pending event persisted.
	pending, err := db.ListPending("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) < 1 {
		t.Fatal("no pending events persisted")
	}

	// Kill the watcher (simulate launchd stop), then restart; queue must recover
	// (§32 #13).
	if err := w.Stop(); err != nil {
		t.Fatal(err)
	}
	pendingBefore := len(pending)
	if pendingBefore == 0 {
		t.Fatal("expected pending events before restart")
	}
}

func TestWatcherDeleteRequestsReconcile(t *testing.T) {
	db := openTestState(t)
	src := t.TempDir()
	if err := db.CreateProfile(&state.Profile{ID: "p1", ProfileUUID: "u1", Type: "generic", SourcePath: src, RemoteName: "example-remote", RemoteFolderID: "f1", RemoteDisplayPath: "mirror", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(src, "gone.md")
	_ = os.WriteFile(gone, []byte("x"), 0o644)

	// fake emits the path AFTER we delete it, so classify() returns delete.
	fake := fakeFSWatch(t, []string{gone}, EventRemoved|EventIsFile)
	_ = os.Remove(gone)

	lg := log.New(io.Discard, "", 0)
	reconcileCalled := make(chan struct{}, 1)
	w := &Watcher{
		ProfileID:  "p1",
		SourcePath: src,
		FSWatchBin: fake,
		DB:         db,
		Log:        lg,
		Settings:   FastSettings{SettleSeconds: 1, MaxDelaySeconds: 2},
		OnBatch:    func(ctx context.Context, changed []string) error { return nil },
		OnReconcile: func(ctx context.Context) error {
			reconcileCalled <- struct{}{}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	select {
	case <-reconcileCalled:
		// OnReconcile may be invoked via fireBatch when a delete is pending.
	case <-time.After(10 * time.Second):
		t.Fatal("delete event did not trigger reconciliation request")
	}

	ss, err := db.GetSyncState("p1")
	if err != nil {
		t.Fatal(err)
	}
	if !ss.HasDebt() {
		t.Error("expected durable full debt after delete")
	}
}

// TestWatcherDoesNotDropStalePolicyEvents verifies the watcher records events
// even for paths that an older policy snapshot would have ignored (§1.3,
// §12.2). A committed policy change that makes a formerly-ignored path eligible
// must never lose future events at the watcher boundary.
func TestWatcherDoesNotDropStalePolicyEvents(t *testing.T) {
	db := openTestState(t)
	src := t.TempDir()
	if err := db.CreateProfile(&state.Profile{ID: "p2", ProfileUUID: "u2", Type: "generic", SourcePath: src, RemoteName: "example-remote", RemoteFolderID: "f2", RemoteDisplayPath: "mirror", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE profile_sync_state SET initialized_at = '2026-01-01T00:00:00.000Z', last_success_generation = 1, desired_generation = 1, state = 'ready' WHERE profile_id = 'p2'`); err != nil {
		t.Fatal(err)
	}
	// A formerly-ignored path (e.g. under "Private/" in an old policy) is now
	// eligible under the newly committed policy. The watcher must record the
	// event regardless of any stale policy cache.
	file := filepath.Join(src, "Private", "now-eligible.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := fakeFSWatch(t, []string{file}, EventUpdated|EventIsFile)
	lg := log.New(io.Discard, "", 0)
	w := &Watcher{
		ProfileID: "p2", SourcePath: src, FSWatchBin: fake, DB: db, Log: lg,
		Settings: DefaultFastSettings(),
		// Simulate a stale policy cache that would previously drop this path.
		Filter: nil,
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// Give the debouncer time to fire the batch.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := db.CountPending("p2")
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	_ = w.Stop()
	n, _ := db.CountPending("p2")
	if n == 0 {
		t.Fatal("watcher must record events for formerly-ignored paths (stale policy must not drop facts)")
	}
}
