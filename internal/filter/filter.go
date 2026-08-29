package filter

import (
	"os"
	"path/filepath"
	"strings"

	"knowledge-sync/internal/state"
)

// Engine evaluates the structured filter model (§10.1). The same Engine
// instance is used by the watcher, local manifest scans, fast-path file lists,
// and full rclone reconciliation so semantics never split.
type Engine struct {
	ProfileID   string
	MaxFileSize int64 // 0 = unlimited
	rules       []rule
}

type rule struct {
	kind  string
	value string
}

// New builds an Engine from a profile and its excludes.
func New(profileID string, maxFileSize int64, excludes []string) *Engine {
	e := &Engine{ProfileID: profileID, MaxFileSize: maxFileSize}
	for _, ex := range excludes {
		e.addRaw(ex)
	}
	return e
}

// FromProfile builds an Engine from a state.Profile.
func FromProfile(p *state.Profile) *Engine {
	return New(p.ID, p.MaxFileSize, p.Excludes)
}

func (e *Engine) addRaw(raw string) {
	idx := strings.Index(raw, ":")
	if idx <= 0 {
		e.rules = append(e.rules, rule{kind: state.RuleExcludePathPrefix, value: raw})
		return
	}
	kind, val := raw[:idx], raw[idx+1:]
	switch kind {
	case state.RuleExcludePathPrefix, state.RuleExcludeDirName, state.RuleExcludeFileName, state.RuleExcludeExtension:
		e.rules = append(e.rules, rule{kind: kind, value: strings.TrimSpace(val)})
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

// Excluded reports whether a relative path (slash-separated) should be excluded.
func (e *Engine) Excluded(relPath string) (bool, string) {
	if relPath == "" || relPath == "." {
		return true, "root"
	}
	norm := filepath.ToSlash(relPath)
	parts := strings.Split(norm, "/")
	fileName := parts[len(parts)-1]
	ext := strings.ToLower(filepath.Ext(fileName))

	for _, r := range e.rules {
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
