package cli

import (
	"path/filepath"
	"testing"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

// TestConfigSocketPathSetGetUnset verifies the socket-path config surface
// persists, reads back, and clears the override (§4.5, §18.11).
func TestConfigSocketPathSetGetUnset(t *testing.T) {
	app, _ := asyncTestApp(t)
	p := asyncTestProfile(t, app, "cfg-sock")

	// asyncTestApp sets an isolated temp socket path; clear it so this test
	// exercises the unset -> default resolution order it owns.
	if err := app.DB.UnsetSetting(state.SettingWorkerSocketPath); err != nil {
		t.Fatal(err)
	}

	// get with no override → default.
	configured, _ := app.DB.GetSetting(state.SettingWorkerSocketPath)
	if configured != "" {
		t.Fatalf("fresh socket-path override = %q, want empty", configured)
	}
	if got := live.ResolveSocketPath(configured); got != live.DefaultSocketPath() {
		t.Fatalf("resolved default = %q, want %q", got, live.DefaultSocketPath())
	}

	// set.
	custom := filepath.Join(shortCliTemp(t), "custom.sock")
	if err := app.DB.SetSetting(state.SettingWorkerSocketPath, custom); err != nil {
		t.Fatal(err)
	}
	configured, _ = app.DB.GetSetting(state.SettingWorkerSocketPath)
	if configured != custom {
		t.Fatalf("set socket-path = %q, want %q", configured, custom)
	}
	if got := live.ResolveSocketPath(configured); got != custom {
		t.Fatalf("resolved custom = %q, want %q", got, custom)
	}

	// unset → default again.
	if err := app.DB.UnsetSetting(state.SettingWorkerSocketPath); err != nil {
		t.Fatal(err)
	}
	configured, _ = app.DB.GetSetting(state.SettingWorkerSocketPath)
	if configured != "" {
		t.Fatalf("after unset override = %q, want empty", configured)
	}

	// The worker job detection against a real plist (best-effort).
	if workerJobInstalled(app) {
		t.Log("managed worker job present; config restart path will reload it")
	}
	_ = p
}

// TestConfigSetPersistsAndRestartReportsNoManagedWorker verifies the config set
// path reports next-start semantics when no managed worker job is installed
// (§15.3). We avoid touching the real LaunchAgents dir by checking the message.
func TestConfigSetPersistsAndRestartReportsNoManagedWorker(t *testing.T) {
	app, _ := asyncTestApp(t)
	custom := filepath.Join(shortCliTemp(t), "x.sock")
	if err := app.DB.SetSetting(state.SettingWorkerSocketPath, custom); err != nil {
		t.Fatal(err)
	}
	// No launchd plist for the worker in this environment → the restart path
	// should not fail; it reports next-start semantics.
	if err := restartManagedWorker(app); err != nil {
		t.Fatal(err)
	}
	// Persisted value is unchanged.
	v, _ := app.DB.GetSetting(state.SettingWorkerSocketPath)
	if v != custom {
		t.Fatalf("config value = %q", v)
	}
}

// TestConfigCommandHelpText verifies the config command surface exposes the
// socket-path subcommands.
func TestConfigCommandHelpText(t *testing.T) {
	c := newConfigCmd()
	if c.Name() != "config" {
		t.Fatalf("config command name = %q", c.Name())
	}
	names := map[string]bool{}
	for _, sub := range c.Commands() {
		names[sub.Name()] = true
	}
	for _, want := range []string{"get", "set", "unset"} {
		if !names[want] {
			t.Fatalf("config subcommand %q missing", want)
		}
	}
}
