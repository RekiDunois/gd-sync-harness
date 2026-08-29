package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/filter"
	"knowledge-sync/internal/logger"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
	"knowledge-sync/internal/watch"
)

// reconcileDestructiveDebounce is the §14 settle delay before a destructive
// event triggers full reconciliation (RECONCILE_SETTLE_SECONDS).
const reconcileDestructiveDebounce = 10 * time.Second

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
			var eligible []string
			for _, rel := range changed {
				abs := joinSource(p.SourcePath, rel)
				if _, err := os.Stat(abs); err != nil {
					continue
				}
				eligible = append(eligible, rel)
			}
			if len(eligible) == 0 {
				return nil
			}
			if err := app.upsertForProfile(ctx, p, eligible); err != nil {
				lg.Printf("fast upsert failed: %v", err)
				return err
			}
			_ = app.DB.ClearPendingPaths(p.ID, eligible)
			_ = app.DB.MarkFastSuccess(p.ID)
			return nil
		},
		OnReconcile: func(ctx context.Context) error {
			// §14: destructive events get a dedicated debounce then a full
			// reconciliation. Reconciliation is serialized by the per-profile
			// lock and routed through the atomic claim path; run it
			// asynchronously so the watcher's read/debounce loops are not
			// blocked. The hourly job remains the safety net.
			lg.Printf("destructive event: scheduling full reconciliation")
			go func() {
				time.Sleep(reconcileDestructiveDebounce)
				if err := runReconcile(app, p, sync.SyncOptions{}, true); err != nil {
					lg.Printf("destructive reconcile: %v", err)
				}
			}()
			return nil
		},
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

func joinSource(source, rel string) string {
	return source + "/" + rel
}
