package cli

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

type phase3CLIFixture struct {
	dbPath  string
	profile *state.Profile
}

// newPhase3CLIFixture gives commands a private HOME, rclone config, database,
// source tree, and local mock remote. Commands still enter through NewRootCmd.
func newPhase3CLIFixture(t *testing.T, id string) *phase3CLIFixture {
	t.Helper()
	bin, err := osexec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not installed")
	}
	home := t.TempDir()
	remoteRoot := t.TempDir()
	sourceRoot := t.TempDir()
	configPath := filepath.Join(home, "rclone.conf")
	t.Setenv("HOME", home)
	t.Setenv("RCLONE_CONFIG", configPath)
	t.Chdir(remoteRoot)
	mustRun(t, bin, "--config", configPath, "config", "create", "mock", "local")

	app, err := NewLocalApp()
	if err != nil {
		t.Fatal(err)
	}
	p := &state.Profile{
		ID: id, ProfileUUID: id + "-uuid", Type: "generic", SourcePath: sourceRoot,
		RemoteName: "mock", RemoteFolderID: "folder-" + id,
		RemoteDisplayPath: "mirror-" + id, Enabled: true, MaxDelete: 100,
	}
	if err := app.DB.CreateProfileWithPolicy(p, &policy.Snapshot{}); err != nil {
		app.Close()
		t.Fatal(err)
	}
	app.Rclone = rcexec.NewRclone(bin, configPath)
	writeSidecarForTest(t, app, p)
	dbPath, err := filepath.Abs(filepath.Join(home, ".local", "share", "knowledge-sync", "knowledge-sync.sqlite"))
	if err != nil {
		app.Close()
		t.Fatal(err)
	}
	app.Close()
	return &phase3CLIFixture{dbPath: dbPath, profile: p}
}

func runPhase3CLI(args ...string) error {
	cmd := NewRootCmd()
	cmd.SetArgs(args)
	return cmd.Execute()
}

