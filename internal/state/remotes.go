package state

// QuotaStatus values.
const (
	QuotaOK      = "ok"
	QuotaLow     = "quota_low"
	QuotaFull    = "quota_full"
	QuotaUnknown = "unknown"
)

// Remote is quota/health state for a single rclone remote.
type Remote struct {
	RemoteName     string `json:"remote_name"`
	Backend        string `json:"backend"`
	LastQuotaCheck string `json:"last_quota_check"`
	TotalBytes     int64  `json:"total_bytes"`
	UsedBytes      int64  `json:"used_bytes"`
	FreeBytes      int64  `json:"free_bytes"`
	QuotaStatus    string `json:"quota_status"`
}

// GetRemote returns remote state.
func (d *DB) GetRemote(name string) (*Remote, error) {
	var r Remote
	var q string
	err := d.QueryRow(`SELECT remote_name, backend, COALESCE(last_quota_check,''), total_bytes, used_bytes, free_bytes, quota_status
		FROM remotes WHERE remote_name = ?`, name).
		Scan(&r.RemoteName, &r.Backend, &r.LastQuotaCheck, &r.TotalBytes, &r.UsedBytes, &r.FreeBytes, &q)
	if err != nil {
		return nil, err
	}
	r.QuotaStatus = q
	return &r, nil
}

// UpsertRemote records the latest quota observation for a remote.
func (d *DB) UpsertRemote(r *Remote) error {
	_, err := d.Exec(`INSERT INTO remotes (remote_name, backend, last_quota_check, total_bytes, used_bytes, free_bytes, quota_status)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (remote_name) DO UPDATE SET
			backend = excluded.backend,
			last_quota_check = excluded.last_quota_check,
			total_bytes = excluded.total_bytes,
			used_bytes = excluded.used_bytes,
			free_bytes = excluded.free_bytes,
			quota_status = excluded.quota_status`,
		r.RemoteName, r.Backend, r.LastQuotaCheck, r.TotalBytes, r.UsedBytes, r.FreeBytes, r.QuotaStatus)
	return err
}
