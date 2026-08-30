package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"knowledge-sync/internal/launchd"
	"knowledge-sync/internal/paths"
	"knowledge-sync/internal/sidecar"
	"knowledge-sync/internal/state"
)

const (
	launchLabelPrefix = "com.local.knowledge-sync"
	reconcileHour     = 13
	reconcileMin      = 37
)

// workerJobLabel is the label of the single global reconciliation worker job.
// The worker is one process that serves all profiles (§9.5); per-profile
// launchd jobs (watch/reconcile) complement it but never duplicate execution
// (atomic claims prevent concurrency).
const workerJobLabel = launchLabelPrefix + ".worker"

func jobConfigs(app *App, p *state.Profile) []launchd.Config {
	return []launchd.Config{
		{
			LabelPrefix: launchLabelPrefix,
			ProfileID:   p.ID,
			Kind:        launchd.JobWatch,
			Binary:      mustSelfPath(),
			LogDir:      app.LogDir,
		},
		{
			LabelPrefix:   launchLabelPrefix,
			ProfileID:     p.ID,
			Kind:          launchd.JobReconcile,
			Binary:        mustSelfPath(),
			LogDir:        app.LogDir,
			ReconcileHour: reconcileHour,
			ReconcileMin:  reconcileMin,
		},
	}
}

// workerConfig returns the global worker launchd config (no profile id).
func workerConfig(app *App) launchd.Config {
	return launchd.Config{
		LabelPrefix: launchLabelPrefix,
		ProfileID:   "",
		Kind:        launchd.JobWorker,
		Binary:      mustSelfPath(),
		LogDir:      app.LogDir,
	}
}

func mustSelfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "/usr/local/bin/knowledge-sync"
	}
	return exe
}

func installJobs(app *App, p *state.Profile) error {
	agentsDir, _ := paths.LaunchAgentsDir()
	for _, cfg := range jobConfigs(app, p) {
		if err := cfg.Reload(agentsDir); err != nil {
			return fmt.Errorf("load %s: %w", cfg.Label(), err)
		}
	}
	return nil
}

func uninstallJobs(app *App, profileID string) error {
	agentsDir, _ := paths.LaunchAgentsDir()
	cfgs := []launchd.Config{
		{LabelPrefix: launchLabelPrefix, ProfileID: profileID, Kind: launchd.JobWatch},
		{LabelPrefix: launchLabelPrefix, ProfileID: profileID, Kind: launchd.JobReconcile},
	}
	var firstErr error
	for _, cfg := range cfgs {
		if err := cfg.Uninstall(agentsDir); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	_ = app
	return firstErr
}

func stopJobs(app *App, profileID string) error {
	cfgs := []launchd.Config{
		{LabelPrefix: launchLabelPrefix, ProfileID: profileID, Kind: launchd.JobWatch},
		{LabelPrefix: launchLabelPrefix, ProfileID: profileID, Kind: launchd.JobReconcile},
	}
	var firstErr error
	for _, cfg := range cfgs {
		if err := cfg.Unload(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	_ = app
	return firstErr
}

// installWorkerJob installs and loads the global worker job.
func installWorkerJob(app *App) error {
	agentsDir, _ := paths.LaunchAgentsDir()
	cfg := workerConfig(app)
	if err := cfg.Reload(agentsDir); err != nil {
		return fmt.Errorf("load %s: %w", cfg.Label(), err)
	}
	return nil
}

// uninstallWorkerJob removes the global worker job.
func uninstallWorkerJob(app *App) error {
	agentsDir, _ := paths.LaunchAgentsDir()
	cfg := workerConfig(app)
	return cfg.Uninstall(agentsDir)
}

func startJobs(app *App, p *state.Profile) error {
	return installJobs(app, p)
}

func writeSidecarCtx(ctx context.Context, app *App, p *state.Profile) error {
	return sidecar.Write(ctx, app.Rclone, p.RemoteName, sidecar.Create(p))
}

func validateOwnership(ctx context.Context, app *App, p *state.Profile) error {
	remotePath := sidecar.SidecarPath(p.ProfileUUID)
	return sidecar.Validate(ctx, app.Rclone, p, remotePath)
}

func backupNow(app *App) {
	backupsDir, _ := paths.BackupsDir()
	if _, _, err := app.DB.BackupIfMtimeOld(backupsDir, 10*time.Minute); err != nil {
		_ = app.DB.SetLastError("", fmt.Sprintf("backup: %v", err))
	}
}
