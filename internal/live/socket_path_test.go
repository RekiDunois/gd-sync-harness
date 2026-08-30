package live

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func listenUnix(t *testing.T, path string) (net.Listener, error) {
	t.Helper()
	return net.Listen("unix", path)
}

func TestResolveSocketPathDefault(t *testing.T) {
	// The default resolver does not depend on a controllable temp dir, so we
	// assert the shape rather than the exact absolute path.
	p := ResolveSocketPath("")
	if p == "" {
		t.Fatal("default socket path must be non-empty")
	}
	if filepath.Base(p) != "worker.sock" {
		t.Fatalf("default socket basename = %q, want worker.sock", filepath.Base(p))
	}
}

func TestResolveSocketPathConfiguredOverrides(t *testing.T) {
	got := ResolveSocketPath("/custom/override.sock")
	if got != "/custom/override.sock" {
		t.Fatalf("configured path must override default, got %q", got)
	}
}

func TestResolveSocketPathUnsetReturnsDefault(t *testing.T) {
	dflt := DefaultSocketPath()
	if ResolveSocketPath("") != dflt {
		t.Fatalf("empty override must resolve to default %q", dflt)
	}
}

func TestPrepareDefaultRuntimeDirPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket permissions not meaningful on windows")
	}
	base := t.TempDir()
	path := filepath.Join(base, "knowledge-sync", "worker.sock")
	if err := PrepareSocketDir(path, true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("runtime dir perms = %o, want user-only", perm)
	}
}

func TestPrepareConfiguredParentMustExist(t *testing.T) {
	base := t.TempDir()
	// A configured path whose parent does not exist is an error.
	if err := PrepareSocketDir(filepath.Join(base, "missing", "worker.sock"), false); err == nil {
		t.Fatal("configured socket with missing parent must fail")
	}
	// An existing directory parent is accepted.
	parent := filepath.Join(base, "parent")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSocketDir(filepath.Join(parent, "worker.sock"), false); err != nil {
		t.Fatalf("configured socket with existing parent must succeed: %v", err)
	}
}

func TestClassifySocketPathStates(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing.sock")
	if ClassifySocketPath(missing) != StaleSocketMissing {
		t.Fatal("missing path must classify as missing")
	}

	sock := filepath.Join(base, "real.sock")
	l, err := listenUnix(t, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if ClassifySocketPath(sock) != StaleSocketUnixSocket {
		t.Fatal("existing unix socket must classify as socket")
	}
	l.Close()

	regular := filepath.Join(base, "regular.txt")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ClassifySocketPath(regular) != StaleSocketUnsafe {
		t.Fatal("regular file must classify as unsafe")
	}

	symlink := filepath.Join(base, "link.sock")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	if ClassifySocketPath(symlink) != StaleSocketUnsafe {
		t.Fatal("symlink must classify as unsafe (never blindly removed)")
	}
}
