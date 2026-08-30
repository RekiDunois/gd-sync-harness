package live

import "time"

// RateSample is the estimator output. Known distinguishes an unknown speed
// (no baseline yet) from a measured value, including a measured zero.
type RateSample struct {
	Known          bool
	BytesPerSecond float64
}

// ThroughputEstimator computes whole-activity throughput from the latest two
// cumulative-byte samples (§7):
//
//	speed = (bytes_now - bytes_previous) / actual_elapsed_time
//
// It deliberately ignores rclone's top-level average speed. The first sample
// only establishes a baseline (Known=false). A measured 0 B/s is distinct from
// an unknown speed. The estimator is reset between activities so a previous
// run's last sample never leaks into a new run.
type ThroughputEstimator struct {
	lastBytes int64
	lastAt    time.Time
	hasSample bool
}

// Observe ingests a cumulative-byte sample at the given clock time and returns
// the resulting rate. The supplied timestamp makes tests deterministic.
func (e *ThroughputEstimator) Observe(totalBytes int64, now time.Time) RateSample {
	if !e.hasSample {
		e.lastBytes = totalBytes
		e.lastAt = now
		e.hasSample = true
		return RateSample{Known: false}
	}
	elapsed := now.Sub(e.lastAt)
	delta := totalBytes - e.lastBytes
	// A negative byte delta (counters reset by a new rclone invocation or an
	// out-of-order frame) resets the baseline and reports unknown rather than
	// emitting a negative rate.
	if delta < 0 {
		e.lastBytes = totalBytes
		e.lastAt = now
		return RateSample{Known: false}
	}
	// Non-positive elapsed time cannot produce a meaningful rate; refresh the
	// baseline so the next valid sample measures from here.
	if elapsed <= 0 {
		e.lastBytes = totalBytes
		e.lastAt = now
		return RateSample{Known: false}
	}
	// Zero positive-time delta bytes is a known zero: the activity is live but
	// not currently producing bytes.
	if delta == 0 {
		e.lastBytes = totalBytes
		e.lastAt = now
		return RateSample{Known: true, BytesPerSecond: 0}
	}
	e.lastBytes = totalBytes
	e.lastAt = now
	// delta is measured in int64 nanoseconds via Duration; converting to float
	// seconds avoids integer truncation for sub-second intervals. Intermediate
	// arithmetic uses float64 after the int64 delta so large byte values do not
	// overflow (delta itself is int64; bytes are int64 cumulative counters).
	return RateSample{Known: true, BytesPerSecond: float64(delta) / elapsed.Seconds()}
}

// Reset clears the baseline so a new activity/run starts with an unknown speed
// (§7 rule 7 and 8).
func (e *ThroughputEstimator) Reset() {
	*e = ThroughputEstimator{}
}
