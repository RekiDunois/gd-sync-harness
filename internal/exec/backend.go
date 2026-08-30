package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// NewRclone constructs an Rclone wrapper with sensible defaults.
func NewRclone(binary, configPath string) *Rclone {
	return &Rclone{
		Binary:     binary,
		ConfigPath: configPath,
		// Long data-plane operations are governed by rclone's transport
		// timeouts. Callers may set Timeout explicitly for short probes.
		Timeout: 0,
	}
}

// Feature describes a rclone backend feature.
type Feature struct {
	Name     string `json:"Name"`
	Root     string `json:"Root"`
	String   string `json:"String"`
	Hashes   []string
	Features struct {
		About bool `json:"About"`
	} `json:"Features"`
}

// RemoteBackend returns the backend type for a remote. rclone's `backend
// features` JSON has `Name` = remote name (not the backend type), so we parse
// `rclone config show <remote>` which contains a `type = <backend>` line.
func RemoteBackend(ctx context.Context, r *Rclone, remote string) (string, error) {
	name := strings.TrimSuffix(remote, ":")
	res := r.Run(ctx, "config", "show", name)
	if res.Err != nil {
		return "", fmt.Errorf("rclone config show %s: %w: %s", name, res.Err, res.StderrTrimmed())
	}
	// Parse the `type = <backend>` line from the config section.
	for _, line := range strings.Split(res.StdoutTrimmed(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "type = ") {
			backend := strings.TrimSpace(strings.TrimPrefix(line, "type = "))
			if backend == "" {
				continue
			}
			return strings.ToLower(backend), nil
		}
	}
	return "", fmt.Errorf("could not determine backend type for remote %q", name)
}

// ListRemotes returns remote names via `rclone listremotes`.
func ListRemotes(ctx context.Context, r *Rclone) ([]string, error) {
	res := r.Run(ctx, "listremotes")
	if res.Err != nil {
		return nil, fmt.Errorf("rclone listremotes: %w: %s", res.Err, res.StderrTrimmed())
	}
	var out []string
	for _, line := range strings.Split(res.StdoutTrimmed(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// Quota is the `rclone about` output.
type Quota struct {
	Total   int64 `json:"Total"`
	Used    int64 `json:"Used"`
	Free    int64 `json:"Free"`
	Trashed int64 `json:"Trashed"`
	Other   int64 `json:"Other"`
}

// About returns quota info for a remote via `rclone about --json <remote>:`.
func About(ctx context.Context, r *Rclone, remote string) (*Quota, error) {
	res := r.Run(ctx, "about", "--json", strings.TrimSuffix(remote, ":")+":")
	if res.Err != nil {
		return nil, fmt.Errorf("rclone about %s: %w: %s", remote, res.Err, res.StderrTrimmed())
	}
	var q Quota
	if err := json.Unmarshal(res.Stdout, &q); err != nil {
		return nil, fmt.Errorf("parse about output for %s: %w", remote, err)
	}
	return &q, nil
}
