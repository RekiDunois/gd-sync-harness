package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/state"
)

// Sidecar is the remote ownership metadata stored outside the mirror root (§7).
type Sidecar struct {
	SchemaVersion  int    `json:"schema_version"`
	ProfileID      string `json:"profile_id"`
	ProfileUUID    string `json:"profile_uuid"`
	RemoteFolderID string `json:"remote_folder_id"`
	CreatedAt      string `json:"created_at"`
	RemoteName     string `json:"remote_name"`
}

// DerivedSidecar identifies the compiler-owned Chat-visible namespace. It is
// kept under .knowledge-sync so the derived root contains only artifacts.
type DerivedSidecar struct {
	SchemaVersion            int    `json:"schema_version"`
	ProfileID                string `json:"profile_id"`
	ProfileUUID              string `json:"profile_uuid"`
	RemoteName               string `json:"remote_name"`
	RemoteFolderID           string `json:"remote_folder_id"`
	RemoteBindingFingerprint string `json:"remote_binding_fingerprint"`
	DerivedPath              string `json:"derived_path"`
	CreatedAt                string `json:"created_at"`
}

// ValidationError distinguishes a permanent ownership mismatch from an
// ownership check that could not be completed because transport was temporary.
type ValidationError struct {
	Code      string
	Temporary bool
	Err       error
}

func (e *ValidationError) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

const schemaVersion = 1
const derivedSchemaVersion = 1

// RemoteMetadataRoot is the harness-owned metadata directory on each Drive.
const RemoteMetadataRoot = ".knowledge-sync"

// RemoteProfilesDir is the profiles subdirectory.
func RemoteProfilesDir() string { return RemoteMetadataRoot + "/profiles" }

// SidecarPath returns the remote path of a profile's sidecar.
func SidecarPath(profileUUID string) string {
	return RemoteProfilesDir() + "/" + profileUUID + ".json"
}

func DerivedSidecarPath(profileUUID string) string {
	return RemoteMetadataRoot + "/derived/" + profileUUID + ".json"
}

// Read fetches and parses the sidecar from the remote root path.
func Read(ctx context.Context, r *exec.Rclone, remote, remotePath string) (*Sidecar, error) {
	tmp, err := os.CreateTemp("", "ks-sidecar-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	res := r.Run(ctx, "copyto", remote+":"+remotePath, tmp.Name())
	if res.Err != nil {
		return nil, fmt.Errorf("sidecar fetch %s: %w: %s", remote+":"+remotePath, res.Err, res.StderrTrimmed())
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	var s Sidecar
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("sidecar parse: %w", err)
	}
	return &s, nil
}

// Write uploads the sidecar JSON to the remote root path.
func Write(ctx context.Context, r *exec.Rclone, remote string, s *Sidecar) error {
	return writeJSON(ctx, r, remote, RemoteProfilesDir()+"/"+s.ProfileUUID+".json", s, "sidecar write")
}

func writeJSON(ctx context.Context, r *exec.Rclone, remote, remotePath string, value any, operation string) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "ks-sidecar-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	res := r.Run(ctx, "copyto", tmp.Name(), remote+":"+remotePath)
	if res.Err != nil {
		return fmt.Errorf("%s: %w: %s", operation, res.Err, res.StderrTrimmed())
	}
	return nil
}

