package sync

import (
	"sort"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/source"
)

// PolicyScanResult is the outcome of classifying local files against a
// committed policy snapshot. It is the ledger input for a policy refresh.
type PolicyScanResult struct {
	// ActivePaths are eligible files under the committed policy.
	ActivePaths []string
}

// ActiveEntry is one eligible local file discovered by a metadata-aware active
// scan under the committed policy. It carries the high-resolution size and
// mtime so a same-path change can be detected by the source-stability
// fingerprint without a content hash (§9 of the ignored-churn fix plan).
type ActiveEntry = source.Entry

// ScanActiveEntries walks the profile source with the committed policy matcher
// (directory-aware), applying the non-path eligibility rules (symlink, size),
// and returns the active entries with metadata (§9.1). The profile's legacy
// Excludes field is not used: committed policy is the sole path authority.
func ScanActiveEntries(sourcePath string, maxFileSize int64, snap *policy.Snapshot) ([]ActiveEntry, error) {
	result, err := source.Scan(source.Options{
		SourceRoot: sourcePath, MaxFileSize: maxFileSize, Policy: snap,
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].RelPath < result.Entries[j].RelPath })
	return result.Entries, nil
}

// ScanActivePaths is the path-only projection of ScanActiveEntries. It shares
// the same eligibility traversal so rclone --files-from, manifest active sets,
// and preflight fingerprints never drift from each other (§9.2).
func ScanActivePaths(sourcePath string, maxFileSize int64, snap *policy.Snapshot) ([]string, error) {
	entries, err := ScanActiveEntries(sourcePath, maxFileSize, snap)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for i := range entries {
		paths = append(paths, entries[i].RelPath)
	}
	return paths, nil
}
