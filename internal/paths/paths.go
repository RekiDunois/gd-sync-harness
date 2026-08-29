package paths

import (
	"os"
	"path/filepath"
)

const dataDirName = "knowledge-sync"

// StateDir returns ~/.local/share/knowledge-sync
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", dataDirName), nil
}

// DBPath returns the canonical SQLite database path.
func DBPath() (string, error) {
	d, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "knowledge-sync.sqlite"), nil
}

// BackupsDir returns ~/.local/share/knowledge-sync/backups
func BackupsDir() (string, error) {
	d, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "backups"), nil
}

// LogsDir returns ~/Library/Logs/knowledge-sync
func LogsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", dataDirName), nil
}

// LaunchAgentsDir returns ~/Library/LaunchAgents
func LaunchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// Ensure creates all harness-owned directories.
func Ensure() error {
	for _, d := range []struct {
		fn func() (string, error)
	}{
		{StateDir},
		{BackupsDir},
		{LogsDir},
		{LaunchAgentsDir},
	} {
		p, err := d.fn()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
	}
	return nil
}
