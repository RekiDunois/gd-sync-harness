package state

import "database/sql"

// Settings keys for persisted tool paths (§31.2).
const (
	SettingRcloneBin  = "rclone_bin"
	SettingFSWatchBin = "fswatch_bin"
	SettingRcloneCfg  = "rclone_config"
)

// SettingWorkerSocketPath is the persisted override for the worker live-status
// Unix socket path. The user-facing configuration name is `socket-path`; the
// internal key intentionally differs from the CLI spelling (§4.1).
const SettingWorkerSocketPath = "worker_socket_path"

// GetSetting returns a setting value, or "" if absent.
func (d *DB) GetSetting(key string) (string, error) {
	var v string
	err := d.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

// SetSetting upserts a setting value.
func (d *DB) SetSetting(key, value string) error {
	_, err := d.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// UnsetSetting removes a setting row.
func (d *DB) UnsetSetting(key string) error {
	_, err := d.Exec(`DELETE FROM settings WHERE key = ?`, key)
	return err
}
