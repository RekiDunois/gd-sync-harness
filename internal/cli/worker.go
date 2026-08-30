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
	"knowledge-sync/internal/live"
	"knowledge-sync/internal/logger"
	"knowledge-sync/internal/paths"
	"knowledge-sync/internal/sidecar"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
	"knowledge-sync/pkg/version"
)

// workerPollInterval is the idle re-scan interval for the reconciliation
// worker. Waking signals are optimization only; durable state is authoritative
// (§9.3, §17.4).
const workerPollInterval = 5 * time.Second

// Architecture note (§2, §24): the worker is the single synchronization
// data-plane owner. Reconciliation intent, retry state, and event facts live in
// SQLite; rclone operations only ever run inside this process. Live telemetry
// flows over the Unix status socket from in-memory activity state — SQLite is
// never a per-frame telemetry transport. Observer commands (profile status,
// status --watch, wait, reconcile-now waiting) are socket-first with a coarse
// SQLite fallback that omits Speed and live-stall diagnosis.

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
	if app.LockDir != "" {
		lockDir = app.LockDir
	}
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
	lg.Printf("worker starting: %s", version.String())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the live status server after singleton ownership is confirmed
	// (§4.4). Socket failure must not corrupt durable state or prevent
	// reconciliation: it is logged prominently and observers fall back to
	// SQLite.
	liveServer := startWorkerLiveServer(app, lg)
	app.LiveServer = liveServer
	if liveServer != nil {
		defer liveServer.Stop()
	}

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

