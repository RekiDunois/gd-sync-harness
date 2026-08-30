package sync

import (
	"context"
	"fmt"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/state"
)

// Reconciler orchestrates a full authoritative reconciliation.
type Reconciler struct {
	Service *Service
}

// NewReconciler builds a Reconciler.
func NewReconciler(s *Service) *Reconciler { return &Reconciler{Service: s} }

// Reconcile runs the complete safety flow:
//
//  1. stable-generation preflight;
//  2. authoritative dry-run / preflight;
//  3. if source changed during preflight → discard, restart;
//  4. if expected deletions exceed budget (unless overridden) → fail closed;
//  5. run live destructive sync with rclone's --max-delete guard;
//  6. mark profile dirty if local changes occurred during live sync.
func (r *Reconciler) Reconcile(ctx context.Context, p *state.Profile, options SyncOptions) (*PreflightResult, error) {
	pre, err := r.ReconcileProgress(ctx, p, options, nil)
	return pre, err
}

// ReconcileProgress is Reconcile with best-effort transfer progress callbacks
// (§10.1). onStats may be nil.
func (r *Reconciler) ReconcileProgress(ctx context.Context, p *state.Profile, options SyncOptions, onStats func(exec.ProgressStats)) (*PreflightResult, error) {
	return r.reconcileProgress(ctx, p, options, onStats, nil)
}

// ReconcileProgressWithPhase exposes the real planning/uploading boundary to
// status without duplicating the reconciliation algorithm.
func (r *Reconciler) ReconcileProgressWithPhase(ctx context.Context, p *state.Profile, options SyncOptions, onStats func(exec.ProgressStats), onPhase func(string)) (*PreflightResult, error) {
	return r.reconcileProgress(ctx, p, options, onStats, onPhase)
}

func (r *Reconciler) reconcileProgress(ctx context.Context, p *state.Profile, options SyncOptions, onStats func(exec.ProgressStats), onPhase func(string)) (*PreflightResult, error) {
	if onPhase != nil {
		onPhase(state.PhasePlanning)
	}
	pre, err := r.Preflight(ctx, p, options)
	if err != nil {
		return nil, err
	}
	if pre.RemoteDup {
		return nil, fmt.Errorf("remote duplicates detected; fail closed (run repair-duplicates)")
	}
	if !pre.SourceStable {
		return nil, ErrSourceUnstable
	}
	if pre.ToDelete > effectiveDeleteLimit(p, options) {
		return pre, ErrDeleteBudgetExceeded
	}
	if onPhase != nil {
		onPhase(state.PhaseUploading)
	}
	if _, err := r.Service.fullSync(ctx, p, options, onStats); err != nil {
		return pre, err
	}
	if err := r.markDirtyIfChanged(ctx, p, pre); err != nil {
		return pre, err
	}
	return pre, nil
}

// Preflight performs the dry-run against a stable source generation.
func (r *Reconciler) Preflight(ctx context.Context, p *state.Profile, options SyncOptions) (*PreflightResult, error) {
	scan1, err := ScanLocal(p)
	if err != nil {
		return nil, err
	}
	fp1 := scan1.ChangedFingerprint()

	dry, err := r.Service.DryRunSync(ctx, p, options)
	if err != nil {
		return nil, err
	}
	dry.SourceFiles = len(scan1.Entries)
	dry.SourceFingerprint = fp1

	scan2, err := ScanLocal(p)
	if err != nil {
		return nil, err
	}
	fp2 := scan2.ChangedFingerprint()
	dry.SourceStable = fp1 == fp2

	return dry, nil
}

// markDirtyIfChanged re-scans after the live sync and advances desired
// reconciliation intent if the source changed during sync (§15.1.7). Advancing
// the durable generation leaves the follow-up reconciliation eligible; it never
// expands the already-completed run's target (§20).
func (r *Reconciler) markDirtyIfChanged(ctx context.Context, p *state.Profile, pre *PreflightResult) error {
	scan, err := ScanLocal(p)
	if err != nil {
		return err
	}
	if scan.ChangedFingerprint() != pre.SourceFingerprint {
		gen, err := r.Service.DB.BumpGeneration(p.ID)
		if err != nil {
			return err
		}
		return r.Service.DB.EnsureReconcileGeneration(p.ID, gen)
	}
	return nil
}
