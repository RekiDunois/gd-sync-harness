package state

import (
	"path/filepath"
	"testing"
)

func TestDerivedClaimCoalescesAndGuardsActiveAttempt(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p := &Profile{ID: "profile", ProfileUUID: "uuid", Type: "generic", SourcePath: t.TempDir(), RemoteName: "remote", RemoteFolderID: "folder", RemoteDisplayPath: "root", Enabled: true}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := db.StartCompilerRun(p.ID, "compile-run", "generation", "compiler", 1, "policy", "contract"); err != nil {
		t.Fatal(err)
	}
	binding := DerivedBindingFingerprint(p.RemoteName, p.RemoteFolderID)
	if err := db.FinishCompilerRun(p.ID, "compile-run", "generation", "snapshot", "policy", "contract", binding, 1, 0); err != nil {
		t.Fatal(err)
	}

	run, claimed, err := db.ClaimDerivedRun(p.ID, "derived-run", binding)
	if err != nil || !claimed {
		t.Fatalf("claim = %+v, %v, %v", run, claimed, err)
	}
	if run.Kind != DerivedRunPublish || run.TargetGenerationID == nil || *run.TargetGenerationID != "generation" {
		t.Fatalf("unexpected derived target: %+v", run)
	}
	if _, claimed, err := db.ClaimDerivedRun(p.ID, "second-run", binding); err != nil || claimed {
		t.Fatalf("active claim should be guarded: claimed=%t err=%v", claimed, err)
	}
	if err := db.FinishDerivedRunSuccess(p.ID, run.ID, binding, run.TargetGenerationID, run.Kind); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := db.ClaimDerivedRun(p.ID, "third-run", binding); err != nil || claimed {
		t.Fatalf("current generation should be coalesced: claimed=%t err=%v", claimed, err)
	}
}

func TestDerivedCleanCreatesPurgeDesire(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &Profile{ID: "profile", ProfileUUID: "uuid", Type: "generic", SourcePath: t.TempDir(), RemoteName: "remote", RemoteFolderID: "folder", RemoteDisplayPath: "root", Enabled: true}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkCompilerClean(p.ID, "clean"); err != nil {
		t.Fatal(err)
	}
	run, claimed, err := db.ClaimDerivedRun(p.ID, "purge-run", DerivedBindingFingerprint("remote", "folder"))
	if err != nil || !claimed || run.Kind != DerivedRunPurge {
		t.Fatalf("purge claim = %+v, %t, %v", run, claimed, err)
	}
}

func TestRecoverDerivedRunsReleasesPin(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &Profile{ID: "profile", ProfileUUID: "uuid", Type: "generic", SourcePath: t.TempDir(), RemoteName: "remote", RemoteFolderID: "folder", RemoteDisplayPath: "root", Enabled: true}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	if err := db.StartCompilerRun(p.ID, "compile-run", "generation", "compiler", 1, "policy", "contract"); err != nil {
		t.Fatal(err)
	}
	binding := DerivedBindingFingerprint("remote", "folder")
	if err := db.FinishCompilerRun(p.ID, "compile-run", "generation", "snapshot", "policy", "contract", binding, 1, 0); err != nil {
		t.Fatal(err)
	}
	run, claimed, err := db.ClaimDerivedRun(p.ID, "derived-run", binding)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := db.RecoverDerivedRuns(); err != nil {
		t.Fatal(err)
	}
	current, err := db.GetCompilerState(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ActivePublishGenerationID != nil || current.DerivedState != "pending" {
		t.Fatalf("recovered compiler state = %+v", current)
	}
	if run.Status != DerivedRunRunning {
		t.Fatalf("claim status changed unexpectedly: %s", run.Status)
	}
}