// startWorkerLiveServer resolves the socket path, prepares the runtime
// directory, and starts the live status server. It returns nil when the socket
// cannot be served so reconciliation continues without live IPC (§4.4).
func startWorkerLiveServer(app *App, lg *log.Logger) *live.Server {
	configured, _ := app.DB.GetSetting(state.SettingWorkerSocketPath)
	path := live.ResolveSocketPath(configured)
	// The initial-subscribe snapshot must merge activity already in worker
	// memory (§6.4).
	app.liveReader().SetActivityProvider(func(profileID string) *live.ActivityS {
		return app.activities().snapshot(profileID)
	})
	server := live.NewServer(live.ServerOptions{
		Path:      path,
		IsDefault: configured == "",
		Reader:    app.liveReader(),
		Log:       lg,
	})
	if err := server.Start(); err != nil {
		lg.Printf("live status socket unavailable at %s: %v (observers will use SQLite fallback)", path, err)
		return nil
	}
	lg.Printf("live status socket listening at %s", path)
	return server
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
// reconciliation for each active profile, drain due fast events, then finalize
// pending deletions (§13.3).
func runWorkerPass(ctx context.Context, app *App, onlyProfile string, lg *log.Logger) error {
	lg = workerLog(lg)
	if ctx == nil {
		ctx = context.Background()
	}
	ps, err := activeProfilesFor(app, onlyProfile)
	if err != nil {
		return err
	}
	for _, p := range ps {
		// Refresh the durable cache on every scheduling rescan so lost
		// invalidate messages are eventually corrected (§6.1).
		app.liveReader().Refresh(p.ID)
		lock, lockErr := acquireProfileLock(app, p)
		if lockErr != nil {
			if !errors.Is(lockErr, flock.ErrLocked) {
				lg.Printf("profile lock %s: %v", p.ID, lockErr)
			}
			continue
		}
		func() {
			defer lock.Release()
			lease := leaseID()
			if err := app.DB.AcquireRemoteLease(ctx, p.RemoteName, 1, 2, os.Getpid(), lease); err != nil {
				if !errors.Is(err, context.Canceled) {
					lg.Printf("remote lease %s: %v", p.ID, err)
				}
				return
			}
			stopRenewal := startLeaseRenewal(ctx, app.DB, lease)
			defer stopRenewal()
			defer app.DB.ReleaseRemoteLease(lease)
			run, res, err := app.DB.ClaimRun(p.ID, newRunID())
			if err != nil {
				lg.Printf("claim %s: %v", p.ID, err)
				return
			}
			if res != state.ClaimOK {
				// No full reconciliation was claimable; a due fast-event batch
				// may still be eligible (§13.3: full debt wins, so we only
				// drain fast events when full debt is absent).
				if err := runFastUpsertBatch(ctx, app, p, lg); err != nil {
					lg.Printf("fast upsert %s: %v", p.ID, err)
				}
				return
			}
			lg.Printf("claimed run %s for %s (target_generation=%d, kind=%s)", run.ID, p.ID, run.TargetGeneration, run.Kind)
			// Execution options derive from the claimed durable run/claim
			// result, not from the original CLI process (§11.4). The effective
			// delete limit was atomically captured at claim time.
			options := sync.SyncOptions{AllowDeletes: run.EffectiveMaxDelete}
			if err := executeReconcileAttempt(app, p, run, options, true, lg); err != nil {
				lg.Printf("run %s (%s) failed: %v", run.ID, p.ID, err)
			} else {
				lg.Printf("run %s (%s) succeeded", run.ID, p.ID)
			}
		}()
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

	// Live activity begins with the run so observers see it immediately.
	runID := run.ID
	app.activities().start(p.ID, live.ActivityFullReconcile, &runID)
	defer app.activities().finish(p.ID)
	publishLive := func() {
		if app.LiveServer != nil {
			app.LiveServer.PublishActivity(p.ID, app.activities().snapshot(p.ID))
		}
	}

	if err := validateOwnership(ctx, app, p); err != nil {
		return commitFailure(app, p, run, err, "ownership_validation")
	}
	if err := app.DB.UpdateRunPhase(p.ID, run.ID, state.PhaseScanning); err != nil {
		return err
	}
	app.activities().setPhase(p.ID, state.PhaseScanning)
	publishLive()

	scan, err := sync.ScanLocalProgress(p, func(progress sync.ScanProgress) {
		_ = app.DB.UpdateRunFilesDiscovered(p.ID, run.ID, int64(progress.Eligible))
	})
	if err != nil {
		return commitFailure(app, p, run, err, "local_scan")
	}
	if err := app.DB.UpdateRunFilesDiscovered(p.ID, run.ID, int64(len(scan.Entries))); err != nil {
		return err
	}
	var previous *rcexec.ProgressStats
	checkpoint := newCheckpointTracker()
	// Phase transitions are required durable checkpoints (§9.2).
	checkpointWrite := func(force bool) {
		now := state.Now()
		if write, measurable := checkpoint.consume(force, now); write {
			snap := app.activities().snapshot(p.ID)
			if snap != nil {
				_ = app.DB.UpdateRunStats(p.ID, run.ID, activityToProgress(snap), measurable)
			}
		}
	}
	pre, err := app.Reconciler.ReconcileProgressWithPhase(ctx, p, options, func(s rcexec.ProgressStats) {
		measurable := s.MeasurableProgress(previous)
		snapshot := progressSnapshot(s)
		// Live memory update + throughput estimate + status publish (§9.1).
		// Durable checkpointing is sparse; no per-frame SQL write.
		app.activities().observe(p.ID, snapshot, measurable, state.Now())
		checkpoint.frame(measurable)
		publishLive()
		checkpointWrite(false)
		copy := s
		previous = &copy
	}, func(phase string) {
		_ = app.DB.UpdateRunPhase(p.ID, run.ID, phase)
		app.activities().setPhase(p.ID, phase)
		publishLive()
		checkpointWrite(true)
	})
	if err != nil {
		if errors.Is(err, sync.ErrDeleteBudgetExceeded) {
			return commitFailure(app, p, run, err, "delete_budget_exceeded")
		}
		return commitFailure(app, p, run, err, "reconcile")
	}

	if err := app.DB.UpdateRunPhase(p.ID, run.ID, state.PhaseFinalizing); err != nil {
		return err
	}
	app.activities().setPhase(p.ID, state.PhaseFinalizing)
	publishLive()
	if err := refreshManifest(app.DB, p, pre); err != nil {
		return commitFailure(app, p, run, err, "manifest_refresh")
	}
	if err := app.DB.CommitRunSuccess(p.ID, run.ID, run.TargetGeneration); err != nil {
		return err
	}
	// Refresh the durable cache so subsequent subscribers see the final state.
	app.liveReader().Refresh(p.ID)
	publishLive()
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
		f.Code = "context_canceled"
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
	var ownershipErr *sidecar.ValidationError
	if errors.As(err, &ownershipErr) {
		f.Code = ownershipErr.Code
		if ownershipErr.Temporary {
			f.Classification = state.RetryRetryable
		}
		return f
	}

	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case 5:
			// rclone exit code 5 is explicitly temporary.
			f.Code = "rclone_temporary"
			f.Classification = state.RetryRetryable
		case 2, 6, 7:
			f.Code = fmt.Sprintf("rclone_exit_%d", exitErr.ExitCode())
			f.Classification = state.RetryTerminal
		case 1:
			f.Code = "rclone_uncategorized"
			f.Classification = state.RetryRetryableLimited
		default:
			f.Code = fmt.Sprintf("rclone_exit_%d", exitErr.ExitCode())
			f.Classification = state.RetryRetryableLimited
		}
		return f
	}
	return f
}

func acquireProfileLock(app *App, p *state.Profile) (*flock.Lock, error) {
	lockDir := app.LockDir
	if lockDir == "" {
		stateDir, err := paths.StateDir()
		if err != nil {
			return nil, err
		}
		lockDir = filepath.Join(stateDir, "locks")
	}
	return flock.Acquire(lockDir, p.ID)
}

// refreshManifest rebuilds the durable manifest from the local source after a
// successful reconciliation.
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

func progressSnapshot(s rcexec.ProgressStats) state.ProgressSnapshot {
	var item *string
	if s.CurrentItemKnown {
		item = &s.CurrentItem
	}
	// SpeedBytesPerSecond is deliberately not populated here: rclone's top-level
	// average speed is not a live current rate (§8.2), and durable checkpoints
	// must not resurrect a stale live-looking speed (§9.3).
	return state.ProgressSnapshot{
		FilesCompleted: s.Transfers, BytesCompleted: s.Bytes, BytesTotal: s.TotalBytes,
		ChecksCompleted: s.Checks, ChecksTotal: s.TotalChecks, ItemsListed: s.Listed,
		ErrorsCount: s.Errors, CurrentItem: item,
		CurrentItemBytes: s.CurrentItemBytes, CurrentItemSize: s.CurrentItemSize,
		ActiveTransfers: s.ActiveTransfers,
	}
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
