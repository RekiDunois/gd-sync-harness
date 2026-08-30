// Package policy implements the durable committed .gitignore policy snapshot,
// the Git-compatible matcher used by the worker, and repository-local-only
// stable snapshot collection.
//
// The profile's source root is the logical Gitignore root even when it is not a
// Git repository. Only repository-local root/nested .gitignore files
// participate: .git/info/exclude and global excludes are never loaded, and the
// runtime never requires the git executable (§0.1, §3, §4 of the policy plan).
package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/git-pkgs/gitignore"
)

// PolicySource values.
const (
	SourceGitignore      = "gitignore"
	SourceLegacyMigrated = "legacy_migrated"
)

// Reserved source name for the synthetic migrated legacy snapshot. It is not a
// filesystem path and is only used as the snapshot file's relative identity.
const LegacySourceName = "__legacy_migrated__"

// RefreshState values (§8.2).
const (
	RefreshPending = "pending"
	RefreshRunning = "running"
	RefreshReady   = "ready"
	RefreshError   = "error"
)

// File is one captured .gitignore file within a snapshot.
type File struct {
	// RelativePath is the slash-separated path relative to the source root,
	// e.g. ".gitignore" or "src/.gitignore".
	RelativePath string
	// ScopeDir is the slash-separated directory scope for the matcher, e.g.
	// "" for root or "src" for src/.gitignore.
	ScopeDir string
	// Content is the exact file bytes.
	Content []byte
}

// PatternWarning records an invalid pattern surfaced by the matcher library.
type PatternWarning struct {
	RelativePath string `json:"relative_path"`
	Line         int    `json:"line"`
	Pattern      string `json:"pattern"`
	Message      string `json:"message"`
}

// IgnoreSnapshot is the stable committed policy snapshot (§5.2).
type IgnoreSnapshot struct {
	Files    []File
	Warnings []PatternWarning
}

// Snapshot is a stable committed policy snapshot.
type Snapshot = IgnoreSnapshot

