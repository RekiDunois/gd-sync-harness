package state

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const maxBackups = 24 // rolling window

// Backup performs a consistent SQLite backup via VACUUM INTO. Keeps maxBackups newest.
func (d *DB) Backup(backupsDir string) (string, error) {
	if err := os.MkdirAll(backupsDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("knowledge-sync-%s.sqlite.bak", time.Now().Format("20060102-150405"))
	dst := filepath.Join(backupsDir, name)
	if _, err := d.Exec(fmt.Sprintf("VACUUM INTO %q", dst)); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	if err := pruneOld(backupsDir); err != nil {
		return "", err
	}
	return dst, nil
}

// BackupIfMtimeOld performs a backup only if the last backup is older than maxAge.
func (d *DB) BackupIfMtimeOld(backupsDir string, maxAge time.Duration) (bool, string, error) {
	latest, err := newestBackup(backupsDir)
	if err != nil {
		return false, "", err
	}
	if latest != "" {
		fi, err := os.Stat(latest)
		if err == nil && time.Since(fi.ModTime()) < maxAge {
			return false, "", nil
		}
	}
	p, err := d.Backup(backupsDir)
	return true, p, err
}

func newestBackup(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".bak" {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)
	return filepath.Join(dir, names[len(names)-1]), nil
}

func pruneOld(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var baks []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".bak" {
			baks = append(baks, e.Name())
		}
	}
	if len(baks) <= maxBackups {
		return nil
	}
	sort.Strings(baks)
	for _, name := range baks[:len(baks)-maxBackups] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
