package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultAndOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"rclone":{"global_args":["--transfers=8","--checkers=6"],"full_sync_args":["--transfers=12"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := ArgsFor(c, FullSync)
	if len(got) != 2 || got[0] != "--transfers=12" || got[1] != "--checkers=6" {
		t.Fatalf("args = %#v", got)
	}
}

func TestReservedFlagRejected(t *testing.T) {
	if err := Validate(Config{Rclone: RcloneConfig{FullSyncArgs: []string{"--max-delete=999"}}}); err == nil {
		t.Fatal("expected reserved flag error")
	}
}

func TestMissingConfigUsesDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ArgsFor(c, FullSync); len(got) != 1 || got[0] != "--transfers=12" {
		t.Fatalf("args = %#v", got)
	}
}
