package launchd

import (
	"strings"
	"testing"
)

func TestWatchPlistRender(t *testing.T) {
	c := Config{
		LabelPrefix: "com.local.knowledge-sync",
		ProfileID:   "obsidian-main",
		Kind:        JobWatch,
		Binary:      "/usr/local/bin/knowledge-sync",
		LogDir:      "/Users/x/Library/Logs/knowledge-sync",
	}
	if c.Label() != "com.local.knowledge-sync.obsidian-main.watch" {
		t.Fatalf("label = %s", c.Label())
	}
	if c.PlistFilename() != "com.local.knowledge-sync.obsidian-main.watch.plist" {
		t.Fatalf("filename = %s", c.PlistFilename())
	}
	b, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"<key>Label</key>",
		"com.local.knowledge-sync.obsidian-main.watch",
		"/usr/local/bin/knowledge-sync",
		"watch",
		"obsidian-main",
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
		ProfileID:     "obsidian-main",
		Kind:          JobReconcile,
		Binary:        "/usr/local/bin/knowledge-sync",
		LogDir:        "/Users/x/Library/Logs/knowledge-sync",
		ReconcileHour: 13,
		ReconcileMin:  37,
	}
	b, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
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
		LogDir:      "/Users/x/Library/Logs/knowledge-sync",
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
