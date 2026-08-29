package sync

import (
	"os"
	"path/filepath"
	"testing"

	"knowledge-sync/internal/state"
)

func TestScanLocalWithFilters(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "a.md", "hello")
	mkfile(t, root, "notes/b.md", "world")
	mkfile(t, root, "Private/secret.md", "s")
	mkfile(t, root, "video.mp4", "v")
	mkfile(t, root, ".hidden.md", "h")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkfile(t, root, ".git/config", "x")

	p := &state.Profile{
		ID: "test", SourcePath: root, Type: "obsidian",
		MaxFileSize: 0,
		Excludes: []string{
			"exclude_dir_name:.git",
			"exclude_dir_name:Private",
			"exclude_extension:mp4",
		},
	}
	scan, err := ScanLocal(p)
	if err != nil {
		t.Fatal(err)
	}
	var rels []string
	for _, e := range scan.Entries {
		rels = append(rels, e.RelPath)
	}
	want := []string{".hidden.md", "a.md", "notes/b.md"}
	if len(rels) != len(want) {
		t.Fatalf("got %v want %v", rels, want)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Fatalf("got %v want %v", rels, want)
		}
	}
}

func TestDiffLocalManifest(t *testing.T) {
	scan := &ScanResult{Entries: []ScanEntry{
		{RelPath: "a.md", Size: 10, ModTime: 100},
		{RelPath: "b.md", Size: 20, ModTime: 100},
	}}
	manifest := []state.ManifestEntry{
		{ProfileID: "p", RelPath: "a.md", Size: 10, ModTime: 100},
		{ProfileID: "p", RelPath: "b.md", Size: 20, ModTime: 99},
		{ProfileID: "p", RelPath: "gone.md", Size: 5, ModTime: 1},
	}
	changed, deletes, _ := DiffLocalManifest(scan, manifest)
	if len(changed) != 1 || changed[0] != "b.md" {
		t.Fatalf("changed = %v", changed)
	}
	if len(deletes) != 1 || deletes[0] != "gone.md" {
		t.Fatalf("deletes = %v", deletes)
	}
}

func TestChangedFingerprintStable(t *testing.T) {
	s1 := &ScanResult{Entries: []ScanEntry{
		{RelPath: "a", ModTime: 1},
		{RelPath: "b", ModTime: 2},
	}}
	s2 := &ScanResult{Entries: []ScanEntry{
		{RelPath: "b", ModTime: 2},
		{RelPath: "a", ModTime: 1},
	}}
	if s1.ChangedFingerprint() != s2.ChangedFingerprint() {
		t.Error("fingerprint should be order-independent")
	}
	s3 := &ScanResult{Entries: []ScanEntry{
		{RelPath: "a", ModTime: 1},
		{RelPath: "b", ModTime: 2},
		{RelPath: "c", ModTime: 3},
	}}
	if s1.ChangedFingerprint() == s3.ChangedFingerprint() {
		t.Error("fingerprint should change with different content")
	}
}

func TestParseCount(t *testing.T) {
	if parseCount("Transferred:   5 / 5, 1.2 KiB, 0% ETA") != 5 {
		t.Error("parse transferred failed")
	}
	if parseCount("Deleted:       2") != 2 {
		t.Error("parse deleted failed")
	}
	if parseCount("Nothing") != 0 {
		t.Error("no digits should be 0")
	}
}

func mkfile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
