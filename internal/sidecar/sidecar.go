package sidecar

import (
	"context"
	"encoding/json"
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

const schemaVersion = 1

// RemoteMetadataRoot is the harness-owned metadata directory on each Drive.
const RemoteMetadataRoot = ".knowledge-sync"

// RemoteProfilesDir is the profiles subdirectory.
func RemoteProfilesDir() string { return RemoteMetadataRoot + "/profiles" }

// SidecarPath returns the remote path of a profile's sidecar.
func SidecarPath(profileUUID string) string {
	return RemoteProfilesDir() + "/" + profileUUID + ".json"
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
	b, err := json.MarshalIndent(s, "", "  ")
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

	res := r.Run(ctx, "copyto", tmp.Name(), remote+":"+RemoteProfilesDir()+"/"+s.ProfileUUID+".json")
	if res.Err != nil {
		return fmt.Errorf("sidecar write: %w: %s", res.Err, res.StderrTrimmed())
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
		return fmt.Errorf("ownership fail: sidecar missing/unreadable: %w", err)
	}
	if sc.ProfileUUID != p.ProfileUUID {
		return fmt.Errorf("ownership fail: sidecar UUID %q != profile UUID %q", sc.ProfileUUID, p.ProfileUUID)
	}
	if sc.RemoteFolderID != p.RemoteFolderID {
		return fmt.Errorf("ownership fail: sidecar folder_id %q != profile folder_id %q", sc.RemoteFolderID, p.RemoteFolderID)
	}
	if sc.RemoteName != p.RemoteName {
		return fmt.Errorf("ownership fail: sidecar remote %q != profile remote %q", sc.RemoteName, p.RemoteName)
	}
	if !strings.HasPrefix(path.Clean("/"+sc.RemoteFolderID+"/"), "/") {
		return fmt.Errorf("ownership fail: malformed folder id %q", sc.RemoteFolderID)
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
