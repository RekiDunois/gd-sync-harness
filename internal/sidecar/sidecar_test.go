package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/state"
)

func sidecarProfile() *state.Profile {
	return &state.Profile{
		ID: "profile", ProfileUUID: "profile-uuid", Type: "generic",
		SourcePath: "/source", RemoteName: "example-drive", RemoteFolderID: "folder-id",
		RemoteDisplayPath: "Knowledge Mirror/Notes", Enabled: true,
	}
}

func sidecarRclone(t *testing.T) (*rcexec.Rclone, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(t.TempDir(), "fake-rclone.sh")
	script := `#!/bin/sh
set -eu
command=""
first=""
second=""
capture=0
for arg in "$@"; do
  if [ "$capture" -eq 1 ]; then
    if [ -z "$first" ]; then first="$arg"; else second="$arg"; fi
  fi
  case "$arg" in
    copyto|lsf) command="$arg"; capture=1 ;;
  esac
done
case "$command" in
  copyto)
    case "$first" in
      *:*)
        if [ "${FAKE_SIDECAR_MODE:-}" = "missing" ]; then exit 5; fi
        cp "$FAKE_SIDECAR_ROOT/${first#*:}" "$second" ;;
      *)
        mkdir -p "$FAKE_SIDECAR_ROOT/$(dirname "${second#*:}")"
        cp "$first" "$FAKE_SIDECAR_ROOT/${second#*:}" ;;
    esac ;;
  lsf)
    dir="$FAKE_SIDECAR_ROOT/${first#*:}"
    [ -d "$dir" ] || exit 3
    for file in "$dir"/*; do
      [ -f "$file" ] && basename "$file"
    done ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SIDECAR_ROOT", root)
	return &rcexec.Rclone{Binary: bin}, root
}

func writeRemoteJSON(t *testing.T, root, remotePath string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(remotePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSidecarPathsAndCreate(t *testing.T) {
	if got := RemoteMetadataRoot; got != ".knowledge-sync" {
		t.Fatalf("metadata root = %q", got)
	}
	if got := RemoteProfilesDir(); got != ".knowledge-sync/profiles" {
		t.Fatalf("profiles dir = %q", got)
	}
	if got := SidecarPath("u"); got != ".knowledge-sync/profiles/u.json" {
		t.Fatalf("sidecar path = %q", got)
	}
	if got := DerivedSidecarPath("u"); got != ".knowledge-sync/derived/u.json" {
		t.Fatalf("derived path = %q", got)
	}

	p := sidecarProfile()
	s := Create(p)
	if s.SchemaVersion != schemaVersion || s.ProfileID != p.ID || s.ProfileUUID != p.ProfileUUID ||
		s.RemoteFolderID != p.RemoteFolderID || s.RemoteName != p.RemoteName {
		t.Fatalf("created sidecar = %+v", s)
	}
	if _, err := time.Parse(time.RFC3339, s.CreatedAt); err != nil {
		t.Fatalf("created_at = %q: %v", s.CreatedAt, err)
	}
}

func TestSidecarReadWriteAndExists(t *testing.T) {
	r, root := sidecarRclone(t)
	p := sidecarProfile()
	s := Create(p)
	if err := Write(context.Background(), r, p.RemoteName, s); err != nil {
		t.Fatal(err)
	}
	got, err := Read(context.Background(), r, p.RemoteName, SidecarPath(p.ProfileUUID))
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileUUID != s.ProfileUUID || got.RemoteFolderID != s.RemoteFolderID {
		t.Fatalf("read sidecar = %+v, want %+v", got, s)
	}
	exists, err := Exists(context.Background(), r, p.RemoteName, p.ProfileUUID)
	if err != nil || !exists {
		t.Fatalf("Exists = %v, %v", exists, err)
	}
	exists, err = Exists(context.Background(), r, p.RemoteName, "other")
	if err != nil || exists {
		t.Fatalf("missing Exists = %v, %v", exists, err)
	}

	d := CreateDerived(p, "binding-hash")
	if err := WriteDerived(context.Background(), r, p.RemoteName, d); err != nil {
		t.Fatal(err)
	}
	gotDerived, err := ReadDerived(context.Background(), r, p.RemoteName, p.ProfileUUID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDerived.RemoteBindingFingerprint != d.RemoteBindingFingerprint || gotDerived.DerivedPath != d.DerivedPath {
		t.Fatalf("read derived sidecar = %+v, want %+v", gotDerived, d)
	}
	exists, err = DerivedExists(context.Background(), r, p.RemoteName, p.ProfileUUID)
	if err != nil || !exists {
		t.Fatalf("DerivedExists = %v, %v", exists, err)
	}

	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(DerivedSidecarPath(p.ProfileUUID)))); err != nil {
		t.Fatalf("derived sidecar was not written: %v", err)
	}
}

func TestValidateOwnershipBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Sidecar)
		code      string
		temporary bool
	}{
		{name: "valid"},
		{name: "uuid mismatch", mutate: func(s *Sidecar) { s.ProfileUUID = "other" }, code: "ownership_uuid_mismatch"},
		{name: "folder mismatch", mutate: func(s *Sidecar) { s.RemoteFolderID = "other" }, code: "ownership_folder_mismatch"},
		{name: "remote mismatch", mutate: func(s *Sidecar) { s.RemoteName = "other" }, code: "ownership_remote_mismatch"},
		{name: "schema mismatch", mutate: func(s *Sidecar) { s.SchemaVersion = 99 }, code: "ownership_metadata_malformed"},
		{name: "profile mismatch", mutate: func(s *Sidecar) { s.ProfileID = "other" }, code: "ownership_metadata_malformed"},
		{name: "empty folder", mutate: func(s *Sidecar) { s.RemoteFolderID = "" }, code: "ownership_folder_mismatch"},
		{name: "nested folder", mutate: func(s *Sidecar) { s.RemoteFolderID = "a/b" }, code: "ownership_folder_mismatch"},
		{name: "missing temporary", code: "ownership_unavailable", temporary: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, root := sidecarRclone(t)
			p := sidecarProfile()
			if tt.name == "missing temporary" {
				t.Setenv("FAKE_SIDECAR_MODE", "missing")
			} else {
				s := Create(p)
				if tt.mutate != nil {
					tt.mutate(s)
				}
				writeRemoteJSON(t, root, SidecarPath(p.ProfileUUID), s)
			}
			err := Validate(context.Background(), r, p, p.RemoteDisplayPath)
			if tt.code == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want success", err)
				}
				return
			}
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("Validate error = %v, want ValidationError", err)
			}
			if validationErr.Code != tt.code || validationErr.Temporary != tt.temporary {
				t.Fatalf("validation error = %+v, want code=%q temporary=%v", validationErr, tt.code, tt.temporary)
			}
		})
	}
}

func TestValidateReportsMalformedJSONAndTransportErrors(t *testing.T) {
	for _, test := range []struct {
		name      string
		mode      string
		body      string
		code      string
		temporary bool
	}{
		{name: "malformed json", body: "not-json", code: "ownership_missing"},
		{name: "temporary copy failure", mode: "missing", code: "ownership_unavailable", temporary: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, root := sidecarRclone(t)
			p := sidecarProfile()
			if test.mode != "" {
				t.Setenv("FAKE_SIDECAR_MODE", test.mode)
			} else {
				path := filepath.Join(root, filepath.FromSlash(SidecarPath(p.ProfileUUID)))
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(test.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := Validate(context.Background(), r, p, "ignored")
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) || validationErr.Code != test.code || validationErr.Temporary != test.temporary {
				t.Fatalf("Validate = %v, want code=%q temporary=%v", err, test.code, test.temporary)
			}
		})
	}
}

func TestValidateDerivedOwnership(t *testing.T) {
	p := sidecarProfile()
	valid := CreateDerived(p, "binding")
	if err := ValidateDerived(valid, p, "binding"); err != nil {
		t.Fatalf("valid derived sidecar: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*DerivedSidecar)
	}{
		{name: "nil", mutate: nil},
		{name: "schema", mutate: func(s *DerivedSidecar) { s.SchemaVersion = 2 }},
		{name: "profile", mutate: func(s *DerivedSidecar) { s.ProfileID = "other" }},
		{name: "uuid", mutate: func(s *DerivedSidecar) { s.ProfileUUID = "other" }},
		{name: "remote", mutate: func(s *DerivedSidecar) { s.RemoteName = "other" }},
		{name: "folder", mutate: func(s *DerivedSidecar) { s.RemoteFolderID = "other" }},
		{name: "binding", mutate: func(s *DerivedSidecar) { s.RemoteBindingFingerprint = "other" }},
		{name: "path", mutate: func(s *DerivedSidecar) { s.DerivedPath = "other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var candidate *DerivedSidecar
			if tt.name != "nil" {
				candidate = &DerivedSidecar{}
				*candidate = *valid
				tt.mutate(candidate)
			}
			err := ValidateDerived(candidate, p, "binding")
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) || validationErr.Code != "derived_ownership_mismatch" {
				t.Fatalf("ValidateDerived = %v, want derived_ownership_mismatch", err)
			}
		})
	}

	withColon := *p
	withColon.RemoteName = p.RemoteName + ":"
	if got := CreateDerived(&withColon, "binding").RemoteName; got != p.RemoteName {
		t.Fatalf("derived remote name = %q, want %q", got, p.RemoteName)
	}
	if err := ValidateDerived(CreateDerived(&withColon, "binding"), &withColon, "binding"); err != nil {
		t.Fatalf("colon-terminated remote should validate: %v", err)
	}
}
