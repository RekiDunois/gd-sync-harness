package sync

import (
	"context"
	"fmt"

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
	if err := r.Service.FullSync(ctx, p, options); err != nil {
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

// markDirtyIfChanged re-scans after the live sync and requests a follow-up
// reconciliation if the source changed during sync (§15.1.7).
func (r *Reconciler) markDirtyIfChanged(ctx context.Context, p *state.Profile, pre *PreflightResult) error {
	scan, err := ScanLocal(p)
	if err != nil {
		return err
	}
	if scan.ChangedFingerprint() != pre.SourceFingerprint {
		return r.Service.DB.RequestReconcile(p.ID)
	}
	return nil
}
