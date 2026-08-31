package compiler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-sync/internal/policy"
)

func makeCompilerFile(t *testing.T, root, rel, body string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompileBuildsDeterministicGraphFacts(t *testing.T) {
	root := t.TempDir()
	makeCompilerFile(t, root, "a.md", "[[b]] [[b]] ![[asset.png]] [[missing]] [[dup]] #Topic")
	makeCompilerFile(t, root, "b.md", "[[a]]")
	makeCompilerFile(t, root, "self.md", "[[self]]")
	makeCompilerFile(t, root, "one/dup.md", "one")
	makeCompilerFile(t, root, "two/dup.md", "two")
	makeCompilerFile(t, root, "asset.png", "binary")
	makeCompilerFile(t, root, "secret.md", "hidden")
	snap := &policy.Snapshot{Files: []policy.File{{RelativePath: ".gitignore", Content: []byte("secret.md\n")}}}
	result, err := Compile(Input{ProfileID: "p", ProfileUUID: "u", SourceRoot: root, Policy: snap, CompilerRunID: "run", GenerationID: "generation", CompiledAt: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.Counts.EligibleFiles != 6 || result.Manifest.Counts.Notes != 5 || result.Manifest.Counts.Attachments != 1 {
		t.Fatalf("counts = %+v", result.Manifest.Counts)
	}
	if result.Manifest.SourceSnapshotID == "" || result.Manifest.EligibilityContractHash == "" {
		t.Fatal("snapshot and eligibility hashes must be recorded")
	}
	if _, ok := result.Manifest.Artifacts["MANIFEST.json"]; ok {
		t.Fatal("generation manifest must not inventory itself")
	}
	if string(result.Artifacts["FILE_INDEX.jsonl"]) == "" || string(result.Artifacts["LINKS.jsonl"]) == "" {
		t.Fatal("required JSONL artifacts are empty")
	}
	if bytes := result.Artifacts["FILE_INDEX.jsonl"]; containsBytes(bytes, []byte("secret.md")) {
		t.Fatal("excluded path leaked into FILE_INDEX")
	}
	if result.Manifest.Counts.BrokenLinks != 2 {
		t.Fatalf("broken links = %d, want unresolved + ambiguous", result.Manifest.Counts.BrokenLinks)
	}
}

func TestGenerationPublishAndVerify(t *testing.T) {
	root := filepath.Join(t.TempDir(), "compiler", "profile")
	result, err := Compile(Input{ProfileID: "p", ProfileUUID: "u", SourceRoot: t.TempDir(), Policy: &policy.Snapshot{}, CompilerRunID: "run", GenerationID: "generation", CompiledAt: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(root)
	pointer, err := store.Publish(result)
	if err != nil {
		t.Fatal(err)
	}
	if pointer.CurrentGenerationID != "generation" {
		t.Fatalf("pointer = %+v", pointer)
	}
	if _, err := store.VerifyCurrent(); err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(root, "generations", "generation", "MANIFEST.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := manifest.Artifacts["MANIFEST.json"]; ok {
		t.Fatal("published manifest inventories itself")
	}
}

func TestSummaryArtifactsGroupRecords(t *testing.T) {
	model := &graphModel{
		Orphans: []OrphanRecord{
			{Path: "notes/a.md", HardOrphan: true, NoBacklink: true},
			{Path: "notes/b.md", NoBacklink: true, OutboundOnly: true},
		},
		Broken: []BrokenLinkRecord{
			{SourcePath: "notes/a.md", RawTarget: "missing", ResolutionStatus: "unresolved"},
			{SourcePath: "notes/b.md", RawTarget: "missing", ResolutionStatus: "unresolved"},
		},
	}

	orphans := renderOrphans(Input{GenerationID: "generation"}, model)
	if strings.Contains(orphans, "notes/a.md") || !strings.Contains(orphans, `"notes": 1`) {
		t.Fatalf("orphans summary is not compact and grouped: %s", orphans)
	}
	broken := renderBroken(Input{GenerationID: "generation"}, model)
	if strings.Count(broken, `target="missing"`) != 1 || !strings.Contains(broken, "count=2") {
		t.Fatalf("broken links are not grouped: %s", broken)
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
