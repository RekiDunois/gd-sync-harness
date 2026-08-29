package sync

import (
	"context"
	"fmt"
	"os"
	"strings"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/state"
)

// Service performs fast-path upserts and full reconciliation through rclone.
type Service struct {
	Rclone *exec.Rclone
	DB     *state.DB
}

// New builds the sync service.
func New(r *exec.Rclone, db *state.DB) *Service { return &Service{Rclone: r, DB: db} }

// FastUpsert uploads only the given changed files using a files-from targeted
// copy (§12). It never deletes.
func (s *Service) FastUpsert(ctx context.Context, p *state.Profile, files []string) error {
	if len(files) == 0 {
		return nil
	}
	list, err := writeFilesFrom(files)
	if err != nil {
		return err
	}
	defer os.Remove(list)

	args := []string{
		"copy",
		"--files-from", list,
		"--no-traverse",
		"--fast-list",
		"--transfers", "4",
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("fast upsert: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}

// DryRunSync performs rclone sync in dry-run mode to compute the destructive
// plan without mutating the remote. rclone writes the summary to stderr, so we
// combine stdout+stderr for parsing.
func (s *Service) DryRunSync(ctx context.Context, p *state.Profile, options SyncOptions) (*PreflightResult, error) {
	args := []string{
		"sync",
		"--dry-run",
		"--fast-list",
		"--track-renames",
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return nil, fmt.Errorf("preflight dry-run: %w: %s", res.Err, res.StderrTrimmed())
	}
	return parseDryRun(string(res.Stdout) + "\n" + string(res.Stderr))
}

// parseDryRun interprets the rclone sync summary output.
func parseDryRun(out string) (*PreflightResult, error) {
	res := &PreflightResult{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "Transferred:") {
			res.ToCopy = parseCount(l)
		}
		if strings.HasPrefix(l, "Deleted:") {
			res.ToDelete = parseCount(l)
		}
	}
	return res, nil
}

func parseCount(line string) int {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	for _, f := range fields {
		n, err := parseLeadingInt(f)
		if err == nil {
			return n
		}
	}
	return 0
}

func parseLeadingInt(s string) (int, error) {
	var n int
	sign := 1
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		if s[i] == '-' {
			sign = -1
		}
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + int(s[i]-'0')
		i++
	}
	if i == start {
		return 0, fmt.Errorf("no digits")
	}
	return sign * n, nil
}

// FullSync runs the live destructive reconciliation (§15).
func (s *Service) FullSync(ctx context.Context, p *state.Profile, options SyncOptions) error {
	args := []string{
		"sync",
		"--fast-list",
		"--track-renames",
		fmt.Sprintf("--max-delete=%d", effectiveDeleteLimit(p, options)),
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("full sync: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}

func effectiveDeleteLimit(p *state.Profile, o SyncOptions) int {
	if o.AllowDeletes > 0 {
		return o.AllowDeletes
	}
	return p.MaxDelete
}

// VerifyCheck runs `rclone check --one-way --size-only` (migration verification).
func (s *Service) VerifyCheck(ctx context.Context, p *state.Profile) error {
	args := []string{
		"check", "--one-way", "--size-only",
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("verify check: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}

// VerifyFull runs a full hash-level check (`rclone check --one-way`), §27.
func (s *Service) VerifyFull(ctx context.Context, p *state.Profile) error {
	args := []string{
		"check", "--one-way",
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("verify full: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}
