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

// TestActivityTrackerFilesPerMinute verifies the live Files/min is a two-sample
// instantaneous rate over the file counter, mirroring Speed semantics (§0.1).
func TestActivityTrackerFilesPerMinute(t *testing.T) {
	base := time.Unix(3000, 0)
	now := base
	tr := &activityTracker{live: map[string]*liveActivity{}, now: func() time.Time { return now }}
	p := "upload-profile"
	r := "run-upload"
	tr.start(p, live.ActivityFullReconcile, &r)

	// Not known until a second sample establishes the delta.
	tr.setPhase(p, state.PhaseUploading)
	tr.observe(p, state.ProgressSnapshot{FilesCompleted: 0}, false, base)
	if snap := tr.snapshot(p); snap.FilesPerMinuteKnown {
		t.Fatal("files/min must be unknown after a single sample")
	}

	// Second sample: 10 files over 10 seconds => 1 file/s => 60 files/min.
	tr.observe(p, state.ProgressSnapshot{FilesCompleted: 10}, true, base.Add(10*time.Second))
	snap := tr.snapshot(p)
	if !snap.FilesPerMinuteKnown {
		t.Fatal("files/min must be known after two samples")
	}
	if snap.FilesPerMinute != 60.0 {
		t.Fatalf("files/min = %v, want 60.0 (10 files / 10s * 60)", snap.FilesPerMinute)
	}

	// Zero-delta over positive time is a known zero.
	tr.observe(p, state.ProgressSnapshot{FilesCompleted: 10}, false, base.Add(15*time.Second))
	snap = tr.snapshot(p)
	if !snap.FilesPerMinuteKnown || snap.FilesPerMinute != 0 {
		t.Fatalf("zero-delta files/min = known=%v rate=%v", snap.FilesPerMinuteKnown, snap.FilesPerMinute)
	}

	// Leaving the transfer phase clears files/min.
	tr.setPhase(p, state.PhaseFinalizing)
	snap = tr.snapshot(p)
	if snap.FilesPerMinuteKnown || snap.FilesPerMinute != 0 {
		t.Fatal("files/min must disappear outside a transfer phase")
	}
}

// TestActivityTrackerFilesPerMinuteObserveInitializes verifies that when the
// first progress frame arrives before any phase callback (empty phase), the
// observe() path treats it as a transfer phase and feeds the file estimator.
func TestActivityTrackerFilesPerMinuteObserveInitializes(t *testing.T) {
	base := time.Unix(4000, 0)
	now := base
	tr := &activityTracker{live: map[string]*liveActivity{}, now: func() time.Time { return now }}
	p := "upload-observe"
	r := "run-obs"
	tr.start(p, live.ActivityFullReconcile, &r)

	// First frame with empty phase initializes to uploading.
	tr.observe(p, state.ProgressSnapshot{FilesCompleted: 5, BytesCompleted: 1}, true, base)
	snap := tr.snapshot(p)
	if snap.Phase != state.PhaseUploading {
		t.Fatalf("phase = %s, want uploading", snap.Phase)
	}
	if snap.FilesPerMinuteKnown {
		t.Fatal("files/min must be unknown after the first sample")
	}

	// Later frame with a delta yields a rate.
	tr.observe(p, state.ProgressSnapshot{FilesCompleted: 65, BytesCompleted: 2}, true, base.Add(time.Minute))
	snap = tr.snapshot(p)
	if !snap.FilesPerMinuteKnown || snap.FilesPerMinute != 60.0 {
		t.Fatalf("files/min = known=%v rate=%v, want 60.0 (60 files / 60s)", snap.FilesPerMinuteKnown, snap.FilesPerMinute)
	}
}
