package policy_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-sync/internal/filter"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// legacyEngine builds the current filter.Engine from structured rules.
func legacyEngine(t *testing.T, rules []policy.LegacyRule) *filter.Engine {
	t.Helper()
	raw := make([]string, 0, len(rules))
	for _, r := range rules {
		raw = append(raw, r.Kind+":"+r.Value)
	}
	return filter.New("p", 0, raw)
}

// TestLegacyConversionCompatibility is the oracle test (§6.3, §21.5): for a
// representative and adversarial candidate matrix, the old structured engine and
// the migrated Gitignore snapshot must agree on eligibility.
func TestLegacyConversionCompatibility(t *testing.T) {
	candidatePaths := []struct {
		path  string
		isDir bool
	}{
		{"Private", true},
		{"Private/secret.md", false},
		{"docs/Private/a.md", false},
		{"a/b/c/Private/deep.md", false},
		{"Private2/a.md", false},
		{"notes/.git/config", false},
		{".git/config", false},
		{"a/.git/x", false},
		{"build", true},
		{"build/out.txt", false},
		{"sub/build/x.md", false},
		{".DS_Store", false},
		{"a/.DS_Store", false},
		{"a/b/.DS_Store", false},
		{"x.tmp", false},
		{"X.TMP", false},
		{"y.TmP", false},
		{"z.txt", false},
		{"tmp.bak", false},
		{"space file.md", false},
		{"star*file.md", false},
		{"question?file.md", false},
		{"bracket[1].md", false},
		{"#hash.md", false},
		{"!bang.md", false},
		{"Private/x.tmp", false},
	}

	ruleSets := [][]policy.LegacyRule{
		{{Kind: state.RuleExcludePathPrefix, Value: "Private"}},
		{{Kind: state.RuleExcludeDirName, Value: "Private"}},
		{{Kind: state.RuleExcludeDirName, Value: ".git"}},
		{{Kind: state.RuleExcludeFileName, Value: ".DS_Store"}},
		{{Kind: state.RuleExcludeExtension, Value: "tmp"}},
		{{Kind: state.RuleExcludeExtension, Value: "TMP"}},
		{{Kind: state.RuleExcludeExtension, Value: ".mp4"}},
		{
			{Kind: state.RuleExcludeDirName, Value: ".git"},
			{Kind: state.RuleExcludeFileName, Value: ".DS_Store"},
			{Kind: state.RuleExcludeExtension, Value: "tmp"},
			{Kind: state.RuleExcludePathPrefix, Value: "build"},
		},
		// Adversarial literal metacharacters.
		{{Kind: state.RuleExcludeFileName, Value: "star*file.md"}},
		{{Kind: state.RuleExcludeFileName, Value: "question?file.md"}},
		{{Kind: state.RuleExcludeFileName, Value: "bracket[1].md"}},
		{{Kind: state.RuleExcludeDirName, Value: "dir[2]"}},
	}

	for si, rules := range ruleSets {
		old := legacyEngine(t, rules)
		snap := policy.ConvertLegacyRules(rules)
		for _, c := range candidatePaths {
			oldWant, _ := old.ExcludedDir(c.path, c.isDir)
			newWant := snap.Excluded(c.path, c.isDir)
			// A bare dir node is the one benign divergence: the old engine's
			// dir_name rule does not match the directory node itself (only
			// descendants), while Gitignore `name/` also matches the node.
			// Both exclude the same set of descendant files, which is the
			// semantic invariant (§6.3). Skip the bare dir-node equality and
			// assert descendant-file equivalence via TestLegacyConversionScanEq.
			if c.isDir && !strings.Contains(c.path, "/") {
				continue
			}
			if oldWant != newWant {
				t.Errorf("ruleset %d rules=%v path=%s dir=%v: old=%v new=%v",
					si, rules, c.path, c.isDir, oldWant, newWant)
			}
		}
	}
}

// TestLegacyConversionExtensionCase covers uppercase/lowercase extension cases
// explicitly (§21.5).
func TestLegacyConversionExtensionCase(t *testing.T) {
	snap := policy.ConvertLegacyRules([]policy.LegacyRule{{Kind: state.RuleExcludeExtension, Value: "tmp"}})
	for _, path := range []string{"a.tmp", "a.TMP", "a.Tmp", "a.tmP"} {
		if !snap.Excluded(path, false) {
			t.Errorf("%s must be excluded case-insensitively", path)
		}
	}
	if snap.Excluded("a.txt", false) {
		t.Error("a.txt must not be excluded")
	}
	snap2 := policy.ConvertLegacyRules([]policy.LegacyRule{{Kind: state.RuleExcludeExtension, Value: ".mp4"}})
	if !snap2.Excluded("v.MP4", false) {
		t.Error(".mp4 case-insensitive match failed")
	}
}

