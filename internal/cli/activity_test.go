package cli

import (
	"testing"
	"time"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

func ptr(s string) *string { return &s }

// TestActivityTrackerSpeedSemantics verifies live speed is two-sample
// throughput, not the rclone average, with phase-aware visibility (§0.1).
func TestActivityTrackerSpeedSemantics(t *testing.T) {
	base := time.Unix(1000, 0)
	now := base
	tr := &activityTracker{live: map[string]*liveActivity{}, now: func() time.Time { return now }}
	p := "example-profile"
	runID := "run-1"
	tr.start(p, live.ActivityFullReconcile, &runID)
	tr.setPhase(p, state.PhaseScanning)

	tr.observe(p, state.ProgressSnapshot{BytesCompleted: 0, BytesTotal: 1000}, false, base)

	// During scanning phase speed must not be known.
	snap := tr.snapshot(p)
	if snap.SpeedKnown {
		t.Fatal("speed must not be known in scanning phase")
	}
	if snap.Kind != live.ActivityFullReconcile || snap.RunID == nil || *snap.RunID != runID {
		t.Fatalf("activity identity = %+v", snap)
	}

	// Move to uploading and provide a 1024-byte delta over 2 seconds.
	tr.setPhase(p, state.PhaseUploading)
	tr.observe(p, state.ProgressSnapshot{BytesCompleted: 0, BytesTotal: 4096}, false, base)
	tr.observe(p, state.ProgressSnapshot{BytesCompleted: 1024, BytesTotal: 4096}, true, base.Add(2*time.Second))
	snap = tr.snapshot(p)
	if !snap.SpeedKnown {
		t.Fatal("speed must be known after two uploading samples")
	}
	if snap.SpeedBytesPerSecond != 512 {
		t.Fatalf("speed = %f, want 512 (1024 bytes / 2s)", snap.SpeedBytesPerSecond)
	}
	if snap.BytesCompleted != 1024 || snap.BytesTotal != 4096 {
		t.Fatalf("bytes = %d/%d", snap.BytesCompleted, snap.BytesTotal)
	}
	if !snap.LastMeasurableProgressAt.Equal(base.Add(2 * time.Second)) {
		t.Fatalf("last measurable progress = %v", snap.LastMeasurableProgressAt)
	}
	if snap.PossibleStall {
		t.Fatal("recent progress must not be a stall")
	}

	// A zero-delta frame yields a known zero (genuine no-byte interval).
	tr.observe(p, state.ProgressSnapshot{BytesCompleted: 1024, BytesTotal: 4096}, false, base.Add(3*time.Second))
	snap = tr.snapshot(p)
	if !snap.SpeedKnown || snap.SpeedBytesPerSecond != 0 {
		t.Fatalf("zero-delta speed = known=%v rate=%f", snap.SpeedKnown, snap.SpeedBytesPerSecond)
	}

	// Leaving the transfer phase clears speed.
	tr.setPhase(p, state.PhaseFinalizing)
	snap = tr.snapshot(p)
	if snap.SpeedKnown || snap.SpeedBytesPerSecond != 0 {
		t.Fatal("speed must disappear outside a transfer phase")
	}
}

// TestActivityTrackerResetBetweenRuns verifies a prior run's baseline does not
// leak into a new run (§7 rule 8).
func TestActivityTrackerResetBetweenRuns(t *testing.T) {
	tr := newActivityTracker()
	p := "example-profile"
	base := time.Unix(2000, 0)
	// First run: build a large baseline.
	r1 := "run-1"
	tr.start(p, live.ActivityFullReconcile, &r1)
	tr.setPhase(p, state.PhaseUploading)
	tr.observe(p, state.ProgressSnapshot{BytesCompleted: 10_000_000}, false, base)
	tr.observe(p, state.ProgressSnapshot{BytesCompleted: 10_000_000}, true, base.Add(time.Second))
	tr.finish(p)

	// Second run must start unknown, not carry the 10 MB baseline.
	r2 := "run-2"
	tr.start(p, live.ActivityFullReconcile, &r2)
	tr.setPhase(p, state.PhaseUploading)
	tr.observe(p, state.ProgressSnapshot{BytesCompleted: 0}, false, base.Add(time.Minute))
	snap := tr.snapshot(p)
	if snap.SpeedKnown {
		t.Fatal("new run must not inherit the prior run's speed baseline")
	}
	tr.observe(p, state.ProgressSnapshot{BytesCompleted: 100}, true, base.Add(time.Minute).Add(time.Second))
	snap = tr.snapshot(p)
	if !snap.SpeedKnown || snap.SpeedBytesPerSecond != 100 {
		t.Fatalf("post-reset speed = %+v, want 100", snap.SpeedBytesPerSecond)
	}
}

// TestActivityTrackerFinishesClearsState verifies finish removes the activity.
func TestActivityTrackerFinishesClearsState(t *testing.T) {
	tr := newActivityTracker()
	p := "example-profile"
	r := "run-x"
	tr.start(p, live.ActivityFastUpsert, &r)
	if tr.snapshot(p) == nil {
		t.Fatal("activity must exist before finish")
	}
	tr.finish(p)
	if tr.snapshot(p) != nil {
		t.Fatal("activity must be cleared after finish")
	}
}
