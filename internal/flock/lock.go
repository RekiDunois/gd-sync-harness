package flock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrLocked is returned when the lock is held by a live process.
var ErrLocked = errors.New("lock held by another process")

// Lock is a PID-aware per-profile mutex (§20.1). Uses O_EXCL file creation so a
// stale lock (owner PID no longer exists) is safely recoverable.
type Lock struct {
	dir       string
	name      string
	path      string
	held      bool
	recovered bool
}

// LockPath returns the lock file path for a profile.
func LockPath(dir, profileID string) string {
	return filepath.Join(dir, "profile-"+profileID+".lock")
}

// Acquire takes the lock. If the previous owner's PID is dead, it recovers the
// lock. Returns ErrLocked if a live process holds it.
func Acquire(dir, profileID string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Lock{dir: dir, name: profileID, path: LockPath(dir, profileID)}

	for attempt := 0; attempt < 3; attempt++ {
		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			l.held = true
			return l, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if l.ownerAlive() {
			return nil, fmt.Errorf("%w: %s", ErrLocked, l.path)
		}
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		l.recovered = true
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("could not acquire lock %s", l.path)
}

// ownerAlive reads the PID in the lock file and checks the process exists.
func (l *Lock) ownerAlive() bool {
	b, err := os.ReadFile(l.path)
	if err != nil {
		return false
	}
	pidStr := strings.TrimSpace(string(b))
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	return processAlive(pid)
}

// processAlive checks whether a PID exists via signal 0.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// Release removes the lock file if we hold it.
func (l *Lock) Release() error {
	if !l.held {
		return nil
	}
	l.held = false
	return os.Remove(l.path)
}
