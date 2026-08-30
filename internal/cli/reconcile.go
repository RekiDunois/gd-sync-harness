package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

func newReconcileScheduledCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile-scheduled <profile>",
		Short: "Request the hourly safety reconciliation for a profile (launchd entrypoint)",
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
			// Persist durable scheduled intent, wake the worker, and exit
			// without waiting for transfer completion (§12.1).
			gen, err := app.DB.SubmitScheduledReconcile(p.ID)
			if err != nil {
				return err
			}
			wakeWorker(app, p.ID)
			fmt.Printf("reconciliation scheduled for %s (generation %d); the worker owns execution\n", p.ID, gen)
			return nil
		},
	}
}

func newReconcileNowCmd() *cobra.Command {
	var allowDeletes int
	c := &cobra.Command{
		Use:   "reconcile-now <profile>",
		Short: "Run full authoritative reconciliation with preflight + delete budget (worker-owned)",
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
			if p.DeletionRequestedAt != nil {
				return fmt.Errorf("profile %q is being deleted; reconcile-now rejected", p.ID)
			}
			// Submit the durable one-attempt manual intent and wait for the
			// submitted generation in the worker (§12.1, §14.4).
			gen, err := app.DB.SubmitManualReconcile(p.ID, state.ManualReconcileIntent{
				AllowDeletes: allowDeletes, BypassDebounce: true,
			})
			if err != nil {
				return err
			}
			backupNow(app)
			wakeWorker(app, p.ID)
			if err := waitForGeneration(app, p.ID, gen); err != nil {
				return fmt.Errorf("reconciliation for %q did not reach generation %d: %w", p.ID, gen, err)
			}
			fmt.Printf("reconcile %s: complete (generation %d)\n", p.ID, gen)
			return nil
		},
	}
	c.Flags().IntVar(&allowDeletes, "allow-deletes", 0,
		"one reconcile attempt's ordinary deletion budget (distinct from `prune execute --allow-deletes`, which is a durable ceiling for one immutable suppressed-object request)")
	return c
}

// runReconcile is the shared control-plane entrypoint used by sync-now upgrade
// paths and profile migration (§7, §12.1). It persists durable intent, wakes
// the worker, and (when wait is true) waits for the submitted generation. It
// never executes a competing transfer inside the CLI process.
func runReconcile(app *App, p *state.Profile, options sync.SyncOptions, wait bool) error {
	intent := state.ManualReconcileIntent{AllowDeletes: options.AllowDeletes, BypassDebounce: true}
	gen, err := app.DB.SubmitManualReconcile(p.ID, intent)
	if err != nil {
		return err
	}
	wakeWorker(app, p.ID)
	if !wait {
		return nil
	}
	if err := waitForGeneration(app, p.ID, gen); err != nil {
		return fmt.Errorf("reconciliation for %q did not reach generation %d: %w", p.ID, gen, err)
	}
	return nil
}

// wakeWorker sends a best-effort invalidation/wake message so the worker
// reschedules promptly. Durable SQLite intent remains authoritative; the 5s
// worker rescan is the correctness fallback (§5.4, §15.2).
func wakeWorker(app *App, profileID string) {
	configured, _ := app.DB.GetSetting(state.SettingWorkerSocketPath)
	path := live.ResolveSocketPath(configured)
	live.SendInvalidate(path, profileID)
}

// waitForGeneration observes until last_success_generation >= minimum or a
// blocking lifecycle/terminal condition (§14.3, §14.4). It is socket-first with
// SQLite fallback and never cancels the worker.
func waitForGeneration(app *App, profileID string, minimum int64) error {
	obs := socketObserver(app, profileID)
	deadline := time.Now().Add(24 * time.Hour)
	for {
		stream, err := obs.Connect()
		if err == nil {
			ok, done := waitStreamGeneration(stream, profileID, minimum)
			stream.Close()
			if done {
				if ok {
					return nil
				}
				return fmt.Errorf("wait terminated before generation %d was reached", minimum)
			}
			time.Sleep(waiterPollInterval)
			continue
		}
		// SQLite fallback.
		ok, done, err := durableGenerationReached(app, profileID, minimum)
		if err != nil {
			return err
		}
		if done {
			if ok {
				return nil
			}
			return fmt.Errorf("wait terminated before generation %d was reached", minimum)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for generation %d", minimum)
		}
		time.Sleep(waiterPollInterval)
	}
}

// waitStreamGeneration consumes snapshots until generation success or terminal.
// Returns (ok, done).
func waitStreamGeneration(stream *live.Stream, profileID string, minimum int64) (bool, bool) {
	for {
		snap, err := stream.Next()
		if err != nil {
			return false, false // reconnect/fallback
		}
		if snap.ProfileID != profileID {
			continue
		}
		if snap.Sync.LastSuccessGeneration != nil && *snap.Sync.LastSuccessGeneration >= minimum {
			return true, true
		}
		done, err := terminalFromSnapshot(snap)
		if err != nil {
			return false, true
		}
		if done {
			// Ready for an older generation is not success for this waiter.
			return false, false
		}
	}
}

// durableGenerationReached is the SQLite fallback generation check.
func durableGenerationReached(app *App, profileID string, minimum int64) (bool, bool, error) {
	ss, err := app.DB.GetSyncState(profileID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return false, true, fmt.Errorf("profile %q is no longer present (deleted)", profileID)
		}
		return false, true, err
	}
	if ss.LastSuccessGeneration != nil && *ss.LastSuccessGeneration >= minimum {
		return true, true, nil
	}
	// Terminal conditions terminate the wait even if the generation was not
	// reached.
	if ss.State == state.StateError {
		if ss.RetryClassification != nil && *ss.RetryClassification == state.RetryTerminal {
			return false, true, fmt.Errorf("profile %q blocked by terminal error: %s", profileID, stringOr(ss.LastError, "unknown"))
		}
	}
	return false, false, nil
}
