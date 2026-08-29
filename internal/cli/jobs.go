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
		if _, err := cfg.Install(agentsDir); err != nil {
			return err
		}
		if err := cfg.Load(agentsDir); err != nil {
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
