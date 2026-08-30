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

// ScanActivePaths walks the profile source with the committed policy matcher
// (directory-aware), applying the non-path eligibility rules (symlink, size)
// and returning the active relative paths (§8.1, §10.2). The profile's legacy
// Excludes field is not used: committed policy is the sole path authority.
func ScanActivePaths(sourcePath string, maxFileSize int64, snap *policy.Snapshot) ([]string, error) {
	eng := filter.FromPolicy("", maxFileSize, snap)
	var out []string
	err := walkDirActive(sourcePath, "", eng, &out)
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func walkDirActive(sourcePath, rel string, eng *filter.Engine, out *[]string) error {
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
			if err := walkDirActive(sourcePath, childRel, eng, out); err != nil {
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
		*out = append(*out, childRel)
	}
	return nil
}
