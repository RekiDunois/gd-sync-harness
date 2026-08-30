package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/logger"
	"knowledge-sync/internal/policy"
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
		// No stale startup filter is set: the watcher is a fact producer and
		// must never drop events because they match a policy older than the
		// currently committed hash (§1.3, §12.2). The worker applies the
		// committed matcher to durable events.
		// The watcher is an event/intent producer only (§13.1): it records
		// durable events and wakes the worker, which owns fast-upsert
		// execution. The debounce deadline is evaluated by the worker from
		// durable first_seen/last_seen timestamps.
		OnBatch: func(ctx context.Context, changed []string) error {
			for _, rel := range changed {
				if _, err := os.Stat(joinSource(p.SourcePath, rel)); err == nil {
					if _, err := app.DB.RecordEvent(p.ID, rel, state.EventModify, false); err != nil {
						return err
					}
				}
			}
			wakeWorker(app, p.ID)
			return nil
		},
		OnReconcile: func(ctx context.Context) error {
			rt, err := app.DB.GetRuntime(p.ID)
			if err != nil {
				return err
			}
			lg.Printf("destructive event: durable full reconciliation scheduled")
			err = app.DB.ScheduleDestructiveReconcile(p.ID, rt.SourceGeneration, state.Now())
			wakeWorker(app, p.ID)
			return err
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

// runWatcherCatchUp compares the current local source against the last durable
// manifest after fswatch is live, recovering changes made while the watcher
// was stopped without materializing a large initial pending queue.
func runWatcherCatchUp(ctx context.Context, app *App, p *state.Profile) error {
	snap, err := app.DB.GetCommittedSnapshot(p.ID)
	if err != nil {
		return err
	}
	if snap == nil {
		snap = &policy.Snapshot{}
	}
	active, err := sync.ScanActivePaths(p.SourcePath, p.MaxFileSize, snap)
	if err != nil {
		return err
	}
	scan := &sync.ScanResult{}
	for _, rel := range active {
		fi, err := os.Stat(joinSource(p.SourcePath, rel))
		if err != nil {
			continue
		}
		scan.Entries = append(scan.Entries, sync.ScanEntry{RelPath: rel, Size: fi.Size(), ModTime: fi.ModTime().Unix()})
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
