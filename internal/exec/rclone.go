package exec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Rclone wraps invocations of the rclone external binary. It always passes the
// explicit config file path so launchd does not depend on interactive-shell
// environment variables. Arguments are never shell-concatenated.
type Rclone struct {
	Binary     string
	ConfigPath string
	// Timeout is optional and should only be set for short control operations.
	// A zero value leaves long data-plane operations uncapped.
	Timeout time.Duration
}

// Result captures a completed rclone invocation.
type Result struct {
	Stdout []byte
	Stderr []byte
	Err    error
}

func (r Result) OK() bool              { return r.Err == nil }
func (r Result) StdoutTrimmed() string { return strings.TrimSpace(string(r.Stdout)) }
func (r Result) StderrTrimmed() string { return strings.TrimSpace(string(r.Stderr)) }

func (r *Rclone) baseArgs(args ...string) []string {
	out := []string{}
	if r.ConfigPath != "" {
		out = append(out, "--config", r.ConfigPath)
	}
	return append(out, args...)
}

// Run executes rclone with the given args under ctx.
func (r *Rclone) Run(ctx context.Context, args ...string) Result {
	return r.run(ctx, r.baseArgs(args...))
}

// ProgressStats is a best-effort snapshot of rclone's structured stats output.
// The *Known fields distinguish an unavailable metric from a measured zero.
type ProgressStats struct {
	Bytes, TotalBytes                           int64
	Checks, TotalChecks                         int64
	Transfers, TotalTransfers                   int64
	Listed, Deletes, Errors                     int64
	Speed                                       float64
	CurrentItem                                 string
	CurrentItemBytes, CurrentItemSize           int64
	BytesKnown, TotalBytesKnown                 bool
	ChecksKnown, TotalChecksKnown               bool
	TransfersKnown, TotalTransfersKnown         bool
	ListedKnown, DeletesKnown, ErrorsKnown      bool
	SpeedKnown, CurrentItemKnown                bool
	CurrentItemBytesKnown, CurrentItemSizeKnown bool
}

// MeasurableProgress reports whether work advanced between two stats frames.
// Repeated frames and error-only changes are deliberately not progress.
func (s ProgressStats) MeasurableProgress(previous *ProgressStats) bool {
	if previous == nil {
		return (s.BytesKnown && s.Bytes > 0) || (s.ChecksKnown && s.Checks > 0) ||
			(s.TransfersKnown && s.Transfers > 0) || (s.ListedKnown && s.Listed > 0) ||
			(s.DeletesKnown && s.Deletes > 0) || (s.CurrentItemKnown && s.CurrentItem != "")
	}
	return (s.BytesKnown && previous.BytesKnown && s.Bytes > previous.Bytes) ||
		(s.ChecksKnown && previous.ChecksKnown && s.Checks > previous.Checks) ||
		(s.TransfersKnown && previous.TransfersKnown && s.Transfers > previous.Transfers) ||
		(s.ListedKnown && previous.ListedKnown && s.Listed > previous.Listed) ||
		(s.DeletesKnown && previous.DeletesKnown && s.Deletes > previous.Deletes) ||
		(s.CurrentItemKnown && (!previous.CurrentItemKnown || s.CurrentItem != previous.CurrentItem)) ||
		(s.CurrentItemBytesKnown && previous.CurrentItemBytesKnown && s.CurrentItemBytes > previous.CurrentItemBytes)
}

// RunProgress executes a long rclone operation with NDJSON telemetry. Ordinary
// JSON log lines remain in stderr; only records containing a structured stats
// object reach onStats.
func (r *Rclone) RunProgress(ctx context.Context, onStats func(ProgressStats), args ...string) Result {
	flags := []string{"--use-json-log", "--stats", "10s", "--stats-log-level", "NOTICE"}
	full := r.baseArgs(append(flags, args...)...)
	return r.runProgress(ctx, full, onStats)
}

func (r *Rclone) runProgress(ctx context.Context, args []string, onStats func(ProgressStats)) Result {
	cctx, cancel := r.operationContext(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return Result{Err: err}
	}
	if err := cmd.Start(); err != nil {
		return Result{Err: err}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		parseJSONProgressStream(stderrPipe, &stderr, onStats)
	}()
	runErr := cmd.Wait()
	<-done
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Err: commandError(cctx, args, runErr)}
}

func parseJSONProgressStream(r io.Reader, sink *bytes.Buffer, onStats func(ProgressStats)) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := append([]byte(scanner.Bytes()), '\n')
		sink.Write(line)
		if onStats != nil {
			if stats, ok := parseJSONProgressLine(line); ok {
				onStats(stats)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_, _ = fmt.Fprintf(sink, "progress reader: %v\n", err)
	}
}

