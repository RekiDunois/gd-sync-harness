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

// FileRateSample is the output of a per-count (file) rate estimate. It mirrors
// the byte RateSample shape but expresses files per second.
type FileRateSample struct {
	Known       bool
	FilesPerSec float64
}

// FileRateEstimator computes whole-activity file throughput from the latest two
// cumulative file-count samples, mirroring the two-sample throughput approach
// used for byte Speed (§7). The first sample only establishes a baseline; a
// measured zero is distinct from an unknown rate. Reset between activities so a
// prior run's last sample never leaks into a new run.
type FileRateEstimator struct {
	lastFiles int64
	lastAt    time.Time
	hasSample bool
}

// Observe ingests a cumulative file-count sample at the given clock time and
// returns the resulting rate. The supplied timestamp makes tests deterministic.
func (e *FileRateEstimator) Observe(totalFiles int64, now time.Time) FileRateSample {
	if !e.hasSample {
		e.lastFiles = totalFiles
		e.lastAt = now
		e.hasSample = true
		return FileRateSample{Known: false}
	}
	elapsed := now.Sub(e.lastAt)
	delta := totalFiles - e.lastFiles
	// A negative delta (counters reset by a new rclone invocation or an
	// out-of-order frame) resets the baseline and reports unknown.
	if delta < 0 {
		e.lastFiles = totalFiles
		e.lastAt = now
		return FileRateSample{Known: false}
	}
	// Non-positive elapsed time cannot produce a meaningful rate; refresh the
	// baseline so the next valid sample measures from here.
	if elapsed <= 0 {
		e.lastFiles = totalFiles
		e.lastAt = now
		return FileRateSample{Known: false}
	}
	// Zero positive-time delta files is a known zero: the activity is live but
	// not currently completing files.
	if delta == 0 {
		e.lastFiles = totalFiles
		e.lastAt = now
		return FileRateSample{Known: true, FilesPerSec: 0}
	}
	e.lastFiles = totalFiles
	e.lastAt = now
	return FileRateSample{Known: true, FilesPerSec: float64(delta) / elapsed.Seconds()}
}

// Reset clears the baseline so a new activity/run starts with an unknown rate.
func (e *FileRateEstimator) Reset() {
	*e = FileRateEstimator{}
}
