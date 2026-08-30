package cli

import (
	"path/filepath"
	"testing"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

// TestLiveReaderBuildsSnapshotFromDurable verifies the worker durable cache
// composes a full status snapshot from SQLite state (§6.1).
func TestLiveReaderBuildsSnapshotFromDurable(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "live.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &state.Profile{
		ID: "example-profile", ProfileUUID: "u-live", Type: "generic",
		SourcePath: "/vault", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true, MaxDelete: 100,
	}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}

	reader := newLiveDurableReader(db)
	snap := reader.BuildSnapshot(p.ID, nil)
	if snap == nil {
		t.Fatal("snapshot must be built for existing profile")
	}
	if !snap.Profile.Enabled || snap.Profile.Tombstoned {
		t.Fatalf("profile lifecycle = %+v", snap.Profile)
	}
	if snap.Sync.State != state.StateInitializing || snap.Sync.DesiredGeneration != 1 {
		t.Fatalf("sync = %+v", snap.Sync)
	}
	// The server stamps protocol fields at publish time.
	v := snap.Versioned()
	if v.ProtocolVersion != live.ProtocolVersion || v.Type != live.MsgTypeStatus {
		t.Fatalf("protocol = %d/%s", v.ProtocolVersion, v.Type)
	}

	// Unknown profile → nil.
	if snap := reader.BuildSnapshot("no-such-profile", nil); snap != nil {
		t.Fatal("unknown profile must produce nil snapshot")
	}
}

// TestLiveReaderActivityMerge verifies live activity is merged over the durable
// snapshot without a durable re-read.
func TestLiveReaderActivityMerge(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "live2.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &state.Profile{
		ID: "example-profile", ProfileUUID: "u-live2", Type: "generic",
		SourcePath: "/vault", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true,
	}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	reader := newLiveDurableReader(db)
	_ = reader.Refresh(p.ID)

	activity := &live.ActivityS{
		Kind: live.ActivityFullReconcile, Phase: "uploading",
		BytesCompleted: 2048, BytesTotal: 8192, SpeedKnown: true, SpeedBytesPerSecond: 1024,
	}
	snap := reader.BuildSnapshot(p.ID, activity)
	if snap.Activity == nil {
		t.Fatal("activity must be present")
	}
	if snap.Activity.Kind != live.ActivityFullReconcile || snap.Activity.BytesCompleted != 2048 {
		t.Fatalf("activity = %+v", snap.Activity)
	}
	if !snap.Activity.SpeedKnown || snap.Activity.SpeedBytesPerSecond != 1024 {
		t.Fatalf("speed = %+v", snap.Activity)
	}
	// The snapshot reflects the cached durable state (state from DB create).
	if snap.Sync.State != state.StateInitializing {
		t.Fatalf("sync state = %s", snap.Sync.State)
	}
}

// TestLiveReaderRunRowFallbackActivity verifies that without live activity the
// snapshot presents the durable run row as coarse activity (§9.4 fallback).
func TestLiveReaderRunRowFallbackActivity(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "live3.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p := &state.Profile{
		ID: "example-profile", ProfileUUID: "u-live3", Type: "generic",
		SourcePath: "/vault", RemoteName: "example-remote", RemoteFolderID: "f",
		RemoteDisplayPath: "mirror", Enabled: true,
	}
	if err := db.CreateProfile(p); err != nil {
		t.Fatal(err)
	}
	reader := newLiveDurableReader(db)
	run, res, err := db.ClaimRun(p.ID, "run-live3")
	if err != nil || res != state.ClaimOK {
		t.Fatalf("claim: res=%v err=%v", res, err)
	}
	_ = db.UpdateRunStats(p.ID, run.ID, state.ProgressSnapshot{
		FilesCompleted: 3, BytesCompleted: 4096, BytesTotal: 16384,
	}, true)
	_ = reader.Refresh(p.ID)
	snap := reader.BuildSnapshot(p.ID, nil)
	if snap.Activity == nil {
		t.Fatal("run-row fallback activity must be present")
	}
	if snap.Activity.FilesCompleted != 3 || snap.Activity.BytesCompleted != 4096 {
		t.Fatalf("fallback activity = %+v", snap.Activity)
	}
	if snap.Activity.SpeedKnown {
		t.Fatal("durable fallback must not claim live speed")
	}
}
