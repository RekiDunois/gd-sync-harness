package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathsUsePrivateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	configDir, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "knowledge-sync"); configDir != want {
		t.Fatalf("ConfigDir = %q, want %q", configDir, want)
	}
	if got, _ := AppConfigPath(); got != filepath.Join(configDir, "config.json") {
		t.Fatalf("AppConfigPath = %q", got)
	}
	stateDir, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "knowledge-sync"); stateDir != want {
		t.Fatalf("StateDir = %q, want %q", stateDir, want)
	}
	if got, _ := DBPath(); got != filepath.Join(stateDir, "knowledge-sync.sqlite") {
		t.Fatalf("DBPath = %q", got)
	}
	if got, _ := BackupsDir(); got != filepath.Join(stateDir, "backups") {
		t.Fatalf("BackupsDir = %q", got)
	}
	if got, _ := LogsDir(); got != filepath.Join(home, "Library", "Logs", "knowledge-sync") {
		t.Fatalf("LogsDir = %q", got)
	}
	if got, _ := LaunchAgentsDir(); got != filepath.Join(home, "Library", "LaunchAgents") {
		t.Fatalf("LaunchAgentsDir = %q", got)
	}

	if err := Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{configDir, stateDir, filepath.Join(stateDir, "backups"), filepath.Join(home, "Library", "Logs", "knowledge-sync"), filepath.Join(home, "Library", "LaunchAgents")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Ensure did not create %q: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("Ensure created non-directory %q", dir)
		}
	}
}

func TestCompilerPathsValidateProfileUUID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := CompilerRoot("profile-uuid")
	if err != nil {
		t.Fatal(err)
	}
	stateDir, _ := StateDir()
	if want := filepath.Join(stateDir, "compiler", "profile-uuid"); root != want {
		t.Fatalf("CompilerRoot = %q, want %q", root, want)
	}
	if got, _ := CompilerGenerationsDir("profile-uuid"); got != filepath.Join(root, "generations") {
		t.Fatalf("CompilerGenerationsDir = %q", got)
	}
	if got, _ := CompilerStagingDir("profile-uuid"); got != filepath.Join(root, "staging") {
		t.Fatalf("CompilerStagingDir = %q", got)
	}
	if got := CompilerLockName("profile-uuid"); got != "compiler-profile-uuid" {
		t.Fatalf("CompilerLockName = %q", got)
	}
	for _, invalid := range []string{"", ".", "..", "nested/name", `nested\name`} {
		if _, err := CompilerRoot(invalid); err == nil {
			t.Errorf("CompilerRoot(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestEnsureReportsHomePathCreationFailure(t *testing.T) {
	home := t.TempDir()
	blocked := filepath.Join(home, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocked)
	if err := Ensure(); err == nil {
		t.Fatal("Ensure unexpectedly succeeded below a regular file")
	}
}