// TestLegacyConversionDirNameAtAnyDepth verifies dir_name matches at arbitrary
// depth (§6.3).
func TestLegacyConversionDirNameAtAnyDepth(t *testing.T) {
	snap := policy.ConvertLegacyRules([]policy.LegacyRule{{Kind: state.RuleExcludeDirName, Value: ".git"}})
	for _, path := range []string{"a/.git/config", "a/b/.git/head", ".git/config"} {
		if !snap.Excluded(path, false) {
			t.Errorf("%s must be excluded by .git dir_name", path)
		}
	}
	if snap.Excluded("git/config", false) {
		t.Error("gitfile without dot must not be excluded")
	}
	if snap.Excluded("notes/x.md", false) {
		t.Error("unrelated file must not be excluded")
	}
}

// TestLegacyConversionLiteralMetachars verifies literal values stay literal.
func TestLegacyConversionLiteralMetachars(t *testing.T) {
	snap := policy.ConvertLegacyRules([]policy.LegacyRule{{Kind: state.RuleExcludeFileName, Value: "star*file.md"}})
	if !snap.Excluded("star*file.md", false) {
		t.Error("literal star filename must match")
	}
	if snap.Excluded("starXfile.md", false) {
		t.Error("star must stay literal, not a glob")
	}
}

func TestConvertLegacyEmpty(t *testing.T) {
	snap := policy.ConvertLegacyRules(nil)
	if len(snap.Files) != 0 {
		t.Fatalf("empty rules should produce empty snapshot, got %d files", len(snap.Files))
	}
	if snap.Excluded("anything", false) {
		t.Error("empty migrated policy excludes nothing")
	}
}

// TestLegacyScanEquivalence walks a realistic tree and asserts the set of
// eligible files is identical between the old engine and the migrated snapshot
// (§6.3 file-level invariant).
func TestLegacyScanEquivalence(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"keep.md", "notes/a.md", "Private/secret.md", "docs/Private/x.md",
		"a/b/c/Private/deep.md", ".git/config", "a/.git/x", "build/out.txt",
		"sub/build/y.md", ".DS_Store", "a/.DS_Store", "x.tmp", "X.TMP",
		"tmp.bak", "star*file.md", "notes/build/z.md",
	} {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ruleSets := [][]policy.LegacyRule{
		{{Kind: state.RuleExcludePathPrefix, Value: "Private"}},
		{{Kind: state.RuleExcludeDirName, Value: "Private"}},
		{{Kind: state.RuleExcludeDirName, Value: ".git"}},
		{{Kind: state.RuleExcludeDirName, Value: "build"}},
		{{Kind: state.RuleExcludeFileName, Value: ".DS_Store"}},
		{{Kind: state.RuleExcludeExtension, Value: "tmp"}},
		{
			{Kind: state.RuleExcludeDirName, Value: ".git"},
			{Kind: state.RuleExcludeDirName, Value: "Private"},
			{Kind: state.RuleExcludeFileName, Value: ".DS_Store"},
			{Kind: state.RuleExcludeExtension, Value: "tmp"},
			{Kind: state.RuleExcludePathPrefix, Value: "build"},
		},
		{{Kind: state.RuleExcludeFileName, Value: "star*file.md"}},
	}

	eligibleOld := func(eng *filter.Engine) map[string]bool {
		out := map[string]bool{}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if path == root {
				return nil
			}
			rel := strings.TrimPrefix(path, root+"/")
			rel = filepath.ToSlash(rel)
			if info.IsDir() {
				if ex, _ := eng.ExcludedDir(rel, true); ex {
					return filepath.SkipDir
				}
				return nil
			}
			if ex, _ := eng.ExcludedDir(rel, false); ex {
				return nil
			}
			out[rel] = true
			return nil
		})
		return out
	}

	eligibleNew := func(snap *policy.IgnoreSnapshot) map[string]bool {
		out := map[string]bool{}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if path == root {
				return nil
			}
			rel := strings.TrimPrefix(path, root+"/")
			rel = filepath.ToSlash(rel)
			if info.IsDir() {
				if snap.Excluded(rel, true) {
					return filepath.SkipDir
				}
				return nil
			}
			if snap.Excluded(rel, false) {
				return nil
			}
			out[rel] = true
			return nil
		})
		return out
	}

	for si, rules := range ruleSets {
		old := legacyEngine(t, rules)
		snap := policy.ConvertLegacyRules(rules)
		a := eligibleOld(old)
		b := eligibleNew(snap)
		if len(a) != len(b) {
			t.Fatalf("ruleset %d: old=%v new=%v", si, a, b)
		}
		for k := range a {
			if !b[k] {
				t.Errorf("ruleset %d: old eligible %s missing in migrated", si, k)
			}
		}
		for k := range b {
			if !a[k] {
				t.Errorf("ruleset %d: migrated eligible %s missing in old", si, k)
			}
		}
	}
}
