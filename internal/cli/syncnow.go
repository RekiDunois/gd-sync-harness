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
		Short: "Drain pending events and fast-upsert changed files (local manifest barrier)",
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
			return app.withProfileLock(p, func() error {
				return runSyncNow(app, p)
			})
		},
	}
}

func runSyncNow(app *App, p *state.Profile) error {
	ctx, cancel := app.Context()
	defer cancel()

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
	var files []string
	for c := range changedSet {
		files = append(files, c)
	}
	if len(files) == 0 {
		fmt.Printf("sync-now %s: up to date\n", p.ID)
		return nil
	}

	if err := app.upsertForProfile(ctx, p, files); err != nil {
		_ = app.DB.SetLastError(p.ID, err.Error())
		return err
	}
	if err := updateManifest(app.DB, p, scan, files); err != nil {
		return err
	}
	if err := app.DB.ClearPendingPaths(p.ID, files); err != nil {
		return err
	}
	if err := app.DB.MarkFastSuccess(p.ID); err != nil {
		return err
	}
	fmt.Printf("sync-now %s: uploaded %d files\n", p.ID, len(files))
	return nil
}

func updateManifest(db *state.DB, p *state.Profile, scan *sync.ScanResult, files []string) error {
	byPath := map[string]sync.ScanEntry{}
	for _, e := range scan.Entries {
		byPath[e.RelPath] = e
	}
	for _, f := range files {
		if e, ok := byPath[f]; ok {
			if err := db.ManifestUpsert(state.ManifestEntry{
				ProfileID: p.ID, RelPath: e.RelPath, Size: e.Size, ModTime: e.ModTime,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
