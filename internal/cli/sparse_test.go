package cli

import (
	"testing"
	"time"
)

// TestSparseCheckpointingAcrossManyFrames drives the checkpoint policy with a
// fake progress stream and asserts that many frames do not produce many durable
// telemetry writes (§18.6). The primary SQL-write reduction is enforced here:
// N live frames must not produce N UpdateRunStats-style writes.
func TestSparseCheckpointingAcrossManyFrames(t *testing.T) {
	ct := newCheckpointTracker()
	base := time.Now()
	_ = base
	ct.frame(true)
	writes := 0
	flush := func(force bool) bool {
		wrote, _ := ct.consume(force, time.Now())
		if wrote {
			writes++
		}
		return wrote
	}
	if !flush(false) {
		t.Fatal("initial frame must checkpoint")
	}
	for i := 0; i < 100; i++ {
		ct.frame(true)
		flush(false)
	}
	if writes > 2 {
		t.Fatalf("100 frames produced %d checkpoint writes; want <= 2", writes)
	}
	// A phase change forces one more durable write.
	ct.frame(false)
	flush(true)
	if writes < 2 {
		t.Fatalf("phase change must force a checkpoint; writes=%d", writes)
	}
}
