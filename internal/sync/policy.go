package sync

import (
	"os"
	"path/filepath"
	"sort"

	"knowledge-sync/internal/filter"
	"knowledge-sync/internal/policy"
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
type ActiveEntry struct {
	RelPath   string
	Size      int64
	ModTimeNS int64
}

// ScanActiveEntries walks the profile source with the committed policy matcher
// (directory-aware), applying the non-path eligibility rules (symlink, size),
// and returns the active entries with metadata (§9.1). The profile's legacy
// Excludes field is not used: committed policy is the sole path authority.
func ScanActiveEntries(sourcePath string, maxFileSize int64, snap *policy.Snapshot) ([]ActiveEntry, error) {
	eng := filter.FromPolicy("", maxFileSize, snap)
	var out []ActiveEntry
	err := walkDirActiveEntries(sourcePath, "", eng, &out)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
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

func walkDirActiveEntries(sourcePath, rel string, eng *filter.Engine, out *[]ActiveEntry) error {
	dir := sourcePath
	if rel != "" {
		dir = filepath.Join(sourcePath, rel)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		name := ent.Name()
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		if ent.IsDir() {
			if excluded, _ := eng.ExcludedDir(childRel, true); excluded {
				continue
			}
			if err := walkDirActiveEntries(sourcePath, childRel, eng, out); err != nil {
				return err
			}
			continue
		}
		if excluded, _ := eng.ExcludedDir(childRel, false); excluded {
			continue
		}
		full := filepath.Join(sourcePath, childRel)
		if filter.IsSymlink(full) {
			continue
		}
		fi, err := os.Stat(full)
		if err != nil {
			continue
		}
		if eng.OverSize(fi.Size()) {
			continue
		}
		*out = append(*out, ActiveEntry{
			RelPath:   childRel,
			Size:      fi.Size(),
			ModTimeNS: fi.ModTime().UnixNano(),
		})
	}
	return nil
}
