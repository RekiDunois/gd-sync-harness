package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const dataDirName = "knowledge-sync"

func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", dataDirName), nil
}

func AppConfigPath() (string, error) {
	d, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// StateDir returns ~/.local/share/knowledge-sync
func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", dataDirName), nil
}

// CompilerRoot returns the app-owned local compiler state for a profile UUID.
func CompilerRoot(profileUUID string) (string, error) {
	if profileUUID == "" || profileUUID == "." || profileUUID == ".." || strings.ContainsAny(profileUUID, `/\\`) {
		return "", fmt.Errorf("invalid compiler profile uuid")
	}
	d, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "compiler", profileUUID), nil
}

// CompilerGenerationsDir returns the immutable generation directory for a
// profile UUID.
func CompilerGenerationsDir(profileUUID string) (string, error) {
	d, err := CompilerRoot(profileUUID)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "generations"), nil
}

// CompilerStagingDir returns the temporary publication directory for a profile.
func CompilerStagingDir(profileUUID string) (string, error) {
	d, err := CompilerRoot(profileUUID)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "staging"), nil
}

// CompilerLockName returns the stable lock name used for local compiler state.
func CompilerLockName(profileUUID string) string {
	return "compiler-" + profileUUID
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
		{ConfigDir},
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