// Hash is the canonical content-derived fingerprint of the snapshot.
func (s *IgnoreSnapshot) Hash() string {
	if s == nil {
		return "empty"
	}
	h := sha256.New()
	for _, f := range s.Files {
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", f.RelativePath, f.ScopeDir, len(f.Content))
		h.Write(f.Content)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FilesInScopeOrder returns the snapshot files ordered for matcher
// construction: parents before children, deterministic within a scope.
func (s *Snapshot) FilesInScopeOrder() []File {
	out := append([]File(nil), s.Files...)
	sort.SliceStable(out, func(i, j int) bool {
		return depth(out[i].ScopeDir) < depth(out[j].ScopeDir)
	})
	return out
}

func depth(scope string) int {
	if scope == "" {
		return 0
	}
	return strings.Count(scope, "/") + 1
}

// ErrSnapshotUnstable reports that the source changed while collecting a
// stable snapshot.
var ErrSnapshotUnstable = errors.New("ignore files changed during snapshot; retry update")

// ErrSourceUnreadable reports an I/O failure while traversing the source tree
// that prevents knowing the reachable policy set.
type ErrSourceUnreadable struct {
	Path string
	Err  error
}

func (e *ErrSourceUnreadable) Error() string {
	return fmt.Sprintf("cannot read ignore policy source %q: %v", e.Path, e.Err)
}
func (e *ErrSourceUnreadable) Unwrap() error { return e.Err }

// CollectSnapshot performs two deterministic Git-aware traversals and requires
// the hashes to agree. It returns an error rather than a partial policy on any
// I/O failure that prevents knowing the reachable policy set (§5.2, §5.3).
func CollectSnapshot(sourceRoot string) (*Snapshot, error) {
	a, err := collectOnce(sourceRoot)
	if err != nil {
		return nil, err
	}
	b, err := collectOnce(sourceRoot)
	if err != nil {
		return nil, err
	}
	if a.Hash() != b.Hash() {
		return nil, ErrSnapshotUnstable
	}
	return a, nil
}

// CollectOnce runs a single deterministic Git-aware traversal. Missing
// .gitignore files are simply absent from the result; a read/traversal error
// fails collection.
func collectOnce(sourceRoot string) (*Snapshot, error) {
	src, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, err
	}
	src = filepath.Clean(src)

	snap := &Snapshot{}
	// Current matcher governs which directories we descend into (§3.5).
	m := gitignore.New("")

	// Load the root .gitignore first, if present.
	if err := loadAt(src, "", m, snap); err != nil {
		return nil, err
	}

	var walk func(dir, scope string) error
	walk = func(dir, scope string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return &ErrSourceUnreadable{Path: dir, Err: err}
		}
		for _, ent := range entries {
			name := ent.Name()
			// Never traverse the .git directory's policy influence even when a
			// real repository exists; knowledge-sync does not load
			// .git/info/exclude (§0.1).
			if name == ".git" && ent.IsDir() {
				continue
			}
			rel := name
			if scope != "" {
				rel = scope + "/" + name
			}
			// Use the current matcher to decide whether a directory is ignored
			// before descending (§3.5). Ignored files simply do not reach the
			// snapshot (they are not .gitignore files).
			if m.MatchPath(rel, ent.IsDir()) {
				continue
			}
			if ent.IsDir() {
				// Load this directory's .gitignore before processing children.
				child := filepath.Join(dir, name)
				if err := loadAt(child, rel, m, snap); err != nil {
					return err
				}
				if err := walk(child, rel); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(src, ""); err != nil {
		return nil, err
	}

	// Deterministic final ordering: parents first, then by relative path.
	sort.SliceStable(snap.Files, func(i, j int) bool {
		if depth(snap.Files[i].ScopeDir) != depth(snap.Files[j].ScopeDir) {
			return depth(snap.Files[i].ScopeDir) < depth(snap.Files[j].ScopeDir)
		}
		return snap.Files[i].RelativePath < snap.Files[j].RelativePath
	})
	snap.Warnings = MatcherWarnings(snap.Matcher())
	return snap, nil
}

// loadAt loads dir/.gitignore (when present) into the matcher with scope, and
// records it in the snapshot. A missing file is normal; a read error is fatal
// for stable collection.
func loadAt(dir, scope string, m *gitignore.Matcher, snap *Snapshot) error {
	path := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return &ErrSourceUnreadable{Path: path, Err: err}
	}
	rel := ".gitignore"
	if scope != "" {
		rel = scope + "/.gitignore"
	}
	// The matcher receives the exact bytes; the snapshot retains them too.
	m.AddPatterns(data, scope)
	snap.Files = append(snap.Files, File{RelativePath: rel, ScopeDir: scope, Content: data})
	return nil
}

// Matcher builds a Git-compatible matcher from a snapshot using
// gitignore.New("") so no global/info/executable sources participate (§3.1).
func (s *Snapshot) Matcher() *gitignore.Matcher {
	m := gitignore.New("")
	for _, f := range s.FilesInScopeOrder() {
		m.AddPatterns(f.Content, f.ScopeDir)
	}
	return m
}

// MatcherWarnings converts the matcher's compilation errors into warnings with
// source provenance.
func MatcherWarnings(m *gitignore.Matcher) []PatternWarning {
	errs := m.Errors()
	if len(errs) == 0 {
		return nil
	}
	out := make([]PatternWarning, 0, len(errs))
	for _, e := range errs {
		out = append(out, PatternWarning{
			RelativePath: e.Source,
			Line:         e.Line,
			Pattern:      e.Pattern,
			Message:      e.Message,
		})
	}
	return out
}

// Excluded reports whether a slash-separated relative path is ignored by the
// snapshot's committed policy. The isDir bit is required for Git directory-only
// patterns (§3.2).
func (s *Snapshot) Excluded(relPath string, isDir bool) bool {
	return s.Matcher().MatchPath(relPath, isDir)
}
