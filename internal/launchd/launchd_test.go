package launchd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchPlistRender(t *testing.T) {
	c := Config{
		LabelPrefix: "com.local.knowledge-sync",
		ProfileID:   "example-profile",
		Kind:        JobWatch,
		Binary:      "/usr/local/bin/knowledge-sync",
		LogDir:      filepath.Join(t.TempDir(), "logs"),
	}
	if c.Label() != "com.local.knowledge-sync.example-profile.watch" {
		t.Fatalf("label = %s", c.Label())
	}
	if c.PlistFilename() != "com.local.knowledge-sync.example-profile.watch.plist" {
		t.Fatalf("filename = %s", c.PlistFilename())
	}
	b, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"<key>Label</key>",
		"com.local.knowledge-sync.example-profile.watch",
		"/usr/local/bin/knowledge-sync",
		"watch",
		"example-profile",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing %q\n%s", want, s)
		}
	}
	if strings.Contains(s, "StartCalendarInterval") {
		t.Error("watch plist should not have StartCalendarInterval")
	}
}

func TestReconcilePlistRender(t *testing.T) {
	c := Config{
		LabelPrefix:   "com.local.knowledge-sync",
		ProfileID:     "example-profile",
		Kind:          JobReconcile,
		Binary:        "/usr/local/bin/knowledge-sync",
		LogDir:        filepath.Join(t.TempDir(), "logs"),
		ReconcileHour: 13,
		ReconcileMin:  37,
	}
	b, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if c.Command() != "reconcile-scheduled" {
		t.Fatalf("reconcile command = %q", c.Command())
	}
	if !strings.Contains(s, "<string>reconcile-scheduled</string>") {
		t.Fatal("reconcile plist must invoke reconcile-scheduled")
	}
	for _, want := range []string{
		"<key>RunAtLoad</key>",
		"<false/>",
		"<key>StartCalendarInterval</key>",
		"<integer>37</integer>",
		"<integer>13</integer>",
		"reconcile",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing %q\n%s", want, s)
		}
	}
}

func TestWorkerLabelAndPlistRender(t *testing.T) {
	c := Config{
		LabelPrefix: "com.local.knowledge-sync",
		ProfileID:   "",
		Kind:        JobWorker,
		Binary:      "/usr/local/bin/knowledge-sync",
		LogDir:      filepath.Join(t.TempDir(), "logs"),
	}
	if c.Label() != "com.local.knowledge-sync.worker" {
		t.Fatalf("worker label = %q", c.Label())
	}
	b, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"com.local.knowledge-sync.worker",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"worker",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("worker plist missing %q\n%s", want, s)
		}
	}
	if strings.Contains(s, "StartCalendarInterval") {
		t.Error("worker plist should not have StartCalendarInterval")
	}
}

func TestReloadBootoutWriteBootstrapOrder(t *testing.T) {
	var calls []string
	old := launchctlRun
	launchctlRun = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	t.Cleanup(func() { launchctlRun = old })

	dir := t.TempDir()
	c := Config{LabelPrefix: "example.knowledge-sync", ProfileID: "demo", Kind: JobReconcile,
		Binary: "/opt/example/knowledge-sync", LogDir: filepath.Join(dir, "logs"), ReconcileHour: 1, ReconcileMin: 7}
	if err := c.Reload(dir); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "bootout ") || !strings.HasPrefix(calls[1], "bootstrap ") {
		t.Fatalf("launchctl calls = %v", calls)
	}
	if _, err := os.Stat(c.PlistPath(dir)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRetriesTransientBootstrapError(t *testing.T) {
	var calls int
	old := launchctlRun
	launchctlRun = func(args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		return nil, nil
	}
	t.Cleanup(func() { launchctlRun = old })

	dir := t.TempDir()
	c := Config{LabelPrefix: "example.knowledge-sync", ProfileID: "demo", Kind: JobWorker,
		Binary: "/opt/example/knowledge-sync", LogDir: filepath.Join(dir, "logs")}
	if _, err := c.Install(dir); err != nil {
		t.Fatal(err)
	}
	if err := c.Load(dir); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("launchctl calls = %d, want 2", calls)
	}
}
