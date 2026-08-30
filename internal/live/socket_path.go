package live

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultRuntimeDirName is the subdirectory of the system temp dir where the
// default worker socket lives (§4.3).
const DefaultRuntimeDirName = "knowledge-sync"

// DefaultSocketPath is the fallback worker socket path when no explicit
// socket-path setting is configured (§4.2).
func DefaultSocketPath() string {
	return filepath.Join(os.TempDir(), DefaultRuntimeDirName, "worker.sock")
}

// ResolveSocketPath implements the single shared resolution order (§4.2):
//
//  1. the persisted socket-path setting, if non-empty;
//  2. <tempdir>/knowledge-sync/worker.sock.
//
// Worker and clients must both use this resolver so they never diverge.
func ResolveSocketPath(configured string) string {
	if configured != "" {
		return configured
	}
	return DefaultSocketPath()
}

// PrepareSocketDir ensures the parent directory of the resolved socket path
// exists with user-only permissions (§4.3).
//
// For the default runtime directory the parent is created with 0700. For an
// explicitly configured path the parent must already exist (safer rule: never
// recursively chmod arbitrary user-provided directories); an absent or
// unusable parent is an actionable error.
func PrepareSocketDir(path string, isDefault bool) error {
	dir := filepath.Dir(path)
	if isDefault {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create socket runtime directory %s: %w", dir, err)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("inspect socket runtime directory %s: %w", dir, err)
		}
		if !fi.IsDir() {
			return fmt.Errorf("socket runtime path %s exists and is not a directory", dir)
		}
		_ = os.Chmod(dir, 0o700)
		return nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("configured socket parent %s must already exist: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("configured socket parent %s is not a directory", dir)
	}
	return nil
}

// StaleSocketCheck classifies what currently exists at the socket path so the
// worker can decide whether removal is safe (§4.4).
type StaleSocketCheck int

const (
	StaleSocketMissing StaleSocketCheck = iota
	StaleSocketUnixSocket
	StaleSocketUnsafe
)

// ClassifySocketPath inspects the existing path.
func ClassifySocketPath(path string) StaleSocketCheck {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return StaleSocketMissing
		}
		return StaleSocketUnsafe
	}
	if fi.Mode()&os.ModeSocket != 0 {
		return StaleSocketUnixSocket
	}
	return StaleSocketUnsafe
}
