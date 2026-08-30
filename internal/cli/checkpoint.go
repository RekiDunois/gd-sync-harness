package cli

import "time"

// checkpointInterval is the maximum spacing between coarse durable telemetry
// checkpoints during a long still-running activity (§9.2). Between checkpoints
// live status flows over the socket without touching SQLite.
const checkpointInterval = time.Hour

// checkpointTracker decides when to persist coarse durable telemetry (§9.1).
// Live frames update worker memory and publish over the socket; a SQLite write
// happens only on a trigger:
//
//  1. a required durable write is already happening and telemetry can ride along
//     (phase changes);
//  2. the run succeeds or fails;
//  3. one hour has elapsed since the last checkpoint during a still-running
//     activity.
type checkpointTracker struct {
	now func() time.Time

	lastCheckpoint time.Time
	hasMeasurable  bool // measurable progress observed since the last checkpoint
}

// newCheckpointTracker builds a tracker using the real clock.
func newCheckpointTracker() *checkpointTracker {
	return &checkpointTracker{now: time.Now}
}

// frame records that a live frame arrived; measurable marks real work (advances
// durable last_progress_at).
func (c *checkpointTracker) frame(measurable bool) {
	if measurable {
		c.hasMeasurable = true
	}
}

// due reports whether a durable checkpoint is warranted for this frame.
func (c *checkpointTracker) due(now time.Time) bool {
	if c.lastCheckpoint.IsZero() {
		return true
	}
	return now.Sub(c.lastCheckpoint) >= checkpointInterval
}

// consume returns whether a checkpoint should be written now, and the durable
// measurable flag to use. When a phase/terminal trigger forces a checkpoint it
// is written even if the interval has not elapsed.
func (c *checkpointTracker) consume(force bool, now time.Time) (bool, bool) {
	if !force && !c.due(now) {
		return false, false
	}
	measurable := c.hasMeasurable
	c.hasMeasurable = false
	c.lastCheckpoint = now
	return true, measurable
}

// reset begins a fresh checkpoint window for a new run.
func (c *checkpointTracker) reset() {
	c.lastCheckpoint = time.Time{}
	c.hasMeasurable = false
}
