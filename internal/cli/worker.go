package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/flock"
	"knowledge-sync/internal/logger"
	"knowledge-sync/internal/paths"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

// workerPollInterval is the idle re-scan interval for the reconciliation
// worker. Waking signals are optimization only; durable state is authoritative
// (§9.3, §17.4).
const workerPollInterval = 5 * time.Second

// newWorkerCmd runs the single reconciliation worker process (§9.5). It is the
// sole owner of reconciliation execution for V1.
func newWorkerCmd() *cobra.Command {
	var once bool
	var profile string
	c := &cobra.Command{
		Use:   "worker [profile]",
		Short: "Run the reconciliation worker daemon (single runtime owner)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			if len(args) == 1 {
				profile = args[0]
			}
			return runWorker(app, profile, once)
		},
	}
	c.Flags().BoolVar(&once, "once", false, "run a single scheduling pass and exit")
	return c
}

// workerOwnershipLockName is the singleton worker lock (PID-aware, recoverable).
const workerOwnershipLockName = "worker"

func runWorker(app *App, onlyProfile string, once bool) error {
	stateDir, err := paths.StateDir()
	if err != nil {
		return err
	}
	lockDir := filepath.Join(stateDir, "locks")
	lock, err := flock.Acquire(lockDir, workerOwnershipLockName)
	if err != nil {
		return fmt.Errorf("worker singleton ownership: %w (another worker may be running)", err)
	}
	defer lock.Release()

	logPath := app.logPathFor("worker", "worker")
	lg, err := logger.New(logPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := workerRecover(ctx, app, onlyProfile, lg); err != nil {
		lg.Printf("recovery pass error: %v", err)
	}

	if once {
		return runWorkerPass(ctx, app, onlyProfile, lg)
	}

	for {
		if err := runWorkerPass(ctx, app, onlyProfile, lg); err != nil {
			lg.Printf("worker pass error: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(workerPollInterval):
		}
	}
}

// workerRecover reacquires ownership: inherited running attempts become
// orphaned (worker_interrupted, immediately retryable) and accepted deletion
// requests are finalized when no active run holds ownership (§17.2, §17.3).
func workerRecover(ctx context.Context, app *App, onlyProfile string, lg *log.Logger) error {
	lg = workerLog(lg)
	ps, err := activeProfilesFor(app, onlyProfile)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if err := app.DB.OrphanCurrentRun(p.ID); err != nil {
			lg.Printf("orphan recovery %s: %v", p.ID, err)
			continue
		}
	}
	if err := finalizeDeletions(app); err != nil {
		return err
	}
	return nil
}

// workerLog returns lg if non-nil, otherwise a discard logger, so callers may
// pass nil in tests or one-shot contexts.
func workerLog(lg *log.Logger) *log.Logger {
	if lg == nil {
		return log.New(io.Discard, "", 0)
	}
	return lg
}

// runWorkerPass performs one scheduling pass: claim and execute eligible
// reconciliation for each active profile, then finalize pending deletions.
func runWorkerPass(ctx context.Context, app *App, onlyProfile string, lg *log.Logger) error {
	lg = workerLog(lg)
	ps, err := activeProfilesFor(app, onlyProfile)
	if err != nil {
		return err
	}
	for _, p := range ps {
		run, res, err := app.DB.ClaimRun(p.ID, newRunID())
		if err != nil {
			lg.Printf("claim %s: %v", p.ID, err)
			continue
		}
		switch res {
		case state.ClaimOK:
			lg.Printf("claimed run %s for %s (target_generation=%d, kind=%s)",
				run.ID, p.ID, run.TargetGeneration, run.Kind)
			if err := executeReconcileAttempt(app, p, run, sync.SyncOptions{}, true, lg); err != nil {
				lg.Printf("run %s (%s) failed: %v", run.ID, p.ID, err)
			} else {
				lg.Printf("run %s (%s) succeeded", run.ID, p.ID)
			}
		case state.ClaimNoDebt, state.ClaimGateBlocked, state.ClaimActiveRun, state.ClaimProfileInactive:
			// Not claimable right now; wait for the next pass.
		}
	}
	return finalizeDeletions(app)
}

// finalizeDeletions tombstones profiles with a durable deletion request once no
// active run remains (§19). Deletion intent has higher scheduling priority than
// reconciliation/retry intent and is never re-opened by filesystem events.
func finalizeDeletions(app *App) error {
	ps, err := app.DB.DeletingProfiles()
	if err != nil {
		return err
	}
	for _, p := range ps {
		ss, err := app.DB.GetSyncState(p.ID)
		if err != nil || ss.CurrentRunID != nil {
			continue
		}
		if err := app.DB.ClearPending(p.ID); err != nil {
			return err
		}
		if err := app.DB.TombstoneProfile(p.ID); err != nil {
			return err
		}
		if err := app.DB.CancelProfileDeletion(p.ID); err != nil {
			return err
		}
	}
	return nil
}

// executeReconcileAttempt runs a claimed reconciliation through the normal
// worker-owned path and atomically commits success/failure (§24.2). It is the
// single shared execution path for the worker, the CLI reconcile commands, and
// --wait observation.
func executeReconcileAttempt(app *App, p *state.Profile, run *state.SyncRun, options sync.SyncOptions, scheduled bool, lg *log.Logger) error {
	ctx, cancel := app.Context()
	defer cancel()

	if err := validateOwnership(ctx, app, p); err != nil {
		return commitFailure(app, p, run, err, "ownership_validation")
	}
	if err := app.DB.UpdateRunPhase(p.ID, run.ID, state.PhaseScanning); err != nil {
		return err
	}

	scan, err := sync.ScanLocal(p)
	if err != nil {
		return commitFailure(app, p, run, err, "local_scan")
	}
	if err := app.DB.UpdateRunFilesDiscovered(p.ID, run.ID, int64(len(scan.Entries))); err != nil {
		return err
	}
	if err := app.DB.UpdateRunPhase(p.ID, run.ID, state.PhaseUploading); err != nil {
		return err
	}

	pre, err := app.Reconciler.ReconcileProgress(ctx, p, options, func(s rcexec.ProgressStats) {
		_ = app.DB.UpdateRunProgress(p.ID, run.ID, s.TransferredFiles, s.TransferredBytes, s.TotalBytes)
	})
	if err != nil {
		if errors.Is(err, sync.ErrDeleteBudgetExceeded) {
			return commitFailure(app, p, run, err, "delete_budget_exceeded")
		}
		return commitFailure(app, p, run, err, "reconcile")
	}

	if err := refreshManifest(app.DB, p, pre); err != nil {
		return commitFailure(app, p, run, err, "manifest_refresh")
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		return err
	}
	_ = app.DB.ClearPending(p.ID)
	if !scheduled {
		fmt.Printf("reconcile %s: %d source files, %d copies, %d deletions\n",
			p.ID, pre.SourceFiles, pre.ToCopy, pre.ToDelete)
	}
	return nil
}

// commitFailure persists a structured failure and returns the original error.
func commitFailure(app *App, p *state.Profile, run *state.SyncRun, err error, code string) error {
	_ = app.DB.CommitRunFailure(p.ID, run.ID, classifyError(err, code))
	return err
}

// classifyError maps an execution error to a structured retry/terminal
// classification (§18.1). Unknown/unclassified failures default to Terminal.
func classifyError(err error, code string) state.RunFailure {
	f := state.RunFailure{Code: code, Classification: state.RetryTerminal, Message: err.Error()}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		f.Code = "timeout"
		f.Classification = state.RetryRetryable
		return f
	}
	if errors.Is(err, sync.ErrSourceUnstable) {
		f.Code = "source_unstable"
		f.Classification = state.RetryRetryable
		return f
	}
	if errors.Is(err, sync.ErrDeleteBudgetExceeded) {
		f.Code = "delete_budget_exceeded"
		f.Classification = state.RetryTerminal
		return f
	}

	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 5 {
			// rclone exit code 5 = temporary error (retry may help).
			f.Code = "rclone_temporary"
			f.Classification = state.RetryRetryable
			return f
		}
	}
	return f
}

// activeProfilesFor lists non-tombstoned, enabled, non-deleting profiles,
// optionally restricted to one profile ID.
func activeProfilesFor(app *App, only string) ([]*state.Profile, error) {
	ps, err := app.DB.ActiveProfiles()
	if err != nil {
		return nil, err
	}
	var out []*state.Profile
	for _, p := range ps {
		if only != "" && p.ID != only {
			continue
		}
		if !p.Enabled {
			continue
		}
		if p.DeletionRequestedAt != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// newRunID generates a unique sync_run identifier.
func newRunID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

var _ = os.Exit
