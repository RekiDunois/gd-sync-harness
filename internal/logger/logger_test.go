package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLoggerWritesToPrivatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "worker.log")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Print("synthetic message")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "synthetic message") {
		t.Fatalf("log contents = %q", b)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %o, want 600", info.Mode().Perm())
	}
}

func TestNewLoggerWithoutPathUsesStderr(t *testing.T) {
	if l, err := New(""); err != nil || l == nil {
		t.Fatalf("New empty path = logger %v, err %v", l, err)
	}
}

func TestSanitizePathRemovesSeparators(t *testing.T) {
	if got := SanitizePath("a/b/c"); got != "a_b_c" {
		t.Fatalf("SanitizePath = %q", got)
	}
	if got := SanitizePath("plain"); got != "plain" {
		t.Fatalf("SanitizePath changed plain value to %q", got)
	}
}

func TestNewLoggerReportsDirectoryCreationFailure(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(filepath.Join(blocked, "worker.log")); err == nil {
		t.Fatal("New unexpectedly succeeded below a regular file")
	}
}
