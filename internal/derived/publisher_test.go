package derived

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/paths"
	"knowledge-sync/internal/sidecar"
	"knowledge-sync/internal/state"
)

func TestPublishDoesNotCommitManifestWhenCheckFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	remoteRoot := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(filepath.Join(remoteRoot, ".knowledge-sync", "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := &state.Profile{ID: "profile", ProfileUUID: "uuid", Type: "generic", SourcePath: t.TempDir(), RemoteName: "remote", RemoteFolderID: "folder", RemoteDisplayPath: "root", Enabled: true}
	ordinary, err := json.Marshal(sidecar.Create(profile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteRoot, ".knowledge-sync", "profiles", "uuid.json"), ordinary, 0o644); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "rclone.log")
	script := filepath.Join(t.TempDir(), "fake-rclone.sh")
	scriptBody := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_RCLONE_LOG"
cmd=""
for arg in "$@"; do
  case "$arg" in
    lsjson|lsf|mkdir|copyto|sync|check) cmd="$arg"; break ;;
  esac
done
case "$cmd" in
  lsjson)
    target="$2"
    case "$target" in
      remote:) printf '[{"Name":"root","ID":"folder","IsDir":true}]\n' ;;
      remote:root/.knowledge-derived)
        if [ -e "$FAKE_REMOTE_ROOT/root/.knowledge-derived" ]; then printf '[]\n'; else exit 3; fi ;;
      *) printf '[]\n' ;;
    esac ;;
  lsf)
    if [ -e "$FAKE_REMOTE_ROOT/.knowledge-sync/derived" ]; then printf '\n'; else exit 3; fi ;;
  mkdir)
    target="$2"
    path=${target#remote:}
    mkdir -p "$FAKE_REMOTE_ROOT/$path" ;;
  copyto)
    src="$2"
    dst="$3"
    case "$src" in
      remote:*) cp "$FAKE_REMOTE_ROOT/${src#remote:}" "$dst" ;;
      *) mkdir -p "$FAKE_REMOTE_ROOT/$(dirname "${dst#remote:}")"; cp "$src" "$FAKE_REMOTE_ROOT/${dst#remote:}" ;;
    esac ;;
  check) exit 1 ;;
  sync) exit 0 ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RCLONE_ROOT", remoteRoot)
	t.Setenv("FAKE_REMOTE_ROOT", remoteRoot)
	t.Setenv("FAKE_RCLONE_LOG", logPath)
	db, err := state.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	publisher := NewPublisher(&exec.Rclone{Binary: script}, db)
	compilerRoot, err := paths.CompilerRoot(profile.ProfileUUID)
	if err != nil {
		t.Fatal(err)
	}
	generationRoot := filepath.Join(compilerRoot, "generations", "generation")
	if err := os.MkdirAll(generationRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generationRoot, "DETAIL.json"), []byte("detail"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generationRoot, "MANIFEST.json"), []byte(`{"compile_generation_id":"generation"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	err = publisher.Publish(context.Background(), profile, "generation", nil, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "derived detail check") {
		t.Fatalf("Publish error = %v, want detail-check failure", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "sync ") || !strings.Contains(string(log), "check ") {
		t.Fatalf("rclone log lacks sync/check: %s", log)
	}
	if strings.Contains(string(log), "MANIFEST.json remote:") {
		t.Fatalf("manifest commit ran after check failure: %s", log)
	}
}
