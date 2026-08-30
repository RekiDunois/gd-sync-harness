package cli

import (
	"testing"
	"time"
)

// TestCheckpointTrackerSparseWrites verifies N live frames do not produce N
// durable checkpoint writes; phase transitions force them (§9.2, §18.6).
func TestCheckpointTrackerSparseWrites(t *testing.T) {
	base := time.Unix(5000, 0)
	now := base
	ct := &checkpointTracker{now: func() time.Time { return now }}

	// First frame forces an initial checkpoint.
	ct.frame(true)
	wrote, measurable := ct.consume(false, now)
	if !wrote || !measurable {
		t.Fatalf("first frame should force a checkpoint: wrote=%v measurable=%v", wrote, measurable)
	}
	// Ten more frames within the interval: no writes.
	for i := 0; i < 10; i++ {
		ct.frame(true)
		wrote, _ := ct.consume(false, now.Add(time.Duration(i+1)*time.Second))
		if wrote {
			t.Fatalf("frame %d within interval must not write", i+1)
		}
	}
	// A phase transition forces a write even within the interval.
	ct.frame(false)
	wrote, _ = ct.consume(true, now.Add(15*time.Second))
	if !wrote {
		t.Fatal("phase transition must force a checkpoint")
	}
}

// TestCheckpointTrackerOneHourGuard verifies the 1-hour guard with a fake clock
// (§9.2, §18.6).
func TestCheckpointTrackerOneHourGuard(t *testing.T) {
	base := time.Unix(6000, 0)
	now := base
	ct := &checkpointTracker{now: func() time.Time { return now }}

	ct.frame(true)
	ct.consume(true, now) // establish baseline

	// At 59 minutes no write.
	ct.frame(true)
	wrote, _ := ct.consume(false, now.Add(59*time.Minute))
	if wrote {
		t.Fatal("must not checkpoint before one hour")
	}
	// At 61 minutes a write occurs.
	ct.frame(true)
	wrote, measurable := ct.consume(false, now.Add(61*time.Minute))
	if !wrote || !measurable {
		t.Fatalf("one-hour guard must write: wrote=%v measurable=%v", wrote, measurable)
	}
}

// TestCheckpointTrackerResetClearsWindow verifies reset begins a fresh window.
func TestCheckpointTrackerResetClearsWindow(t *testing.T) {
	base := time.Unix(7000, 0)
	ct := &checkpointTracker{now: func() time.Time { return base }}
	ct.frame(true)
	ct.consume(true, base)
	ct.reset()
	wrote, _ := ct.consume(false, base)
	if !wrote {
		t.Fatal("after reset the next frame must checkpoint again")
	}
}
