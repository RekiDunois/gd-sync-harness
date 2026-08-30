package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"knowledge-sync/internal/policy"
)

// tmpScriptsSnapshot is a committed-style policy snapshot excluding the
// .tmp_scripts/ directory.
func tmpScriptsSnapshot() *policy.Snapshot {
	return &policy.Snapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte(".tmp_scripts/\n")},
	}}
}

// T12: the committed .tmp_scripts/ directory rule excludes every file under it;
// only eligible files are returned by the active scan.
func TestScanActiveEntriesExcludesTmpScriptsDir(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "notes/a.md", "a")
	mkfile(t, root, ".tmp_scripts/a.py", "py")
	mkfile(t, root, ".tmp_scripts/result.json", "{}")

	snap := tmpScriptsSnapshot()
	entries, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].RelPath != "notes/a.md" {
		t.Fatalf("active entries = %+v, want only notes/a.md", entries)
	}
	paths, err := ScanActivePaths(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "notes/a.md" {
		t.Fatalf("active paths = %v, want [notes/a.md]", paths)
	}
}

// T13: the metadata fingerprint must change when a same-path file's size
// changes, even though the path set is identical.
func TestFingerprintActiveEntriesSamePathSizeChange(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "a.md", "x")
	snap := &policy.Snapshot{}

	e1, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := fingerprintActiveEntries(e1)

	// Same path, different size (1 byte -> 2 bytes).
	mkfile(t, root, "a.md", "xy")
	e2, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp2 := fingerprintActiveEntries(e2)

	if fp1 == fp2 {
		t.Fatal("same-path size change must change the fingerprint")
	}
	if len(e1) != 1 || e1[0].RelPath != "a.md" || len(e2) != 1 || e2[0].RelPath != "a.md" {
		t.Fatalf("path set must be identical; e1=%+v e2=%+v", e1, e2)
	}
}

// T14: the metadata fingerprint must change when a same-path, same-size file's
// mtime changes.
func TestFingerprintActiveEntriesSameSizeMtimeChange(t *testing.T) {
	root := t.TempDir()
	full := filepath.Join(root, "a.md")
	if err := os.WriteFile(full, []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := &policy.Snapshot{}
	e1, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := fingerprintActiveEntries(e1)

	// Same size, but force a later mtime.
	older := time.Unix(0, e1[0].ModTimeNS-1_000_000)
	if err := os.Chtimes(full, older, older); err != nil {
		t.Fatal(err)
	}
	e2, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp2 := fingerprintActiveEntries(e2)

	if e1[0].Size != e2[0].Size {
		t.Fatalf("test requires same size; got %d vs %d", e1[0].Size, e2[0].Size)
	}
	if fp1 == fp2 {
		t.Fatal("same-path same-size mtime change must change the fingerprint")
	}
}

// T15: ignored churn inside .tmp_scripts/ must not change the active metadata
// fingerprint.
func TestFingerprintActiveEntriesIgnoredChurnStable(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "notes/a.md", "a")
	snap := tmpScriptsSnapshot()

	e1, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := fingerprintActiveEntries(e1)

	// Only ignored churn changes.
	mkfile(t, root, ".tmp_scripts/result.json", "{}")
	mkfile(t, root, ".tmp_scripts/result.json", "{v2}")
	e2, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp2 := fingerprintActiveEntries(e2)

	if fp1 != fp2 {
		t.Fatal("ignored churn must not change the active fingerprint")
	}
}

// T16: adding or deleting an active path changes the fingerprint.
func TestFingerprintActiveEntriesActiveCreateDelete(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "a.md", "a")
	snap := &policy.Snapshot{}

	e1, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := fingerprintActiveEntries(e1)

	// Active create.
	mkfile(t, root, "b.md", "b")
	e2, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp2 := fingerprintActiveEntries(e2)
	if fp1 == fp2 {
		t.Fatal("active create must change the fingerprint")
	}

	// Active delete (back to just a.md).
	if err := os.Remove(filepath.Join(root, "b.md")); err != nil {
		t.Fatal(err)
	}
	e3, err := ScanActiveEntries(root, 0, snap)
	if err != nil {
		t.Fatal(err)
	}
	fp3 := fingerprintActiveEntries(e3)
	if fp2 == fp3 {
		t.Fatal("active delete must change the fingerprint")
	}
	if fp1 != fp3 {
		t.Fatal("returning to the original set must restore the original fingerprint")
	}
}

// TestFingerprintActiveEntriesOrderIndependent pins the order-independence of
// the metadata fingerprint.
func TestFingerprintActiveEntriesOrderIndependent(t *testing.T) {
	a := fingerprintActiveEntries([]ActiveEntry{
		{RelPath: "a", Size: 1, ModTimeNS: 10},
		{RelPath: "b", Size: 2, ModTimeNS: 20},
	})
	b := fingerprintActiveEntries([]ActiveEntry{
		{RelPath: "b", Size: 2, ModTimeNS: 20},
		{RelPath: "a", Size: 1, ModTimeNS: 10},
	})
	if a != b {
		t.Fatal("fingerprint must be order-independent")
	}
	c := fingerprintActiveEntries([]ActiveEntry{
		{RelPath: "a", Size: 1, ModTimeNS: 11},
		{RelPath: "b", Size: 2, ModTimeNS: 20},
	})
	if a == c {
		t.Fatal("mtime change must be reflected")
	}
}
