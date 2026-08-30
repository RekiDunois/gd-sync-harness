package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"knowledge-sync/internal/filter"
	"knowledge-sync/internal/state"
)

// SyncOptions tune sync behavior.
type SyncOptions struct {
	// AllowDeletes overrides MaxDelete for a single reconciliation (§16).
	AllowDeletes int
}

// ScanEntry is a local file discovered by a manifest scan.
type ScanEntry struct {
	RelPath string `json:"rel_path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

// ScanResult is the outcome of a local-only manifest scan (§18.1.2).
type ScanResult struct {
	Entries         []ScanEntry
	SkippedSymlinks []string
	SkippedOversize []string
}

type ScanProgress struct {
	Visited  int
	Eligible int
}

// ScanLocal walks the profile source applying the structured filter, returning
// the eligible file set.
func ScanLocal(p *state.Profile) (*ScanResult, error) {
	return ScanLocalProgress(p, nil)
}

// ScanLocalProgress is ScanLocal with a time-throttled progress callback.
func ScanLocalProgress(p *state.Profile, onProgress func(ScanProgress)) (*ScanResult, error) {
	eng := filter.FromProfile(p)
	res := &ScanResult{}
	visited := 0
	lastReport := time.Time{}
	err := filepath.Walk(p.SourcePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == p.SourcePath {
			return nil
		}
		visited++
		rel := filter.NormalizeRelPath(p.SourcePath, path)
		if rel == "" {
			return nil
		}
		if info.IsDir() {
			if excluded, _ := eng.ExcludedDir(rel, true); excluded {
				return filepath.SkipDir
			}
			return nil
		}
		if excluded, _ := eng.ExcludedDir(rel, false); excluded {
			return nil
		}
		if filter.IsSymlink(path) {
			res.SkippedSymlinks = append(res.SkippedSymlinks, rel)
			return nil
		}
		if eng.OverSize(info.Size()) {
			res.SkippedOversize = append(res.SkippedOversize, rel)
			return nil
		}
		res.Entries = append(res.Entries, ScanEntry{
			RelPath: rel,
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		})
		if onProgress != nil && (lastReport.IsZero() || time.Since(lastReport) >= time.Second) {
			onProgress(ScanProgress{Visited: visited, Eligible: len(res.Entries)})
			lastReport = time.Now()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(res.Entries, func(i, j int) bool { return res.Entries[i].RelPath < res.Entries[j].RelPath })
	if onProgress != nil {
		onProgress(ScanProgress{Visited: visited, Eligible: len(res.Entries)})
	}
	return res, nil
}

// DiffLocalManifest compares a scan against the stored manifest and returns the
// changed (create/modify) files plus locally-deleted files.
func DiffLocalManifest(scan *ScanResult, manifest []state.ManifestEntry) (changed []string, deletes []string, missing []string) {
	seen := make(map[string]bool, len(scan.Entries))
	for _, e := range scan.Entries {
		seen[e.RelPath] = true
		m, ok := manifestLookup(manifest, e.RelPath)
		if !ok || m.Size != e.Size || m.ModTime != e.ModTime {
			changed = append(changed, e.RelPath)
		}
	}
	for _, m := range manifest {
		if !seen[m.RelPath] {
			deletes = append(deletes, m.RelPath)
			missing = append(missing, m.RelPath)
		}
	}
	sort.Strings(changed)
	sort.Strings(deletes)
	return changed, deletes, missing
}

func manifestLookup(m []state.ManifestEntry, rel string) (state.ManifestEntry, bool) {
	for _, e := range m {
		if e.RelPath == rel {
			return e, true
		}
	}
	return state.ManifestEntry{}, false
}

// ChangedFingerprint returns a stable, order-independent generation stamp for
// stable-generation preflight (§15.1).
func (s *ScanResult) ChangedFingerprint() string {
	if len(s.Entries) == 0 {
		return "empty"
	}
	sorted := make([]ScanEntry, len(s.Entries))
	copy(sorted, s.Entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RelPath < sorted[j].RelPath })
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d:", len(sorted))
	maxMT := int64(0)
	for _, e := range sorted {
		fmt.Fprintf(&sb, "%s:%d:%d;", e.RelPath, e.Size, e.ModTime)
		if e.ModTime > maxMT {
			maxMT = e.ModTime
		}
	}
	fmt.Fprintf(&sb, "max:%d", maxMT)
	return sb.String()
}

// writeFilesFrom writes a files-from list (one rel path per line).
func writeFilesFrom(files []string) (string, error) {
	f, err := os.CreateTemp("", "ks-filesfrom-*.txt")
	if err != nil {
		return "", err
	}
	for _, x := range files {
		esc := strings.ReplaceAll(x, "\n", "\\n")
		if _, err := fmt.Fprintln(f, esc); err != nil {
			f.Close()
			return "", err
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// PreflightResult summarizes an authoritative dry-run.
type PreflightResult struct {
	SourceFiles       int      `json:"source_files"`
	ToCopy            int      `json:"to_copy"`
	ToDelete          int      `json:"to_delete"`
	DeletePaths       []string `json:"delete_paths"`
	RemoteDup         bool     `json:"remote_dup"`
	SourceStable      bool     `json:"source_stable"`
	SourceFingerprint string   `json:"source_fingerprint"`
	SidecarValid      bool     `json:"sidecar_valid"`
}

// ErrDeleteBudgetExceeded is returned when preflight deletions exceed the budget.
var ErrDeleteBudgetExceeded = errors.New("delete budget exceeded; destructive sync not started")

// ErrSourceUnstable is returned when the source changed during preflight.
var ErrSourceUnstable = errors.New("source changed during preflight; retry after settle")

// MarshalPreflight serializes a PreflightResult.
func (p *PreflightResult) MarshalPreflight() ([]byte, error) { return json.MarshalIndent(p, "", "  ") }
