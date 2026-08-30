package cli

import (
	"fmt"
	"os"

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
	// CLI entrypoints (reconcile-now, reconcile-scheduled, sync, migrate) and
	// watch-triggered reconciles route through the same worker-owned claim and
	// execution path. This never runs a competing transfer inside the CLI: it
	// either claims the durable attempt or observes an existing one.
	//
	// force=true keeps the pre-existing safety-net semantics: the hourly
	// scheduled reconcile and explicit reconcile-now run a full reconciliation
	// even with no known debt (to catch remote drift), while the event-driven
	// worker pass remains strictly debt-driven.
	// Scheduled reconciliation must respect the durable destructive debounce;
	// explicit reconcile-now is allowed to bypass it. Both retain the safety-net
	// behavior of running even when no generation debt is known.
	ctx, cancel := app.Context()
	defer cancel()
	lease := leaseID()
	if err := app.DB.AcquireRemoteLease(ctx, p.RemoteName, 1, 2, os.Getpid(), lease); err != nil {
		return err
	}
	stopRenewal := startLeaseRenewal(ctx, app.DB, lease)
	defer stopRenewal()
	defer app.DB.ReleaseRemoteLease(lease)
	force := true
	run, res, err := app.DB.ClaimRunWithOptions(p.ID, newRunID(), force, !scheduled)
	if err != nil {
		return err
	}
	switch res {
	case state.ClaimOK:
		return executeReconcileAttempt(app, p, run, options, scheduled, nil)
	case state.ClaimActiveRun:
		// An active run already owns the profile; request a newer desired
		// generation so a follow-up reconciliation is eligible (§18.4).
		_ = app.DB.RequestReconcile(p.ID)
		return fmt.Errorf("reconciliation already running for %q; follow-up generation requested", p.ID)
	case state.ClaimGateBlocked:
		// A terminal/retry gate blocks automatic claims. Only explicit manual
		// requests (reconcile-now) reopen eligibility; scheduled safety-net
		// runs and watch-triggered destructive reconciles respect the gate
		// because ordinary filesystem events must not clear terminal errors
		// (§18.4, §20).
		if scheduled {
			return fmt.Errorf("reconciliation for %q is gated (%s); run 'knowledge-sync sync %s' to reopen eligibility",
				p.ID, gateReason(app, p.ID), p.ID)
		}
		if err := app.DB.ReopenSyncGate(p.ID); err != nil {
			return err
		}
		run, res, err = app.DB.ClaimRunMode(p.ID, newRunID(), force)
		if err != nil {
			return err
		}
		if res == state.ClaimOK {
			return executeReconcileAttempt(app, p, run, options, scheduled, nil)
		}
		return fmt.Errorf("reconciliation gate re-opened but claim rejected")
	case state.ClaimNoDebt:
		if !scheduled {
			fmt.Printf("reconcile %s: nothing to reconcile\n", p.ID)
		}
		return nil
	case state.ClaimDeferred:
		return nil
	case state.ClaimProfileInactive:
		return fmt.Errorf("profile %q is not eligible for reconciliation (disabled, tombstoned, or deleting)", p.ID)
	}
	return fmt.Errorf("unexpected claim result")
}

func gateReason(app *App, id string) string {
	ss, err := app.DB.GetSyncState(id)
	if err != nil || ss.RetryClassification == nil {
		return "gate blocked"
	}
	return *ss.RetryClassification
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
