package profile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/filter"
	"knowledge-sync/internal/remote"
	"knowledge-sync/internal/sidecar"
	"knowledge-sync/internal/state"
)

// Service implements the profile lifecycle (§9).
type Service struct {
	DB     *state.DB
	Rclone *exec.Rclone
	Remote *remote.Manager
}

// NewService builds the profile service.
func NewService(db *state.DB, r *exec.Rclone, rm *remote.Manager) *Service {
	return &Service{DB: db, Rclone: r, Remote: rm}
}

// AddOptions are the configurable inputs to `profile add`.
type AddOptions struct {
	ID          string
	SourcePath  string
	RemoteName  string
	RemotePath  string
	Type        string
	MaxDelete   int
	MaxFileSize int64
	DryRun      bool
}

// Add validates and creates a new profile, performing the managed root creation
// (§9.1). Returns the created profile.
func (s *Service) Add(ctx context.Context, o AddOptions) (*state.Profile, error) {
	if err := state.ValidateID(o.ID); err != nil {
		return nil, err
	}
	if _, err := s.DB.GetProfile(o.ID); err == nil {
		return nil, state.ErrIDExists
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, err
	}
	all, _ := s.DB.ListProfiles()
	for _, p := range all {
		if p.ID == o.ID && p.Tombstoned {
			return nil, state.ErrIDTombstoned
		}
	}

	fi, err := os.Stat(o.SourcePath)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("source %q is not a directory", o.SourcePath)
	}
	abs, err := filepath.Abs(o.SourcePath)
	if err != nil {
		return nil, err
	}

	typ := strings.ToLower(o.Type)
	switch typ {
	case "obsidian":
		if _, err := os.Stat(filepath.Join(abs, ".obsidian")); err != nil {
			return nil, fmt.Errorf("obsidian profile requires .obsidian directory in source")
		}
	case "generic":
	default:
		return nil, fmt.Errorf("unsupported profile type %q (want obsidian or generic)", o.Type)
	}

	if err := s.validateOverlap(abs, o.RemoteName, o.RemotePath); err != nil {
		return nil, err
	}

	if _, err := s.Remote.ValidateRemote(ctx, o.RemoteName); err != nil {
		return nil, err
	}

	uuid := newUUID()

	if o.DryRun {
		p := &state.Profile{
			ID: o.ID, ProfileUUID: uuid, Type: typ, SourcePath: abs,
			RemoteName: o.RemoteName, RemoteFolderID: "", RemoteDisplayPath: o.RemotePath,
			Enabled: true, MaxDelete: o.MaxDelete, MaxFileSize: o.MaxFileSize,
		}
		return p, nil
	}

	folderID, err := s.Remote.CreateManagedRoot(ctx, o.RemoteName, o.RemotePath)
	if err != nil {
		return nil, err
	}

	maxSize := o.MaxFileSize
	if maxSize == 0 {
		maxSize = 512 << 20
	}
	maxDel := o.MaxDelete
	if maxDel == 0 {
		maxDel = 100
	}

	p := &state.Profile{
		ID: o.ID, ProfileUUID: uuid, Type: typ, SourcePath: abs,
		RemoteName: o.RemoteName, RemoteFolderID: folderID, RemoteDisplayPath: o.RemotePath,
		Enabled: true, MaxDelete: maxDel, MaxFileSize: maxSize,
	}

	sc := sidecar.Create(p)
	if err := sidecar.Write(ctx, s.Rclone, o.RemoteName, sc); err != nil {
		return nil, err
	}

	if err := s.DB.CreateProfile(p); err != nil {
		return nil, err
	}

	if typ == "obsidian" {
		for _, rule := range obsidianDefaults {
			if err := s.DB.AddExclude(o.ID, rule[0], rule[1]); err != nil {
				return nil, err
			}
		}
	}

	if err := s.initialCopy(ctx, p); err != nil {
		return nil, fmt.Errorf("initial copy: %w", err)
	}

	return p, nil
}

var obsidianDefaults = [][2]string{
	{state.RuleExcludeDirName, ".obsidian"},
	{state.RuleExcludeDirName, ".git"},
	{state.RuleExcludeDirName, ".trash"},
	{state.RuleExcludeDirName, "Private"},
	{state.RuleExcludeFileName, ".DS_Store"},
	{state.RuleExcludeExtension, "tmp"},
	{state.RuleExcludeExtension, "swp"},
	{state.RuleExcludeExtension, "lock"},
	{state.RuleExcludeExtension, "mp4"},
	{state.RuleExcludeExtension, "mov"},
}

// validateOverlap ensures no two active profiles share nested local sources or
// nested remote mirrors on the same storage owner (§3 invariants 7, 8).
func (s *Service) validateOverlap(srcAbs, remoteName, remotePath string) error {
	all, _ := s.DB.ActiveProfiles()
	for _, p := range all {
		pAbs, _ := filepath.Abs(p.SourcePath)
		if hasOverlap(srcAbs, pAbs) {
			return fmt.Errorf("local source %q overlaps active profile %q source %q", srcAbs, p.ID, pAbs)
		}
		if p.RemoteName == remoteName && hasOverlap(remotePath, p.RemoteDisplayPath) {
			return fmt.Errorf("remote path %q overlaps profile %q path %q on remote %q", remotePath, p.ID, p.RemoteDisplayPath, remoteName)
		}
	}
	return nil
}

func hasOverlap(a, b string) bool {
	ca := strings.TrimSuffix(strings.TrimSuffix(filepath.ToSlash(a), "/"), "\\")
	cb := strings.TrimSuffix(strings.TrimSuffix(filepath.ToSlash(b), "/"), "\\")
	return ca == cb || strings.HasPrefix(cb, ca+"/") || strings.HasPrefix(ca, cb+"/")
}

// initialCopy performs the initial non-destructive copy-mode rollout (§21 Phase D).
func (s *Service) initialCopy(ctx context.Context, p *state.Profile) error {
	eng := filter.FromProfile(p)

	var lines []string
	err := filepath.Walk(p.SourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == p.SourcePath {
			return nil
		}
		rel := filter.NormalizeRelPath(p.SourcePath, path)
		if rel == "" {
			return nil
		}
		if info.IsDir() {
			if excluded, _ := eng.Excluded(rel); excluded {
				return filepath.SkipDir
			}
			return nil
		}
		if excluded, _ := eng.Excluded(rel); excluded {
			return nil
		}
		if filter.IsSymlink(path) {
			return nil
		}
		if eng.OverSize(info.Size()) {
			return nil
		}
		lines = append(lines, rel)
		return nil
	})
	if err != nil {
		return err
	}

	return s.copyFiles(ctx, p, lines)
}

// copyFiles runs rclone copy with a files-from list (no remote scan needed).
func (s *Service) copyFiles(ctx context.Context, p *state.Profile, files []string) error {
	if len(files) == 0 {
		return nil
	}
	tmp, err := os.CreateTemp("", "ks-files-*.txt")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	for _, f := range files {
		if _, err := fmt.Fprintln(tmp, f); err != nil {
			tmp.Close()
			return err
		}
	}
	tmp.Close()

	args := []string{
		"copy", "--files-from", tmp.Name(),
		"--no-traverse", "--transfers", "4",
		p.SourcePath, p.RemoteName + ":" + p.RemoteDisplayPath,
	}
	res := s.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("initial copy: %w: %s", res.Err, res.StderrTrimmed())
	}
	return nil
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("profile-%d", state.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:])
}
