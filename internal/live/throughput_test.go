package live

import (
	"math"
	"testing"
	"time"
)

func TestFirstSampleUnknown(t *testing.T) {
	var e ThroughputEstimator
	s := e.Observe(1024, time.Unix(100, 0))
	if s.Known {
		t.Fatalf("first sample must be unknown, got %+v", s)
	}
}

func TestTwoSamplesExactDelta(t *testing.T) {
	var e ThroughputEstimator
	e.Observe(0, time.Unix(100, 0))
	s := e.Observe(2048, time.Unix(102, 0))
	if !s.Known {
		t.Fatal("second sample must be known")
	}
	if got := s.BytesPerSecond; math.Abs(got-1024) > 1e-9 {
		t.Fatalf("rate = %f, want 1024", got)
	}
}

func TestZeroByteDeltaIsKnownZero(t *testing.T) {
	var e ThroughputEstimator
	e.Observe(4096, time.Unix(100, 0))
	s := e.Observe(4096, time.Unix(101, 0))
	if !s.Known {
		t.Fatal("zero delta with positive time must be known")
	}
	if s.BytesPerSecond != 0 {
		t.Fatalf("rate = %f, want 0", s.BytesPerSecond)
	}
}

func TestIrregularIntervalsUseActualElapsed(t *testing.T) {
	var e ThroughputEstimator
	e.Observe(0, time.Unix(100, 0))
	// 4096 bytes over 4 seconds = 1024 B/s despite the nominal 1s cadence.
	s := e.Observe(4096, time.Unix(104, 0))
	if !s.Known || math.Abs(s.BytesPerSecond-1024) > 1e-9 {
		t.Fatalf("rate = %+v, want 1024", s)
	}
}

func TestNegativeCumulativeDeltaResetsUnknown(t *testing.T) {
	var e ThroughputEstimator
	e.Observe(8192, time.Unix(100, 0))
	s := e.Observe(4096, time.Unix(101, 0))
	if s.Known {
		t.Fatalf("negative delta must be unknown, got %+v", s)
	}
	// Baseline was reset: the next sample measures from 4096, not from 8192.
	next := e.Observe(6144, time.Unix(102, 0))
	if !next.Known || math.Abs(next.BytesPerSecond-2048) > 1e-9 {
		t.Fatalf("after reset rate = %+v, want 2048", next)
	}
}

func TestNonPositiveElapsedReturnsUnknown(t *testing.T) {
	var e ThroughputEstimator
	e.Observe(1024, time.Unix(100, 0))
	s := e.Observe(2048, time.Unix(100, 0))
	if s.Known {
		t.Fatalf("zero elapsed must be unknown, got %+v", s)
	}
	// Backwards clock.
	s2 := e.Observe(4096, time.Unix(99, 0))
	if s2.Known {
		t.Fatalf("negative elapsed must be unknown, got %+v", s2)
	}
}

func TestResetPreventsCrossRunSpike(t *testing.T) {
	var e ThroughputEstimator
	e.Observe(10_000_000, time.Unix(100, 0))
	e.Observe(10_000_000, time.Unix(101, 0))
	// New run: reset must discard the 10 MB baseline.
	e.Reset()
	first := e.Observe(0, time.Unix(200, 0))
	if first.Known {
		t.Fatal("first sample after reset must be unknown")
	}
	second := e.Observe(100, time.Unix(201, 0))
	if !second.Known || math.Abs(second.BytesPerSecond-100) > 1e-9 {
		t.Fatalf("post-reset rate = %+v, want 100", second)
	}
}

func TestLargeByteValuesNoOverflow(t *testing.T) {
	var e ThroughputEstimator
	const big = int64(math.MaxInt64 - 4096)
	e.Observe(big, time.Unix(100, 0))
	s := e.Observe(big+1024, time.Unix(101, 0))
	if !s.Known {
		t.Fatal("large delta should be known")
	}
	if math.Abs(s.BytesPerSecond-1024) > 1e-6 {
		t.Fatalf("rate = %f, want ~1024", s.BytesPerSecond)
	}
}