func openPhase3DB(t *testing.T, f *phase3CLIFixture) *state.DB {
	t.Helper()
	db, err := state.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPhase3CLIProfileConfigAndStatus(t *testing.T) {
	f := newPhase3CLIFixture(t, "cli-state")

	for _, args := range [][]string{
		{"profile", "show", f.profile.ID},
		{"profile", "list"},
		{"profile", "status", f.profile.ID},
		{"status", f.profile.ID},
		{"compiler", "status", f.profile.ID, "--verify"},
	} {
		if err := runPhase3CLI(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	customSocket := filepath.Join(t.TempDir(), "custom.sock")
	if err := runPhase3CLI("config", "get", "socket-path"); err != nil {
		t.Fatalf("config get default: %v", err)
	}
	if err := runPhase3CLI("config", "set", "socket-path", customSocket); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if err := runPhase3CLI("config", "get", "socket-path"); err != nil {
		t.Fatalf("config get custom: %v", err)
	}
	if err := runPhase3CLI("config", "unset", "socket-path"); err != nil {
		t.Fatalf("config unset: %v", err)
	}
	db := openPhase3DB(t, f)
	got, err := db.GetSetting(state.SettingWorkerSocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("socket-path after unset = %q, want empty", got)
	}

	if err := runPhase3CLI("status", "missing-profile"); err == nil {
		t.Fatal("status for missing profile must fail")
	}
	if err := runPhase3CLI("profile", "exclude", f.profile.ID, "dir", ".git"); err == nil {
		t.Fatal("deprecated exclude command must fail")
	}
}

func TestPhase3CLIProfileLifecycleAndDryRun(t *testing.T) {
	f := newPhase3CLIFixture(t, "cli-lifecycle")
	drySource := t.TempDir()
	if err := runPhase3CLI("profile", "add", "dry-profile", drySource, "mock", "dry-mirror", "--dry-run"); err != nil {
		t.Fatalf("profile add dry-run: %v", err)
	}
	db := openPhase3DB(t, f)
	if _, err := db.GetProfile("dry-profile"); err == nil {
		t.Fatal("dry-run must not persist a profile")
	}
	_ = db.Close()

	if err := runPhase3CLI("profile", "remove", f.profile.ID, "--force"); err != nil {
		t.Fatalf("profile remove: %v", err)
	}
	db = openPhase3DB(t, f)
	p, err := db.GetProfile(f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.DeletionRequestedAt == nil || p.Tombstoned {
		t.Fatalf("remove state = %+v", p)
	}
	if err := db.TombstoneProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if err := runPhase3CLI("profile", "restore", f.profile.ID); err != nil {
		t.Fatalf("profile restore: %v", err)
	}
	db = openPhase3DB(t, f)
	p, err = db.GetProfile(f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tombstoned || p.DeletionRequestedAt != nil {
		t.Fatalf("restore state = %+v", p)
	}
	if err := db.TombstoneProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := runPhase3CLI("profile", "forget", f.profile.ID, "--force"); err != nil {
		t.Fatalf("profile forget: %v", err)
	}
	db = openPhase3DB(t, f)
	if _, err := db.GetProfile(f.profile.ID); err == nil {
		t.Fatal("forgotten profile must be removed")
	}
}

func TestPhase3CLIIgnoreAndPruneCommands(t *testing.T) {
	f := newPhase3CLIFixture(t, "cli-policy")
	mkTestFile(t, f.profile.SourcePath, ".gitignore", "*.tmp\n")
	if err := runPhase3CLI("profile", "ignore", "update", f.profile.ID); err != nil {
		t.Fatalf("ignore update: %v", err)
	}
	if err := runPhase3CLI("profile", "ignore", "status", f.profile.ID); err != nil {
		t.Fatalf("ignore status: %v", err)
	}

	db := openPhase3DB(t, f)
	snap := &policy.IgnoreSnapshot{Files: []policy.File{
		{RelativePath: ".gitignore", ScopeDir: "", Content: []byte("a.md\n")},
	}}
	if _, err := db.CommitIgnoreSnapshot(f.profile.ID, snap, false); err != nil {
		t.Fatal(err)
	}
	pol, err := db.GetCommittedPolicy(f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPolicyRefreshReady(f.profile.ID, pol.PolicyHash); err != nil {
		t.Fatal(err)
	}
	if err := db.ManifestUpsert(state.ManifestEntry{ProfileID: f.profile.ID, RelPath: "a.md", Size: 1, ModTime: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.ManifestMarkSuppressed(f.profile.ID, "a.md", pol.PolicyHash, 2); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if err := runPhase3CLI("profile", "prune", "preview", f.profile.ID); err != nil {
		t.Fatalf("prune preview: %v", err)
	}
	db = openPhase3DB(t, f)
	req, err := db.GetActivePruneRequest(f.profile.ID)
	if err != nil || req == nil {
		t.Fatalf("active prune request = %+v, err=%v", req, err)
	}
	requestID := req.RequestID
	_ = db.Close()
	if err := runPhase3CLI("profile", "prune", "status", requestID); err != nil {
		t.Fatalf("prune status by request: %v", err)
	}
	if err := runPhase3CLI("profile", "prune", "execute", requestID, "--allow-deletes", "2"); err != nil {
		t.Fatalf("prune execute: %v", err)
	}
	db = openPhase3DB(t, f)
	req, err = db.GetPruneRequest(requestID)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != state.PruneStatePending || req.AuthorizedLimit == nil || *req.AuthorizedLimit != 2 {
		t.Fatalf("authorized prune = %+v", req)
	}
}

func TestPhase3CLIVerifyReconcileWorkerAndCompiler(t *testing.T) {
	f := newPhase3CLIFixture(t, "cli-worker")
	putRemoteTestFile(t, &App{Rclone: rcexec.NewRclone(mustRclone(t), os.Getenv("RCLONE_CONFIG"))}, f.profile, "a.md", "hello")
	if err := runPhase3CLI("verify", f.profile.ID); err != nil {
		t.Fatalf("verify check: %v", err)
	}
	if err := runPhase3CLI("verify", f.profile.ID, "--full"); err != nil {
		t.Fatalf("verify full: %v", err)
	}
	if err := runPhase3CLI("reconcile-scheduled", f.profile.ID); err != nil {
		t.Fatalf("reconcile scheduled: %v", err)
	}
	if err := runPhase3CLI("worker", "--once", f.profile.ID); err != nil {
		t.Fatalf("worker once: %v", err)
	}
	if err := runPhase3CLI("status", f.profile.ID); err != nil {
		t.Fatalf("status after worker: %v", err)
	}
	if err := runPhase3CLI("compiler", "status", f.profile.ID); err != nil {
		t.Fatalf("compiler status: %v", err)
	}
	if err := runPhase3CLI("compiler", "clean", f.profile.ID); err != nil {
		t.Fatalf("compiler clean: %v", err)
	}
	db := openPhase3DB(t, f)
	cs, err := db.GetCompilerState(f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cs.DesiredDerivedMode != state.CompilerDesiredAbsent {
		t.Fatalf("compiler desired mode = %q, want absent", cs.DesiredDerivedMode)
	}
}

func TestPhase3StatusFormattingAndHealthBoundaries(t *testing.T) {
	f := newPhase3CLIFixture(t, "cli-health")
	db := openPhase3DB(t, f)
	rt, err := db.GetRuntime(f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := db.GetSyncState(f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseProfile := *f.profile
	baseRuntime := *rt
	baseSync := *ss
	old := time.Now().Add(-48 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	lastError := "failed"
	baseSync.State = state.StateReady
	baseRuntime.ReconcileRequested = false
	baseRuntime.LastReconcileSuccess = nil
	baseRuntime.LastError = nil

	cases := []struct {
		name    string
		profile state.Profile
		runtime *state.Runtime
		sync    *state.ProfileSyncState
		quota   string
		want    string
	}{
		{name: "tombstoned", profile: state.Profile{Tombstoned: true}, want: "TOMBSTONED"},
		{name: "deleting", profile: state.Profile{DeletionRequestedAt: &old}, want: "DELETING"},
		{name: "disabled", profile: state.Profile{Enabled: false}, want: "DISABLED"},
		{name: "missing runtime", profile: baseProfile, want: "STALE"},
		{name: "initializing", profile: baseProfile, runtime: &baseRuntime, sync: &state.ProfileSyncState{State: state.StateInitializing}, want: "INITIALIZING"},
		{name: "syncing", profile: baseProfile, runtime: &baseRuntime, sync: &state.ProfileSyncState{State: state.StateSyncing}, want: "SYNCING"},
		{name: "terminal", profile: baseProfile, runtime: &baseRuntime, sync: &state.ProfileSyncState{State: state.StateError}, want: "BROKEN"},
		{name: "requested", profile: baseProfile, runtime: &state.Runtime{ReconcileRequested: true}, sync: &baseSync, want: "RECONCILE_REQUESTED"},
		{name: "stale", profile: baseProfile, runtime: &state.Runtime{LastReconcileSuccess: &old}, sync: &baseSync, want: "STALE"},
		{name: "last error", profile: baseProfile, runtime: &state.Runtime{LastError: &lastError}, sync: &baseSync, want: "BROKEN"},
		{name: "quota low", profile: baseProfile, runtime: &baseRuntime, sync: &baseSync, quota: state.QuotaLow, want: "QUOTA_LOW"},
		{name: "quota full", profile: baseProfile, runtime: &baseRuntime, sync: &baseSync, quota: state.QuotaFull, want: "QUOTA_FULL"},
		{name: "healthy", profile: baseProfile, runtime: &baseRuntime, sync: &baseSync, want: "HEALTHY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			quota := tc.quota
			if quota == "" {
				quota = state.QuotaOK
			}
			if err := db.UpsertRemote(&state.Remote{RemoteName: f.profile.RemoteName, QuotaStatus: quota}); err != nil {
				t.Fatal(err)
			}
			if got := computeHealth(&App{DB: db}, &tc.profile, tc.runtime, tc.sync); got != tc.want {
				t.Fatalf("health = %q, want %q", got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		input int64
		want  string
	}{
		{input: 0, want: "0 B"},
		{input: 1024, want: "1.0 KiB"},
		{input: 1024 * 1024, want: "1.0 MiB"},
		{input: 1024 * 1024 * 1024, want: "1.0 GiB"},
	} {
		if got := humanBytes(tc.input); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestPhase3RootHelpSmoke(t *testing.T) {
	if err := runPhase3CLI("--help"); err != nil {
		t.Fatalf("root help: %v", err)
	}
}

func mustRclone(t *testing.T) string {
	t.Helper()
	bin, err := osexec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not installed")
	}
	return bin
}
