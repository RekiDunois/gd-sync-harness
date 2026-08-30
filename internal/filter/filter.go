package filter

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/git-pkgs/gitignore"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// Engine evaluates path eligibility (§3). In the target model the engine wraps
// the committed Gitignore policy snapshot's matcher; legacy structured rules
// remain only as migration inputs until Phase 2 converts them into a synthetic
// Gitignore snapshot.
//
// Non-path eligibility (max_file_size, symlink handling) remains a separate
// check on the Engine and is never encoded as fake .gitignore rules (§3.3).
type Engine struct {
	ProfileID   string
	MaxFileSize int64 // 0 = unlimited
	// matcher is the committed-policy Gitignore matcher, or nil when the
	// engine is built from legacy structured excludes.
	matcher  *gitignore.Matcher
	warnings []policy.PatternWarning
	// legacy rules are the structured migration-input rules.
	legacy []rule
}

type rule struct {
	kind  string
	value string
}

// New builds an Engine from a profile and its excludes. The excludes are the
// legacy structured migration input; a profile without structured excludes gets
// a nil matcher (no path exclusions). The policy snapshot is attached via
// FromPolicy.
func New(profileID string, maxFileSize int64, excludes []string) *Engine {
	e := &Engine{ProfileID: profileID, MaxFileSize: maxFileSize}
	for _, ex := range excludes {
		e.addRaw(ex)
	}
	return e
}

// FromProfile builds an Engine from a state.Profile (legacy structured rules).
func FromProfile(p *state.Profile) *Engine {
	return New(p.ID, p.MaxFileSize, p.Excludes)
}

// FromPolicy builds an Engine from a committed policy snapshot. The snapshot's
// matcher is constructed from repository-local .gitignore bytes only (§3.1).
func FromPolicy(profileID string, maxFileSize int64, snap *policy.Snapshot) *Engine {
	e := &Engine{ProfileID: profileID, MaxFileSize: maxFileSize}
	if snap != nil {
		m := snap.Matcher()
		e.matcher = m
		e.warnings = policy.MatcherWarnings(m)
	}
	return e
}

// Warnings returns the matcher's invalid-pattern warnings (§3.4).
func (e *Engine) Warnings() []policy.PatternWarning { return e.warnings }

func (e *Engine) addRaw(raw string) {
	idx := strings.Index(raw, ":")
	if idx <= 0 {
		e.legacy = append(e.legacy, rule{kind: state.RuleExcludePathPrefix, value: raw})
		return
	}
	kind, val := raw[:idx], raw[idx+1:]
	switch kind {
	case state.RuleExcludePathPrefix, state.RuleExcludeDirName, state.RuleExcludeFileName, state.RuleExcludeExtension:
		e.legacy = append(e.legacy, rule{kind: kind, value: strings.TrimSpace(val)})
	}
}

// NormalizeRelPath converts an absolute path under sourceRoot to a slash-separated
// relative path with forward slashes, as used by rclone remote paths.
func NormalizeRelPath(sourceRoot, absPath string) string {
	rel, err := filepath.Rel(sourceRoot, absPath)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return rel
}

// ExcludedDir reports whether a relative path (slash-separated) should be
// excluded given a real file-vs-directory identity (§3.2). The Gitignore
// matcher is authoritative when present; otherwise legacy structured rules
// apply.
func (e *Engine) ExcludedDir(relPath string, isDir bool) (bool, string) {
	if relPath == "" || relPath == "." {
		return true, "root"
	}
	norm := filepath.ToSlash(relPath)
	if e.matcher != nil {
		return e.matcher.MatchPath(norm, isDir), "gitignore"
	}
	return e.legacyExcluded(norm, isDir)
}

// Excluded reports whether a relative path should be excluded. It is kept for
// call sites that do not know the file-vs-directory identity and for legacy
// compatibility; it treats candidate paths as non-directory unless the path
// itself ends with a slash. New call sites with directory knowledge should use
// ExcludedDir.
func (e *Engine) Excluded(relPath string) (bool, string) {
	return e.ExcludedDir(relPath, strings.HasSuffix(filepath.ToSlash(relPath), "/"))
}

func (e *Engine) legacyExcluded(norm string, isDir bool) (bool, string) {
	parts := strings.Split(norm, "/")
	fileName := parts[len(parts)-1]
	ext := strings.ToLower(filepath.Ext(fileName))

	for _, r := range e.legacy {
		switch r.kind {
		case state.RuleExcludePathPrefix:
			p := strings.TrimPrefix(filepath.ToSlash(r.value), "/")
			if norm == p || strings.HasPrefix(norm, p+"/") {
				return true, r.kind + ":" + p
			}
		case state.RuleExcludeDirName:
			if strings.ContainsRune(r.value, '/') {
				p := strings.TrimPrefix(filepath.ToSlash(r.value), "/")
				if strings.HasPrefix(norm, p+"/") {
					return true, r.kind + ":" + p
				}
			} else {
				for i := 0; i < len(parts)-1; i++ {
					if parts[i] == r.value {
						return true, r.kind + ":" + r.value
					}
				}
			}
		case state.RuleExcludeFileName:
			if fileName == r.value {
				return true, r.kind + ":" + r.value
			}
		case state.RuleExcludeExtension:
			v := strings.TrimPrefix(r.value, ".")
			if strings.TrimPrefix(ext, ".") == strings.ToLower(v) {
				return true, r.kind + ":" + r.value
			}
		}
	}
	return false, ""
}

// IsSymlink reports whether the given absolute path is a symlink (§10.4).
func IsSymlink(absPath string) bool {
	fi, err := os.Lstat(absPath)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}

// OverSize reports whether size exceeds the profile's max_file_size.
func (e *Engine) OverSize(size int64) bool {
	if e.MaxFileSize <= 0 {
		return false
	}
	return size > e.MaxFileSize
}
