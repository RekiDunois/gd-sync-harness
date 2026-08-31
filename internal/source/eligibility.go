// Package source owns the strict local-corpus eligibility contract shared by
// ordinary synchronization and the knowledge compiler.
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"knowledge-sync/internal/namespace"
	"knowledge-sync/internal/policy"
)

const (
	SnapshotEncodingVersion = "source-snapshot-v1"
	RegularFileRuleVersion  = "regular-file-v1"
	SymlinkRuleVersion      = "no-follow-symlink-v1"
	ReservationVersion      = "system-reservation-v1"
)

// EligibilityContractHash fingerprints every eligibility rule that can change
// the ordinary/compiler corpus. The encoding is explicit and map-free.
func EligibilityContractHash(policyHash string, maxFileSize int64) string {
	value := fmt.Sprintf("eligibility-contract-v1\x00%s\x00%d\x00%s\x00%s\x00%s\x00",
		policyHash, maxFileSize, RegularFileRuleVersion, SymlinkRuleVersion, ReservationVersion)
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Entry is metadata for one eligible regular file.
type Entry struct {
	RelPath   string
	Size      int64
	ModTimeNS int64
}

// ScanResult is the strict corpus traversal result. Skipped paths are
// diagnostic-only and never enter the corpus.
type ScanResult struct {
	Entries         []Entry
	SkippedSymlinks []string
	SkippedOversize []string
}

// Options configures a committed-policy corpus scan.
type Options struct {
	SourceRoot  string
	MaxFileSize int64
	Policy      *policy.Snapshot
}

// ValidateCanonicalRelativeTarget rejects malformed persisted or remote target
// paths before any normalization can hide the problem.
func ValidateCanonicalRelativeTarget(value string) error {
	if value == "" {
		return fmt.Errorf("relative target must not be empty")
	}
	if strings.ContainsAny(value, "\\\x00") {
		return fmt.Errorf("relative target %q contains backslash or NUL", value)
	}
	if strings.HasPrefix(value, "/") {
		return fmt.Errorf("relative target %q must not be absolute", value)
	}
	segments := strings.Split(value, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("relative target %q contains invalid segment", value)
		}
	}
	if path.Clean(value) != value {
		return fmt.Errorf("relative target %q is not canonical", value)
	}
	return nil
}

// ValidateSourceRoot verifies the source root itself is a real directory and
// does not overlap the app state directory.
func ValidateSourceRoot(sourceRoot, appStateRoot string) (string, error) {
	abs, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	fi, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("source root %q must not be a symlink", abs)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("source %q is not a directory", abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolved = filepath.Clean(resolved)
	if appStateRoot != "" {
		stateAbs, err := filepath.Abs(appStateRoot)
		if err != nil {
			return "", err
		}
		stateResolved, err := filepath.EvalSymlinks(stateAbs)
		if err != nil {
			return "", err
		}
		if overlaps(resolved, filepath.Clean(stateResolved)) {
			return "", fmt.Errorf("source root %q overlaps app state root %q", resolved, filepath.Clean(stateResolved))
		}
	}
	return resolved, nil
}

// ValidateSourceOverlap rejects a source root that overlaps any other
// non-forgotten profile source root.
func ValidateSourceOverlap(sourceRoot string, otherRoots []string) error {
	for _, other := range otherRoots {
		if other == "" {
			continue
		}
		otherResolved, err := filepath.EvalSymlinks(other)
		if err != nil {
			return err
		}
		if overlaps(sourceRoot, filepath.Clean(otherResolved)) {
			return fmt.Errorf("source root %q overlaps profile source %q", sourceRoot, filepath.Clean(otherResolved))
		}
	}
	return nil
}

func overlaps(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}

// Scan performs a full strict traversal. Excluded and reserved directories are
// skipped before descending; eligible metadata failures are returned.
func Scan(o Options) (*ScanResult, error) {
	root, err := ValidateSourceRoot(o.SourceRoot, "")
	if err != nil {
		return nil, err
	}
	if o.Policy == nil {
		return nil, fmt.Errorf("committed policy snapshot is required")
	}
	matcher := o.Policy.Matcher()
	result := &ScanResult{}
	var walk func(string, string) error
	walk = func(dir, prefix string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("read eligible source directory %q: %w", dir, err)
		}
		for _, ent := range entries {
			name := ent.Name()
			rel := name
			if prefix != "" {
				rel = prefix + "/" + name
			}
			if namespace.IsDerivedPath(rel) {
				continue
			}
			isDir := ent.IsDir()
			var info os.FileInfo
			if !isDir && ent.Type()&os.ModeType == 0 {
				info, err = os.Lstat(filepath.Join(dir, name))
				if err != nil {
					return fmt.Errorf("stat source entry %q: %w", rel, err)
				}
				isDir = info.IsDir()
			}
			if isDir {
				if ent.Type()&os.ModeSymlink != 0 || (info != nil && info.Mode()&os.ModeSymlink != 0) {
					result.SkippedSymlinks = append(result.SkippedSymlinks, rel)
					continue
				}
				if matcher.MatchPath(rel, true) {
					continue
				}
				if err := walk(filepath.Join(dir, name), rel); err != nil {
					return err
				}
				continue
			}
			if matcher.MatchPath(rel, false) {
				continue
			}
			full := filepath.Join(dir, name)
			fi := info
			if fi == nil {
				fi, err = os.Lstat(full)
			}
			if err != nil {
				return fmt.Errorf("stat eligible source %q: %w", rel, err)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				result.SkippedSymlinks = append(result.SkippedSymlinks, rel)
				continue
			}
			if !fi.Mode().IsRegular() {
				continue
			}
			if o.MaxFileSize > 0 && fi.Size() > o.MaxFileSize {
				result.SkippedOversize = append(result.SkippedOversize, rel)
				continue
			}
			result.Entries = append(result.Entries, Entry{RelPath: rel, Size: fi.Size(), ModTimeNS: fi.ModTime().UnixNano()})
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].RelPath < result.Entries[j].RelPath })
	sort.Strings(result.SkippedSymlinks)
	sort.Strings(result.SkippedOversize)
	return result, nil
}
