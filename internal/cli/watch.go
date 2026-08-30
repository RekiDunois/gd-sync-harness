package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/filter"
	"knowledge-sync/internal/logger"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
	"knowledge-sync/internal/watch"
)

func newWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch <profile>",
		Short: "Run the file-watcher service for a profile (launchd entrypoint)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			p, err := app.requireProfile(args[0])
			if err != nil {
				return err
			}
			if !p.Enabled {
				return fmt.Errorf("profile %q is disabled", p.ID)
			}
			if app.FSWatchBin == "" {
				return fmt.Errorf("fswatch not found (run 'knowledge-sync doctor'; brew install fswatch)")
			}
			return runWatcher(app, p)
		},
	}
}

func runWatcher(app *App, p *state.Profile) error {
	logPath := app.logPathFor(p.ID, "watch")
	lg, err := logger.New(logPath)
	if err != nil {
		return err
	}
	_ = app.DB.SetWatcherStatus(p.ID, state.WatcherRunning)

	w := &watch.Watcher{
		ProfileID:  p.ID,
		SourcePath: p.SourcePath,
		FSWatchBin: app.FSWatchBin,
		DB:         app.DB,
		Log:        lg,
		Settings:   watch.DefaultFastSettings(),
		Filter:     filter.FromProfile(p),
		OnBatch: func(ctx context.Context, changed []string) error {
			return app.withProfileLock(p, func() error {
				ss, _ := app.DB.GetSyncState(p.ID)
				if ss != nil && ss.HasDebt() {
					return nil
				}
				var eligible []string
				for _, rel := range changed {
					if _, err := os.Stat(joinSource(p.SourcePath, rel)); err == nil {
						eligible = append(eligible, rel)
					}
				}
				if len(eligible) == 0 {
					return nil
				}
				if err := app.upsertForProfile(ctx, p, eligible); err != nil {
					lg.Printf("fast upsert failed: %v", err)
					return err
				}
				return app.DB.MarkFastSuccess(p.ID)
			})
		},
		OnReconcile: func(ctx context.Context) error {
			rt, err := app.DB.GetRuntime(p.ID)
			if err != nil {
				return err
			}
			lg.Printf("destructive event: durable full reconciliation scheduled")
			return app.DB.ScheduleDestructiveReconcile(p.ID, rt.SourceGeneration, state.Now())
		},
		OnCatchUp: func(ctx context.Context) error { return runWatcherCatchUp(ctx, app, p) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := w.Start(ctx); err != nil {
		_ = app.DB.SetWatcherStatus(p.ID, state.WatcherError)
		return err
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig

	lg.Printf("stopping watcher for %s", p.ID)
	if err := w.Stop(); err != nil {
		lg.Printf("stop error: %v", err)
	}
	_ = app.DB.SetWatcherStatus(p.ID, state.WatcherStopped)
	return nil
}

// runWatcherCatchUp compares the current local manifest with the last durable
// manifest after fswatch is live, recovering changes made while the watcher
// was stopped without materializing a large initial pending queue.
func runWatcherCatchUp(ctx context.Context, app *App, p *state.Profile) error {
	scan, err := sync.ScanLocal(p)
	if err != nil {
		return err
	}
	manifest, err := app.DB.ManifestAll(p.ID)
	if err != nil {
		return err
	}
	changed, deletes, _ := sync.DiffLocalManifest(scan, manifest)
	if len(deletes) > 0 {
		for _, rel := range deletes {
			if _, err := app.DB.RecordEvent(p.ID, rel, state.EventDelete, true); err != nil {
				return err
			}
		}
		return nil
	}
	for _, rel := range changed {
		if _, err := app.DB.RecordEvent(p.ID, rel, state.EventModify, false); err != nil {
			return err
		}
	}
	if len(changed) > 500 || (len(manifest) > 0 && len(changed)*100 > len(manifest)*5) {
		rt, err := app.DB.GetRuntime(p.ID)
		if err != nil {
			return err
		}
		return app.DB.PromoteToFullReconcile(p.ID, rt.SourceGeneration, state.Now())
	}
	_ = ctx
	return nil
}

func joinSource(source, rel string) string {
	return source + "/" + rel
}
