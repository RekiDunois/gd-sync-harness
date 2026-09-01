package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"knowledge-sync/internal/config"
	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/namespace"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/source"
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

// sourceFilesFromPolicy builds the active files-from list under an owned
// committed policy snapshot (§11). The sync command uploads exactly these
// paths; suppressed remote objects (absent from the list) are protected and
// survive ordinary reconciliation.
func (s *Service) sourceFilesFromPolicy(p *state.Profile, snap *policy.Snapshot) (string, []string, error) {
	paths, err := ScanActivePaths(p.SourcePath, p.MaxFileSize, snap)
	if err != nil {
		return "", nil, err
	}
	list, err := writeFilesFrom(paths)
	if err != nil {
		return "", nil, err
	}
	return list, paths, nil
}

// DryRunSyncProtected performs the preflight dry-run against the owned policy
// snapshot (§11.3). It lists active paths only (no --delete-excluded), so the
// plan reflects uploads/updates plus any explicitly supplied proven-delete
// count. rclone writes the summary to stderr, so we combine stdout+stderr for
// parsing.
func (s *Service) DryRunSyncProtected(ctx context.Context, p *state.Profile, snap *policy.Snapshot, options SyncOptions) (*PreflightResult, error) {
	list, _, err := s.sourceFilesFromPolicy(p, snap)
	if err != nil {
		return nil, err
	}
	defer os.Remove(list)

	args := []string{
		"sync",
		"--dry-run",
		"--files-from", list,
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

// FullSyncProtected runs the live non-destructive upload for the active set
// under the owned policy snapshot (§11.3). Suppressed remote objects are not
// in the files-from list and therefore survive. Proven deletions are applied
// separately via DeleteRemotePaths after budget validation.
func (s *Service) FullSyncProtected(ctx context.Context, p *state.Profile, snap *policy.Snapshot, options SyncOptions, onStats func(exec.ProgressStats)) (*PreflightResult, error) {
	list, _, err := s.sourceFilesFromPolicy(p, snap)
	if err != nil {
		return nil, err
	}
	defer os.Remove(list)

	args := []string{
		"sync",
		"--files-from", list,
		"--fast-list",
		"--track-renames",
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

// DeleteRemotePaths removes the given remote paths one by one (targeted
// deletion). It is used for proven ordinary local deletions within the delete
// budget (§11.4). It never touches suppressed objects.
func (s *Service) DeleteRemotePaths(ctx context.Context, p *state.Profile, paths []string) error {
	for _, rel := range paths {
		if err := source.ValidateCanonicalRelativeTarget(rel); err != nil {
			return fmt.Errorf("refuse malformed delete target %q: %w", rel, err)
		}
		if namespace.IsDerivedPath(rel) {
			return fmt.Errorf("refuse delete of reserved derived path %q", rel)
		}
		res := s.Rclone.Run(ctx, "deletefile", p.RemoteName+":"+p.RemoteDisplayPath+"/"+rel)
		if res.Err != nil {
			return fmt.Errorf("delete %s: %w: %s", rel, res.Err, res.StderrTrimmed())
		}
	}
	return nil
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

func effectiveDeleteLimit(p *state.Profile, o SyncOptions) int {
	if o.AllowDeletes > 0 {
		return o.AllowDeletes
	}
	return p.MaxDelete
}

// VerifyCheck runs `rclone check --one-way --size-only` (migration verification).
// It lists the committed-policy active source files so excluded source files are
// not reported as missing on the remote.
func (s *Service) VerifyCheck(ctx context.Context, p *state.Profile) error {
	snap, err := loadCommittedSnapshot(s, p)
	if err != nil {
		return err
	}
	list, _, err := s.sourceFilesFromPolicy(p, snap)
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
// It lists the committed-policy active source files.
func (s *Service) VerifyFull(ctx context.Context, p *state.Profile) error {
	snap, err := loadCommittedSnapshot(s, p)
	if err != nil {
		return err
	}
	list, _, err := s.sourceFilesFromPolicy(p, snap)
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

func (s *Service) remoteDuplicates(ctx context.Context, p *state.Profile) (bool, error) {
	args := []string{"lsjson", "--recursive"}
	args = append(args, s.RcloneConfig.GlobalArgs...)
	args = append(args, p.RemoteName+":"+p.RemoteDisplayPath)
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(res.Err, &exitErr) && exitErr.ExitCode() == 3 {
			return false, nil
		}
		return false, fmt.Errorf("duplicate check: %w: %s", res.Err, res.StderrTrimmed())
	}
	var entries []struct {
		Path  string `json:"Path"`
		Name  string `json:"Name"`
		IsDir bool   `json:"IsDir"`
	}
	if err := json.Unmarshal(res.Stdout, &entries); err != nil {
		return false, fmt.Errorf("duplicate check parse: %w", err)
	}
	counts := make(map[string]int, len(entries))
	for _, entry := range entries {
		key := entry.Path
		if key == "" {
			key = entry.Name
		}
		if key == "" {
			continue
		}
		counts[key]++
		if counts[key] > 1 {
			return true, nil
		}
	}
	return false, nil
}

// loadCommittedSnapshot reads the validated committed policy snapshot for a
// profile. Missing policy rows fail closed at the state API boundary.
func loadCommittedSnapshot(s *Service, p *state.Profile) (*policy.Snapshot, error) {
	return s.DB.GetCommittedSnapshot(p.ID)
}