func ReadDerived(ctx context.Context, r *exec.Rclone, remote, profileUUID string) (*DerivedSidecar, error) {
	tmp, err := os.CreateTemp("", "ks-derived-sidecar-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_ = tmp.Close()
	res := r.Run(ctx, "copyto", remote+":"+DerivedSidecarPath(profileUUID), tmp.Name())
	if res.Err != nil {
		return nil, fmt.Errorf("derived sidecar fetch: %w: %s", res.Err, res.StderrTrimmed())
	}
	b, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	var value DerivedSidecar
	if err := json.Unmarshal(b, &value); err != nil {
		return nil, fmt.Errorf("derived sidecar parse: %w", err)
	}
	return &value, nil
}

func WriteDerived(ctx context.Context, r *exec.Rclone, remote string, s *DerivedSidecar) error {
	return writeJSON(ctx, r, remote, DerivedSidecarPath(s.ProfileUUID), s, "derived sidecar write")
}

func DerivedExists(ctx context.Context, r *exec.Rclone, remote, profileUUID string) (bool, error) {
	res := r.Run(ctx, "lsf", remote+":"+RemoteMetadataRoot+"/derived", "--files-only")
	if res.Err != nil {
		return false, fmt.Errorf("derived sidecar ls: %w: %s", res.Err, res.StderrTrimmed())
	}
	for _, line := range strings.Split(res.StdoutTrimmed(), "\n") {
		if strings.TrimSpace(line) == profileUUID+".json" {
			return true, nil
		}
	}
	return false, nil
}

func CreateDerived(p *state.Profile, binding string) *DerivedSidecar {
	return &DerivedSidecar{
		SchemaVersion: derivedSchemaVersion, ProfileID: p.ID, ProfileUUID: p.ProfileUUID,
		RemoteName: strings.TrimSuffix(p.RemoteName, ":"), RemoteFolderID: p.RemoteFolderID,
		RemoteBindingFingerprint: binding, DerivedPath: ".knowledge-derived",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func ValidateDerived(s *DerivedSidecar, p *state.Profile, binding string) error {
	if s == nil || s.SchemaVersion != derivedSchemaVersion || s.ProfileID != p.ID ||
		s.ProfileUUID != p.ProfileUUID || s.RemoteName != strings.TrimSuffix(p.RemoteName, ":") ||
		s.RemoteFolderID != p.RemoteFolderID || s.RemoteBindingFingerprint != binding ||
		s.DerivedPath != ".knowledge-derived" {
		return &ValidationError{Code: "derived_ownership_mismatch", Err: fmt.Errorf("derived sidecar does not match current profile binding")}
	}
	return nil
}

// Exists reports whether a sidecar exists at the expected remote path.
func Exists(ctx context.Context, r *exec.Rclone, remote, profileUUID string) (bool, error) {
	res := r.Run(ctx, "lsf", remote+":"+RemoteProfilesDir(), "--files-only")
	if res.Err != nil {
		return false, fmt.Errorf("sidecar ls: %w: %s", res.Err, res.StderrTrimmed())
	}
	for _, line := range strings.Split(res.StdoutTrimmed(), "\n") {
		if strings.TrimSpace(line) == profileUUID+".json" {
			return true, nil
		}
	}
	return false, nil
}

// Validate runs the four fail-closed ownership checks (§7.1-7.4).
func Validate(ctx context.Context, r *exec.Rclone, p *state.Profile, remotePath string) error {
	sc, err := Read(ctx, r, p.RemoteName, SidecarPath(p.ProfileUUID))
	if err != nil {
		temporary := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 5 {
			temporary = true
		}
		code := "ownership_missing"
		if temporary {
			code = "ownership_unavailable"
		}
		return &ValidationError{Code: code, Temporary: temporary,
			Err: fmt.Errorf("ownership fail: sidecar missing/unreadable: %w", err)}
	}
	if sc.ProfileUUID != p.ProfileUUID {
		return &ValidationError{Code: "ownership_uuid_mismatch", Err: fmt.Errorf("ownership fail: sidecar UUID %q != profile UUID %q", sc.ProfileUUID, p.ProfileUUID)}
	}
	if sc.RemoteFolderID != p.RemoteFolderID {
		return &ValidationError{Code: "ownership_folder_mismatch", Err: fmt.Errorf("ownership fail: sidecar folder_id %q != profile folder_id %q", sc.RemoteFolderID, p.RemoteFolderID)}
	}
	if sc.RemoteName != p.RemoteName {
		return &ValidationError{Code: "ownership_remote_mismatch", Err: fmt.Errorf("ownership fail: sidecar remote %q != profile remote %q", sc.RemoteName, p.RemoteName)}
	}
	if sc.SchemaVersion != schemaVersion || sc.ProfileID != p.ID || sc.RemoteFolderID == "" || strings.ContainsAny(sc.RemoteFolderID, "/\\") || path.Clean(sc.RemoteFolderID) != sc.RemoteFolderID {
		return &ValidationError{Code: "ownership_metadata_malformed", Err: fmt.Errorf("ownership fail: malformed sidecar metadata")}
	}
	return nil
}

// Create builds a new sidecar for a freshly-created mirror root.
func Create(p *state.Profile) *Sidecar {
	return &Sidecar{
		SchemaVersion:  schemaVersion,
		ProfileID:      p.ID,
		ProfileUUID:    p.ProfileUUID,
		RemoteFolderID: p.RemoteFolderID,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		RemoteName:     p.RemoteName,
	}
}
