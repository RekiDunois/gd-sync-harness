package source

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"knowledge-sync/internal/policy"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanUsesStrictEligibilityContract(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "public.md", "pub")
	writeFile(t, root, "private/secret.md", "secret")
	writeFile(t, root, ".knowledge-derived/generated.md", "must not read")
	writeFile(t, root, "large.bin", "12345")
	if err := os.Symlink(filepath.Join(root, "public.md"), filepath.Join(root, "alias.md")); err != nil {
		t.Fatal(err)
	}
	snap := &policy.Snapshot{Files: []policy.File{{RelativePath: ".gitignore", Content: []byte("private/\n")}}}
	got, err := Scan(Options{SourceRoot: root, MaxFileSize: 4, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0].RelPath != "public.md" {
		t.Fatalf("entries = %+v", got.Entries)
	}
	if len(got.SkippedSymlinks) != 1 || got.SkippedSymlinks[0] != "alias.md" {
		t.Fatalf("symlinks = %v", got.SkippedSymlinks)
	}
	if len(got.SkippedOversize) != 1 || got.SkippedOversize[0] != "large.bin" {
		t.Fatalf("oversize = %v", got.SkippedOversize)
	}
}

func TestScanRetainsNanosecondMetadata(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "note.md")
	writeFile(t, root, "note.md", "same")
	first := time.Unix(100, 123)
	if err := os.Chtimes(name, first, first); err != nil {
		t.Fatal(err)
	}
	a, err := Scan(Options{SourceRoot: root, Policy: &policy.Snapshot{}})
	if err != nil {
		t.Fatal(err)
	}
	second := time.Unix(100, 456)
	if err := os.Chtimes(name, second, second); err != nil {
		t.Fatal(err)
	}
	b, err := Scan(Options{SourceRoot: root, Policy: &policy.Snapshot{}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Entries[0].ModTimeNS == b.Entries[0].ModTimeNS {
		t.Fatalf("mtime precision lost: %d == %d", a.Entries[0].ModTimeNS, b.Entries[0].ModTimeNS)
	}
}

func TestValidateCanonicalRelativeTarget(t *testing.T) {
	for _, value := range []string{"", "/absolute", "a\\b", "a//b", "a/./b", "a/../b", "a/", "a\x00b"} {
		if err := ValidateCanonicalRelativeTarget(value); err == nil {
			t.Errorf("ValidateCanonicalRelativeTarget(%q) accepted malformed path", value)
		}
	}
	for _, value := range []string{"a", "folder/note.md", "unicode/笔记.md"} {
		if err := ValidateCanonicalRelativeTarget(value); err != nil {
			t.Errorf("ValidateCanonicalRelativeTarget(%q): %v", value, err)
		}
	}
}

func TestValidateSourceRootRejectsSymlinkAndOverlap(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "source-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateSourceRoot(link, ""); err == nil {
		t.Fatal("symlink source root accepted")
	}
	stateRoot := filepath.Join(root, "state")
	if err := os.MkdirAll(stateRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateSourceRoot(root, stateRoot); err == nil {
		t.Fatal("source/app-state overlap accepted")
	}
}
