package exec

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestParseProgressLine(t *testing.T) {
	line := "2026/08/30 01:00:00   Transferred:      123 / 456, 12.0 MiB, 88%, 2.1 MiB/s, ETA 0s"
	s, ok := parseProgressLine(line)
	if !ok {
		t.Fatal("expected progress line to parse")
	}
	if s.TransferredFiles != 123 {
		t.Errorf("files = %d, want 123", s.TransferredFiles)
	}
	if s.TransferredBytes != 12*(1<<20) {
		t.Errorf("bytes = %d, want %d", s.TransferredBytes, 12*(1<<20))
	}
	if s.Percent != 88 {
		t.Errorf("percent = %v, want 88", s.Percent)
	}
}

func TestParseProgressLineNonProgress(t *testing.T) {
	if _, ok := parseProgressLine("INFO: some log line"); ok {
		t.Fatal("log line must not parse as progress")
	}
}

func TestParseProgressStreamSplitsCarriageReturns(t *testing.T) {
	// rclone emits \r-separated updates on stderr followed by a final \n summary.
	raw := "Transferred:      1 / 2, 1.0 KiB, 50%, 1.0 KiB/s, ETA 0s\r" +
		"Transferred:      2 / 2, 2.0 KiB, 100%, 1.0 KiB/s, ETA 0s\r" +
		"2026/08/30 01:00:00 INFO: finished\n"
	var stats []ProgressStats
	var sink bytes.Buffer
	parseProgressStream(strings.NewReader(raw), &sink, func(s ProgressStats) {
		stats = append(stats, s)
	})
	if len(stats) != 2 {
		t.Fatalf("got %d stats, want 2", len(stats))
	}
	if stats[0].TransferredFiles != 1 || stats[1].TransferredFiles != 2 {
		t.Fatalf("stats = %+v", stats)
	}
	if !strings.Contains(sink.String(), "finished") {
		t.Fatalf("final newline summary not captured: %q", sink.String())
	}
}

func TestRunProgressRunsRclone(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone not installed")
	}
}
