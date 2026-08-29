package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	res := m.Rclone.Run(ctx, "mkdir", remote+remotePath)
	if res.Err != nil {
		return "", fmt.Errorf("rclone mkdir %s: %w: %s", remote+remotePath, res.Err, res.StderrTrimmed())
	}
	id, err := m.ResolveFolderID(ctx, remote, remotePath)
	if err != nil {
		return "", fmt.Errorf("resolve folder id: %w", err)
	}
	return id, nil
}

// ResolveFolderID returns the Google Drive Folder ID for a remote path using
// `rclone backend get` first, then `rclone lsd --json`.
func (m *Manager) ResolveFolderID(ctx context.Context, remote, remotePath string) (string, error) {
	name := strings.TrimSuffix(remote, ":")
	res := m.Rclone.Run(ctx, "backend", "get", name+":"+remotePath)
	if res.Err == nil && len(res.Stdout) > 0 {
		var meta map[string]any
		if err := json.Unmarshal(res.Stdout, &meta); err == nil {
			if id, ok := meta["id"].(string); ok && id != "" {
				return id, nil
			}
		}
	}
	parent := ""
	leaf := remotePath
	if idx := strings.LastIndex(remotePath, "/"); idx >= 0 {
		parent = remotePath[:idx]
		leaf = remotePath[idx+1:]
	}
	ls := m.Rclone.Run(ctx, "lsd", "--json", name+":"+parent)
	if ls.Err != nil {
		return "", fmt.Errorf("lsd %s: %w: %s", name+":"+parent, ls.Err, ls.StderrTrimmed())
	}
	var entries []struct {
		Path string `json:"Path"`
		ID   string `json:"ID"`
	}
	if err := json.Unmarshal(ls.Stdout, &entries); err != nil {
		return "", fmt.Errorf("parse lsd: %w", err)
	}
	for _, e := range entries {
		if e.Path == leaf {
			return e.ID, nil
		}
	}
	return "", fmt.Errorf("folder %q not found under %q on %q", leaf, parent, remote)
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