func parseJSONProgressLine(line []byte) (ProgressStats, bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(line), &envelope); err != nil {
		return ProgressStats{}, false
	}
	raw, ok := envelope["stats"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ProgressStats{}, false
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return ProgressStats{}, false
	}
	var s ProgressStats
	s.Bytes, s.BytesKnown = jsonInt(values, "bytes")
	s.TotalBytes, s.TotalBytesKnown = jsonInt(values, "totalBytes")
	s.Checks, s.ChecksKnown = jsonInt(values, "checks")
	s.TotalChecks, s.TotalChecksKnown = jsonInt(values, "totalChecks")
	s.Transfers, s.TransfersKnown = jsonInt(values, "transfers")
	s.TotalTransfers, s.TotalTransfersKnown = jsonInt(values, "totalTransfers")
	s.Listed, s.ListedKnown = jsonInt(values, "listed")
	s.Deletes, s.DeletesKnown = jsonInt(values, "deletes")
	s.Errors, s.ErrorsKnown = jsonInt(values, "errors")
	s.Speed, s.SpeedKnown = jsonFloat(values, "speed")
	rawTransfers, exists := values["transferring"]
	if !exists {
		rawTransfers, exists = values["currentTransfers"]
	}
	if exists {
		var transfers []map[string]json.RawMessage
		if json.Unmarshal(rawTransfers, &transfers) == nil && len(transfers) > 0 {
			item := transfers[0]
			if rawName, exists := item["name"]; exists {
				_ = json.Unmarshal(rawName, &s.CurrentItem)
			}
			if s.CurrentItem == "" {
				if rawPath, exists := item["path"]; exists {
					_ = json.Unmarshal(rawPath, &s.CurrentItem)
				}
			}
			s.CurrentItemKnown = s.CurrentItem != ""
			s.CurrentItemBytes, s.CurrentItemBytesKnown = jsonInt(item, "bytes")
			s.CurrentItemSize, s.CurrentItemSizeKnown = jsonInt(item, "size")
		}
	}
	return s, true
}

func jsonInt(values map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := values[key]
	if !ok {
		return 0, false
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	return 0, false
}

func jsonFloat(values map[string]json.RawMessage, key string) (float64, bool) {
	raw, ok := values[key]
	if !ok {
		return 0, false
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n, true
	}
	return 0, false
}

// RunNoConfig executes rclone without injecting the config flag.
func (r *Rclone) RunNoConfig(ctx context.Context, args ...string) Result {
	return r.run(ctx, args)
}

func (r *Rclone) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.Timeout > 0 {
		return context.WithTimeout(ctx, r.Timeout)
	}
	return context.WithCancel(ctx)
}

// CommandError preserves the subprocess exit code while retaining the
// underlying error for errors.Is/As and context cancellation checks.
type CommandError struct {
	Args []string
	Err  error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("rclone %s: %v", strings.Join(e.Args, " "), e.Err)
}
func (e *CommandError) Unwrap() error { return e.Err }
func (e *CommandError) ExitCode() int {
	var exitErr *exec.ExitError
	if errors.As(e.Err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func commandError(ctx context.Context, args []string, err error) error {
	if err == nil {
		return nil
	}
	wrapped := error(&CommandError{Args: append([]string(nil), args...), Err: err})
	if ctx.Err() != nil {
		return errors.Join(context.Cause(ctx), wrapped)
	}
	return wrapped
}

func (r *Rclone) run(ctx context.Context, args []string) Result {
	cctx, cancel := r.operationContext(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Err: commandError(cctx, args, err)}
}

// JSON runs rclone with args and unmarshals stdout into v.
func (r *Rclone) JSON(ctx context.Context, v any, args ...string) error {
	res := r.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("rclone %s: %w: %s", strings.Join(args, " "), res.Err, res.StderrTrimmed())
	}
	if err := json.Unmarshal(res.Stdout, v); err != nil {
		return fmt.Errorf("rclone %s: parse JSON: %w", strings.Join(args, " "), err)
	}
	return nil
}

// DiscoverConfigPath resolves the rclone config location using `rclone config file`.
func (r *Rclone) DiscoverConfigPath(ctx context.Context) (string, error) {
	res := r.RunNoConfig(ctx, "config", "file")
	if res.Err != nil {
		return "", fmt.Errorf("rclone config file: %w", res.Err)
	}
	var last string
	for _, line := range strings.Split(res.StdoutTrimmed(), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "Configuration file is stored at") {
			last = line
		}
	}
	if last == "" {
		return "", fmt.Errorf("rclone config file returned empty path")
	}
	return last, nil
}
