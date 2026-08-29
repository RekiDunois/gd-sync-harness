package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/state"
)

// Manager handles rclone remote operations: backend validation, managed root
// creation, Folder ID capture, and quota.
type Manager struct {
	Rclone *exec.Rclone
	DB     *state.DB
}

// New builds a Manager.
func New(r *exec.Rclone, db *state.DB) *Manager { return &Manager{Rclone: r, DB: db} }

func isDriveBackend(backend string) bool { return backend == "drive" }

// ValidateRemote checks the remote exists, uses the Google Drive backend, and is
// reachable. Returns the backend name.
func (m *Manager) ValidateRemote(ctx context.Context, remote string) (string, error) {
	name := strings.TrimSuffix(remote, ":")
	remotes, err := exec.ListRemotes(ctx, m.Rclone)
	if err != nil {
		return "", err
	}
	found := false
	for _, rr := range remotes {
		if strings.TrimSuffix(rr, ":") == name {
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("rclone remote %q not found in config (rclone listremotes)", name)
	}
	backend, err := exec.RemoteBackend(ctx, m.Rclone, name+":")
	if err != nil {
		return "", err
	}
	if !isDriveBackend(backend) {
		return "", fmt.Errorf("remote %q uses backend %q, but only google drive (drive) is supported", name, backend)
	}
	return backend, nil
}

// CreateManagedRoot creates the remote mirror path under remote root using
// `rclone mkdir` (idempotent) and then resolves the stable Folder ID.
func (m *Manager) CreateManagedRoot(ctx context.Context, remote, remotePath string) (folderID string, err error) {
	res := m.Rclone.Run(ctx, "mkdir", remote+":"+remotePath)
	if res.Err != nil {
		return "", fmt.Errorf("rclone mkdir %s: %w: %s", remote+":"+remotePath, res.Err, res.StderrTrimmed())
	}
	id, err := m.ResolveFolderID(ctx, remote, remotePath)
	if err != nil {
		return "", fmt.Errorf("resolve folder id: %w", err)
	}
	return id, nil
}

// ResolveFolderID returns the stable Google Drive Folder ID for a remote path.
// It walks the path components from the remote root, using `rclone lsjson`
// (which returns each object's `ID`) at each level to descend into the target
// folder. This avoids trusting path strings alone and works with drive.file
// scope where the folder was created by this OAuth app.
func (m *Manager) ResolveFolderID(ctx context.Context, remote, remotePath string) (string, error) {
	name := strings.TrimSuffix(remote, ":")
	parts := strings.Split(strings.Trim(remotePath, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return "", fmt.Errorf("cannot resolve folder id for remote root")
	}

	// Walk from the root: at each level list direct children and find the next
	// component by name (case-sensitive on Drive display names). Google Drive
	// is eventually consistent after mkdir, so retry briefly with backoff.
	current := ""
	for i, part := range parts {
		var found string
		for attempt := 0; attempt < 5; attempt++ {
			lsPath := current
			ls := m.Rclone.Run(ctx, "lsjson", name+":"+lsPath)
			if ls.Err != nil {
				return "", fmt.Errorf("lsjson %s: %w: %s", name+":"+lsPath, ls.Err, ls.StderrTrimmed())
			}
			var entries []struct {
				Path  string `json:"Path"`
				Name  string `json:"Name"`
				ID    string `json:"ID"`
				IsDir bool   `json:"IsDir"`
			}
			if err := json.Unmarshal(ls.Stdout, &entries); err != nil {
				return "", fmt.Errorf("parse lsjson: %w", err)
			}
			for _, e := range entries {
				if e.IsDir && e.Name == part {
					found = e.ID
					break
				}
			}
			if found != "" {
				break
			}
			if attempt < 4 {
				time.Sleep(consistencyRetryDelay(attempt))
			}
		}
		if found == "" {
			return "", fmt.Errorf("folder %q not found under %q on %q (after retries)", part, current, remote)
		}
		current = current + "/" + part
		if i == len(parts)-1 {
			return found, nil
		}
	}
	return "", fmt.Errorf("could not resolve folder id for %q", remotePath)
}

// consistencyRetryDelay returns the backoff delay for the given attempt index,
// scaling up to handle Google Drive eventual consistency after mkdir.
func consistencyRetryDelay(attempt int) time.Duration {
	// 200ms, 400ms, 800ms, 1600ms
	return time.Duration(200*(1<<uint(attempt))) * time.Millisecond
}

// CheckQuota queries and stores quota state for a remote (§24).
func (m *Manager) CheckQuota(ctx context.Context, remote string, warnFreeBytes int64) (*state.Remote, error) {
	name := strings.TrimSuffix(remote, ":")
	q, err := exec.About(ctx, m.Rclone, name+":")
	if err != nil {
		r := &state.Remote{RemoteName: name, Backend: "", QuotaStatus: state.QuotaUnknown}
		r.LastQuotaCheck = state.Now().Format("2006-01-02T15:04:05Z07:00")
		_ = m.DB.UpsertRemote(r)
		return r, err
	}
	status := state.QuotaOK
	if q.Free >= 0 && warnFreeBytes > 0 && q.Free < warnFreeBytes {
		status = state.QuotaLow
	}
	if q.Free == 0 {
		status = state.QuotaFull
	}
	r := &state.Remote{
		RemoteName:     name,
		Backend:        "",
		LastQuotaCheck: state.Now().Format("2006-01-02T15:04:05Z07:00"),
		TotalBytes:     q.Total,
		UsedBytes:      q.Used,
		FreeBytes:      q.Free,
		QuotaStatus:    status,
	}
	if err := m.DB.UpsertRemote(r); err != nil {
		return r, err
	}
	return r, nil
}
