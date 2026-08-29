package exec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Rclone wraps invocations of the rclone external binary. It always passes the
// explicit config file path so launchd does not depend on interactive-shell
// environment variables (§31.2, §21 Phase B). All args are passed as separate
// argv entries; no path is ever shell-concatenated.
type Rclone struct {
	// Binary is the absolute path to the rclone executable.
	Binary string
	// ConfigPath is the explicit rclone config file.
	ConfigPath string
	// Timeout bounds every invocation.
	Timeout time.Duration
}

// Result captures a completed rclone invocation.
type Result struct {
	Stdout []byte
	Stderr []byte
	Err    error
}

// OK reports whether the invocation succeeded.
func (r Result) OK() bool { return r.Err == nil }

// StdoutTrimmed returns trimmed stdout as a string.
func (r Result) StdoutTrimmed() string { return strings.TrimSpace(string(r.Stdout)) }

// StderrTrimmed returns trimmed stderr as a string.
func (r Result) StderrTrimmed() string { return strings.TrimSpace(string(r.Stderr)) }

// baseArgs builds the common argument prefix.
func (r *Rclone) baseArgs(args ...string) []string {
	out := []string{}
	if r.ConfigPath != "" {
		out = append(out, "--config", r.ConfigPath)
	}
	return append(out, args...)
}

// Run executes rclone with the given args under ctx. The rclone config path is
// prepended automatically.
func (r *Rclone) Run(ctx context.Context, args ...string) Result {
	full := r.baseArgs(args...)
	return r.run(ctx, full)
}

// ProgressStats is a best-effort snapshot of rclone's --progress output (§10.1,
// §21). All values are operational metrics, never correctness state.
type ProgressStats struct {
	TransferredFiles int64
	TransferredBytes int64
	TotalBytes       int64 // 0 / -1 when rclone cannot report a reliable total
	Percent          float64
}

// RunProgress executes rclone with a --progress flag, streaming stats to onStats
// as they are emitted. The final Result carries the merged stdout/stderr. It is
// used for long-running uploads so progress is observable independently of the
// invoking CLI process (§10).
func (r *Rclone) RunProgress(ctx context.Context, onStats func(ProgressStats), args ...string) Result {
	full := r.baseArgs(append([]string{"--progress"}, args...)...)
	return r.runProgress(ctx, full, onStats)
}

func (r *Rclone) runProgress(ctx context.Context, args []string, onStats func(ProgressStats)) Result {
	cctx := ctx
	var cancel context.CancelFunc
	if r.Timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(cctx, r.Binary, args...)
	var stdout, stderr bytes.Buffer
	// --progress writes to stderr with \r-separated updates. We stream both to
	// buffers and additionally parse stderr incrementally.
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
		parseProgressStream(stderrPipe, &stderr, onStats)
	}()
	runErr := cmd.Wait()
	<-done
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Err: runErr}
}

// ProgressStats parse of a --progress stream. Progress lines arrive as
// `\r`-separated segments; rclone also writes final summary lines with `\n`.
func parseProgressStream(r io.Reader, sink *bytes.Buffer, onStats func(ProgressStats)) {
	br := bufio.NewReader(r)
	var pending []byte
	for {
		chunk, err := br.ReadByte()
		if err != nil {
			break
		}
		pending = append(pending, chunk)
		if chunk == '\n' {
			sink.Write(pending)
			pending = pending[:0]
			continue
		}
		if chunk == '\r' {
			line := string(pending[:len(pending)-1])
			sink.Write(pending)
			pending = pending[:0]
			if onStats != nil {
				if s, ok := parseProgressLine(line); ok {
					onStats(s)
				}
			}
		}
	}
	if len(pending) > 0 {
		sink.Write(pending)
	}
}

// parseProgressLine extracts counters from a single rclone --progress update
// such as:
//
//	Transferred:      123 / 456, 12.0 MiB, 88%, 2.1 MiB/s, ETA 0s
//
// It reports ok=false for non-progress lines. Unknown totals are represented
// explicitly (TotalBytes=0) rather than fabricated (§21).
func parseProgressLine(line string) (ProgressStats, bool) {
	if !strings.Contains(line, "Transferred:") {
		return ProgressStats{}, false
	}
	var s ProgressStats
	// Strip the leading timestamp/date prefix if present.
	idx := strings.Index(line, "Transferred:")
	if idx > 0 {
		line = line[idx:]
	}
	parts := strings.SplitN(line, "Transferred:", 2)
	if len(parts) < 2 {
		return ProgressStats{}, false
	}
	rest := strings.TrimSpace(parts[1])
	fields := strings.Fields(rest)
	if len(fields) < 4 {
		return ProgressStats{}, false
	}
	// fields: "123", "/", "456,", "12.0", "MiB,", "88%", ...
	doneN, err1 := parseLeadingInt(fields[0])
	if err1 != nil {
		return ProgressStats{}, false
	}
	s.TransferredFiles = doneN
	if fields[1] == "/" && len(fields) >= 3 {
		totalN, err2 := parseLeadingInt(fields[2])
		if err2 == nil {
			s.TotalBytes = -1 // file total unknown; the "/" may be bytes total
			_ = totalN
		}
	}
	// Try to parse the "N.NN MiB, XX%" pattern from the remaining fields.
	for i, f := range fields {
		trimmed := strings.TrimSuffix(f, ",")
		if strings.HasSuffix(trimmed, "%") {
			pct, err := parseFloat(strings.TrimSuffix(trimmed, "%"))
			if err == nil {
				s.Percent = pct
			}
			// The field before the MiB value holds the byte amount.
			if i >= 2 {
				n, err := parseFloat(fields[i-2])
				if err == nil {
					s.TransferredBytes = bytesFromSize(n, fields[i-1])
				}
			}
			break
		}
	}
	return s, true
}

// bytesFromSize converts a size magnitude + unit token to bytes.
func bytesFromSize(mag float64, unit string) int64 {
	unit = strings.TrimSuffix(unit, ",")
	mult := int64(1)
	switch strings.ToLower(unit) {
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "tib":
		mult = 1 << 40
	}
	return int64(mag * float64(mult))
}

func parseFloat(s string) (float64, error) {
	var f float64
	var err error
	fmt.Sscanf(s, "%g", &f)
	if f == 0 && !strings.ContainsAny(s, "0123456789") {
		err = fmt.Errorf("not a number")
	}
	return f, err
}

func parseLeadingInt(s string) (int64, error) {
	var n int64
	i := 0
	neg := false
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		neg = s[i] == '-'
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int64(s[i]-'0')
		i++
	}
	if i == start {
		return 0, fmt.Errorf("no digits in %q", s)
	}
	if neg {
		return -n, nil
	}
	return n, nil
}

// RunNoConfig executes rclone without injecting the config flag.
func (r *Rclone) RunNoConfig(ctx context.Context, args ...string) Result {
	return r.run(ctx, args)
}

func (r *Rclone) run(ctx context.Context, args []string) Result {
	cctx := ctx
	var cancel context.CancelFunc
	if r.Timeout > 0 {
		cctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(cctx, r.Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Err: err}
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
	p := res.StdoutTrimmed()
	lines := strings.Split(p, "\n")
	var last string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "Configuration file is stored at") {
			last = l
		}
	}
	if last == "" {
		return "", fmt.Errorf("rclone config file returned empty path")
	}
	return last, nil
}
