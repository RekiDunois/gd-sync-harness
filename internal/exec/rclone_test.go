package exec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseJSONProgressLine(t *testing.T) {
	line := `{"level":"notice","msg":"stats","stats":{"bytes":1024,"totalBytes":4096,"checks":3,"totalChecks":8,"transfers":2,"totalTransfers":4,"listed":9,"errors":0,"speed":128.5,"transferring":[{"name":"docs/example.md","bytes":12,"size":20}]}}`
	s, ok := parseJSONProgressLine([]byte(line))
	if !ok {
		t.Fatal("expected structured stats to parse")
	}
	if !s.BytesKnown || s.Bytes != 1024 || !s.TotalBytesKnown || s.TotalBytes != 4096 {
		t.Fatalf("byte stats = %+v", s)
	}
	if !s.ChecksKnown || s.Checks != 3 || !s.ListedKnown || s.Listed != 9 {
		t.Fatalf("comparison stats = %+v", s)
	}
	if s.CurrentItem != "docs/example.md" || !s.CurrentItemBytesKnown || s.CurrentItemBytes != 12 {
		t.Fatalf("current item = %+v", s)
	}
}

func TestJSONLogWithoutStatsIsNotProgress(t *testing.T) {
	if _, ok := parseJSONProgressLine([]byte(`{"level":"info","msg":"finished"}`)); ok {
		t.Fatal("ordinary JSON log must not be treated as stats")
	}
	if _, ok := parseJSONProgressLine([]byte("not json")); ok {
		t.Fatal("invalid JSON must not be treated as stats")
	}
}

func TestParseJSONProgressStreamRetainsLogs(t *testing.T) {
	raw := "{\"level\":\"notice\",\"stats\":{\"bytes\":1,\"transfers\":1}}\n" +
		"{\"level\":\"info\",\"msg\":\"finished\"}\n"
	var stats []ProgressStats
	var sink bytes.Buffer
	parseJSONProgressStream(strings.NewReader(raw), &sink, func(s ProgressStats) { stats = append(stats, s) })
	if len(stats) != 1 || stats[0].Bytes != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if !strings.Contains(sink.String(), "finished") {
		t.Fatalf("ordinary log was not retained: %q", sink.String())
	}
}

func TestMeasurableProgressIgnoresErrorsOnly(t *testing.T) {
	previous := &ProgressStats{Bytes: 10, BytesKnown: true, Errors: 1, ErrorsKnown: true}
	current := *previous
	current.Errors = 2
	if current.MeasurableProgress(previous) {
		t.Fatal("error-only change must not be progress")
	}
	current.Bytes = 11
	if !current.MeasurableProgress(previous) {
		t.Fatal("byte increase must be progress")
	}
}

func TestContextCauseIsPreserved(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "slow")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel(context.Canceled)
	}()
	result := (&Rclone{Binary: bin}).Run(ctx, "noop")
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", result.Err)
	}
}

func TestNewRcloneHasNoWallClockTimeout(t *testing.T) {
	if got := NewRclone("example-rclone", "").Timeout; got != 0 {
		t.Fatalf("default timeout = %s, want uncapped", got)
	}
}
