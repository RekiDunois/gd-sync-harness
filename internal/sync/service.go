package sync

import (
	"context"
	"fmt"
	"os"
	"strings"

	"knowledge-sync/internal/config"
	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/state"
)

// Service performs fast-path upserts and full reconciliation through rclone.
type Service struct {
	Rclone       *exec.Rclone
	DB           *state.DB
	RcloneConfig config.RcloneConfig
}

// New builds the sync service.
func New(r *exec.Rclone, db *state.DB, cfg ...config.RcloneConfig) *Service {
	c := config.Default().Rclone
	if len(cfg) > 0 {
		c = cfg[0]
	}
	return &Service{Rclone: r, DB: db, RcloneConfig: c}
}

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
	}
	args = append(args, config.ArgsFor(config.Config{Rclone: s.RcloneConfig}, config.FastUpsert)...)
	args = append(args, p.SourcePath, p.RemoteName+":"+p.RemoteDisplayPath)
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("fast upsert: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}

// sourceFilesFrom builds a files-from list containing exactly the eligible
// source files under the structured filter (§10.1). It is used by both dry-run
// and live full reconciliation so the mirror honors excludes/max-size and, with
// sync semantics, removes previously-mirrored files that became excluded (§17).
func (s *Service) sourceFilesFrom(p *state.Profile) (string, []string, error) {
	scan, err := ScanLocal(p)
	if err != nil {
		return "", nil, err
	}
	paths := make([]string, 0, len(scan.Entries))
	for _, e := range scan.Entries {
		paths = append(paths, e.RelPath)
	}
	list, err := writeFilesFrom(paths)
	if err != nil {
		return "", nil, err
	}
	return list, paths, nil
}

// DryRunSync performs rclone sync in dry-run mode to compute the destructive
// plan without mutating the remote. It uses the structured-filter files-from
// list so the plan reflects the true mirror semantics. rclone writes the
// summary to stderr, so we combine stdout+stderr for parsing.
func (s *Service) DryRunSync(ctx context.Context, p *state.Profile, options SyncOptions) (*PreflightResult, error) {
	list, _, err := s.sourceFilesFrom(p)
	if err != nil {
		return nil, err
	}
	defer os.Remove(list)

	args := []string{
		"sync",
		"--dry-run",
		"--files-from", list,
		"--delete-excluded",
		"--fast-list",
		"--track-renames",
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	args = append(args[:len(args)-2], config.ArgsFor(config.Config{Rclone: s.RcloneConfig}, config.DryRun)...)
	args = append(args, p.SourcePath, p.RemoteName+":"+p.RemoteDisplayPath)
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

// FullSync runs the live destructive reconciliation (§15). It uses the same
// structured-filter files-from list as preflight so excludes and max-size are
// enforced on the source side and excluded remote objects are removed (§17).
func (s *Service) FullSync(ctx context.Context, p *state.Profile, options SyncOptions) error {
	_, err := s.fullSync(ctx, p, options, nil)
	return err
}

// FullSyncProgress is like FullSync but streams best-effort transfer progress
// (§10.1). onStats may be nil.
func (s *Service) FullSyncProgress(ctx context.Context, p *state.Profile, options SyncOptions, onStats func(exec.ProgressStats)) error {
	_, err := s.fullSync(ctx, p, options, onStats)
	return err
}

func (s *Service) fullSync(ctx context.Context, p *state.Profile, options SyncOptions, onStats func(exec.ProgressStats)) (*PreflightResult, error) {
	list, _, err := s.sourceFilesFrom(p)
	if err != nil {
		return nil, err
	}
	defer os.Remove(list)

	args := []string{
		"sync",
		"--files-from", list,
		"--delete-excluded",
		"--fast-list",
		"--track-renames",
		fmt.Sprintf("--max-delete=%d", effectiveDeleteLimit(p, options)),
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	args = append(args[:len(args)-2], config.ArgsFor(config.Config{Rclone: s.RcloneConfig}, config.FullSync)...)
	args = append(args, p.SourcePath, p.RemoteName+":"+p.RemoteDisplayPath)
	res := s.Rclone.RunProgress(ctx, onStats, args...)
	if res.Err != nil {
		return nil, fmt.Errorf("full sync: %w: %s", res.Err, res.StderrTrimmed())
	}
	return &PreflightResult{}, nil
}

func effectiveDeleteLimit(p *state.Profile, o SyncOptions) int {
	if o.AllowDeletes > 0 {
		return o.AllowDeletes
	}
	return p.MaxDelete
}

// VerifyCheck runs `rclone check --one-way --size-only` (migration verification).
// It applies the structured filter (§10.1) so excluded source files are not
// reported as missing on the remote.
func (s *Service) VerifyCheck(ctx context.Context, p *state.Profile) error {
	list, _, err := s.sourceFilesFrom(p)
	if err != nil {
		return err
	}
	defer os.Remove(list)

	args := []string{
		"check", "--one-way", "--size-only",
		"--files-from", list,
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	args = append(args[:len(args)-2], config.ArgsFor(config.Config{Rclone: s.RcloneConfig}, config.Verify)...)
	args = append(args, p.SourcePath, p.RemoteName+":"+p.RemoteDisplayPath)
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("verify check: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}

// VerifyFull runs a full hash-level check (`rclone check --one-way`), §27.
// It applies the structured filter (§10.1).
func (s *Service) VerifyFull(ctx context.Context, p *state.Profile) error {
	list, _, err := s.sourceFilesFrom(p)
	if err != nil {
		return err
	}
	defer os.Remove(list)

	args := []string{
		"check", "--one-way",
		"--files-from", list,
		p.SourcePath,
		p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	args = append(args[:len(args)-2], config.ArgsFor(config.Config{Rclone: s.RcloneConfig}, config.Verify)...)
	args = append(args, p.SourcePath, p.RemoteName+":"+p.RemoteDisplayPath)
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("verify full: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}
