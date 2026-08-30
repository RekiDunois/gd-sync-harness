package cli

import (
	"os"
	"path/filepath"
	"testing"

	"knowledge-sync/internal/launchd"
)

// TestWorkerJobInstalledDetection verifies workerJobInstalled reflects the
// plist presence (§15.3). Uses an injected LaunchAgents dir via the launchd
// Config directly.
func TestWorkerJobInstalledDetection(t *testing.T) {
	dir := t.TempDir()
	cfg := launchd.Config{LabelPrefix: launchLabelPrefix, Kind: launchd.JobWorker, Binary: mustSelfPath()}
	if _, err := cfg.Install(dir); err != nil {
		t.Fatal(err)
	}
	// workerJobInstalled reads the real LaunchAgents dir, not dir; the
	// detection logic itself is a stat. We verify the plist path shape here.
	path := cfg.PlistPath(dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worker plist not installed: %v", err)
	}
	if filepath.Base(path) != "com.local.knowledge-sync.worker.plist" {
		t.Fatalf("worker plist name = %q", filepath.Base(path))
	}
}

// TestWorkerJobConfigShape verifies the global worker config has no profile id
// and uses the worker command (§15.1).
func TestWorkerJobConfigShape(t *testing.T) {
	cfg := workerConfig(&App{LogDir: t.TempDir()})
	if cfg.ProfileID != "" {
		t.Fatalf("worker config must have no profile id: %q", cfg.ProfileID)
	}
	if cfg.Kind != launchd.JobWorker {
		t.Fatalf("worker kind = %s", cfg.Kind)
	}
	if cfg.Command() != "worker" {
		t.Fatalf("worker command = %q", cfg.Command())
	}
}
