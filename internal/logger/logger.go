package logger

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// New returns a logger writing to the given path. If path is empty, it writes
// to stderr. The log directory is created if needed.
func New(path string) (*log.Logger, error) {
	if path == "" {
		return log.New(os.Stderr, "", log.LstdFlags), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return log.New(f, "", log.LstdFlags), nil
}

// SanitizePath removes any path separators that could break log filenames.
func SanitizePath(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, string(filepath.Separator), "_")
	return s
}
