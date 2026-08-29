package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

func newReconcileScheduledCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile-scheduled <profile>",
		Short: "Run the hourly safety reconciliation for a profile (launchd entrypoint)",
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
				return runReconcile(app, p, sync.SyncOptions{}, true)
			})
		},
	}
}

func newReconcileNowCmd() *cobra.Command {
	var allowDeletes int
	c := &cobra.Command{
		Use:   "reconcile-now <profile>",
		Short: "Run full authoritative reconciliation with preflight + delete budget",
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
				return runReconcile(app, p, sync.SyncOptions{AllowDeletes: allowDeletes}, false)
			})
		},
	}
	c.Flags().IntVar(&allowDeletes, "allow-deletes", 0, "one-shot delete budget override (§16)")
	return c
}

func runReconcile(app *App, p *state.Profile, options sync.SyncOptions, scheduled bool) error {
	ctx, cancel := app.Context()
	defer cancel()

	if err := validateOwnership(ctx, app, p); err != nil {
		_ = app.DB.SetLastError(p.ID, err.Error())
		return err
	}

	pre, err := app.reconcileForProfile(ctx, p, options)
	if err != nil {
		_ = app.DB.SetLastError(p.ID, err.Error())
		if err == sync.ErrDeleteBudgetExceeded {
			return fmt.Errorf("preflight: %d expected deletions exceed budget %d; use --allow-deletes <N> to override (§16)",
				pre.ToDelete, effectiveMaxDelete(p, options))
		}
		return err
	}

	if err := refreshManifest(app.DB, p, pre); err != nil {
		return err
	}
	if err := app.DB.MarkReconcileSuccess(p.ID); err != nil {
		return err
	}
	if err := app.DB.ClearPending(p.ID); err != nil {
		return err
	}
	if !scheduled {
		fmt.Printf("reconcile %s: %d source files, %d copies, %d deletions\n",
			p.ID, pre.SourceFiles, pre.ToCopy, pre.ToDelete)
	}
	return nil
}

func effectiveMaxDelete(p *state.Profile, o sync.SyncOptions) int {
	if o.AllowDeletes > 0 {
		return o.AllowDeletes
	}
	return p.MaxDelete
}

func refreshManifest(db *state.DB, p *state.Profile, pre *sync.PreflightResult) error {
	scan, err := sync.ScanLocal(p)
	if err != nil {
		return err
	}
	entries := make([]state.ManifestEntry, 0, len(scan.Entries))
	for _, e := range scan.Entries {
		entries = append(entries, state.ManifestEntry{
			ProfileID: p.ID, RelPath: e.RelPath, Size: e.Size, ModTime: e.ModTime,
		})
	}
	return db.ManifestReplaceAll(p.ID, entries)
}
