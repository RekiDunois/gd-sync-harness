package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
