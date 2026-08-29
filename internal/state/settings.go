package state

import "database/sql"

// Settings keys for persisted tool paths (§31.2).
const (
	SettingRcloneBin  = "rclone_bin"
	SettingFSWatchBin = "fswatch_bin"
	SettingRcloneCfg  = "rclone_config"
)

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
