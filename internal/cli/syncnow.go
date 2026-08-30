package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

func newSyncNowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync-now <profile>",
		Short: "Drain pending events and fast-upsert changed files (worker-owned)",
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
			return runSyncNow(app, p)
		},
	}
}

// runSyncNow compares the local source against the durable manifest and records
// durable events, then wakes the worker which owns fast-upsert execution
// (§13.1, §13.2). Deletes upgrade to a full reconciliation intent. It never
// runs rclone in the CLI process.
func runSyncNow(app *App, p *state.Profile) error {
	scan, err := sync.ScanLocal(p)
	if err != nil {
		return err
	}

	manifest, err := app.DB.ManifestAll(p.ID)
	if err != nil {
		return err
	}
	changed, deletes, _ := sync.DiffLocalManifest(scan, manifest)

	destructivePending, err := app.DB.HasDestructivePending(p.ID)
	if err != nil {
		return err
	}
	if len(deletes) > 0 || destructivePending {
		fmt.Printf("sync-now %s: %d local deletes detected; upgrading to full reconciliation\n", p.ID, len(deletes))
		// Submit durable intent and let the worker own execution.
		return runReconcile(app, p, sync.SyncOptions{}, false)
	}

	pending, err := app.DB.ListPending(p.ID)
	if err != nil {
		return err
	}
	changedSet := map[string]bool{}
	for _, c := range changed {
		changedSet[c] = true
	}
	for _, e := range pending {
		if !changedSet[e.Path] {
			if _, err := os.Stat(joinSource(p.SourcePath, e.Path)); err == nil {
				changedSet[e.Path] = true
			}
		}
	}
	recorded := 0
	for c := range changedSet {
		if _, err := app.DB.RecordEvent(p.ID, c, state.EventModify, false); err != nil {
			return err
		}
		recorded++
	}
	if recorded == 0 {
		fmt.Printf("sync-now %s: up to date\n", p.ID)
		return nil
	}
	if ss, _ := app.DB.GetSyncState(p.ID); ss != nil && ss.CurrentRunID != nil {
		return fmt.Errorf("profile %q has an active full reconciliation; fast sync deferred to the worker", p.ID)
	}
	wakeWorker(app, p.ID)
	fmt.Printf("sync-now %s: queued %d file change(s) for the worker\n", p.ID, recorded)
	return nil
}
