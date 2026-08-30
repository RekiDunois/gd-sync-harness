package state

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestProfileLifecycleTombstone(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{
		ID: "obs", ProfileUUID: "uuid-1", Type: "obsidian",
		SourcePath: "/vault", RemoteName: "gdrive", RemoteFolderID: "f1",
		RemoteDisplayPath: "ChatGPT Knowledge/Obsidian", Enabled: true,
		MaxDelete: 100, MaxFileSize: 512 << 20,
	}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateProfile(p); err != ErrIDExists {
		t.Fatalf("want ErrIDExists, got %v", err)
	}
	if err := db.TombstoneProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetProfile(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Tombstoned {
		t.Error("should be tombstoned")
	}
	p2 := *p
	p2.ProfileUUID = "uuid-2"
	if err := db.CreateProfile(&p2); err != ErrIDTombstoned {
		t.Fatalf("want ErrIDTombstoned, got %v", err)
	}
	if err := db.RestoreProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetProfile(p.ID)
	if got.Tombstoned {
		t.Error("should not be tombstoned after restore")
	}
	if err := db.ForgetProfile(p.ID); err != ErrNotTombstoned {
		t.Fatalf("want ErrNotTombstoned, got %v", err)
	}
	if err := db.TombstoneProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.ForgetProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetProfile(p.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after forget, got %v", err)
	}
}

func TestPendingEventsUpsert(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{ID: "p1", ProfileUUID: "u1", Type: "generic",
		SourcePath: "/src", RemoteName: "g", RemoteFolderID: "f",
		RemoteDisplayPath: "x", Enabled: true}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPendingEvent("p1", "a.md", EventModify, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPendingEvent("p1", "a.md", EventModify, 2); err != nil {
		t.Fatal(err)
	}
	n, err := db.CountPending("p1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("upsert should dedupe; got %d", n)
	}
	evs, _ := db.ListPending("p1")
	if evs[0].SourceGeneration != 2 {
		t.Errorf("generation should bump to 2, got %d", evs[0].SourceGeneration)
	}
	if err := db.UpsertDeleteEvent("p1", "a.md", 4); err != nil {
		t.Fatal(err)
	}
	destructive, _ := db.HasDestructivePending("p1")
	if !destructive {
		t.Error("delete event should be destructive")
	}
	if err := db.ClearPending("p1"); err != nil {
		t.Fatal(err)
	}
	n, _ = db.CountPending("p1")
	if n != 0 {
		t.Error("clear should empty queue")
	}
}

func TestEnsureReconcileGenerationIsMonotonic(t *testing.T) {
	db := openTestDB(t)
	p := &Profile{ID: "generation", ProfileUUID: "u-generation", Type: "generic",
		SourcePath: "/src", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	gen, err := db.BumpGeneration(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureReconcileGeneration(p.ID, gen); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureReconcileGeneration(p.ID, gen-1); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetSyncState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.DesiredGeneration != gen {
		t.Fatalf("desired generation = %d, want %d", s.DesiredGeneration, gen)
	}
}

func TestManifestReplace(t *testing.T) {
	db := openTestDB(t)
	entries := []ManifestEntry{
		{ProfileID: "p", RelPath: "a.md", Size: 1, ModTime: 10},
		{ProfileID: "p", RelPath: "b/c.md", Size: 2, ModTime: 20},
	}
	if err := db.ManifestReplaceAll("p", entries); err != nil {
		t.Fatal(err)
	}
	n, _ := db.ManifestCount("p")
	if n != 2 {
		t.Fatalf("count = %d", n)
	}
	if err := db.ManifestReplaceAll("p", entries[:1]); err != nil {
		t.Fatal(err)
	}
	n, _ = db.ManifestCount("p")
	if n != 1 {
		t.Fatalf("replace should delete stale; count = %d", n)
	}
}
