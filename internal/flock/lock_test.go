package flock

import (
	"os"
	"testing"
)

func TestLockAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir, "profile-x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, "profile-x"); err == nil {
		t.Fatal("expected ErrLocked for live owner")
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := Acquire(dir, "profile-x")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLockStaleRecovery(t *testing.T) {
	dir := t.TempDir()
	path := LockPath(dir, "profile-x")
	if err := os.WriteFile(path, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(dir, "profile-x")
	if err != nil {
		t.Fatalf("stale lock should be recovered: %v", err)
	}
	if !l.recovered {
		t.Error("should mark recovered")
	}
	_ = l.Release()
}
