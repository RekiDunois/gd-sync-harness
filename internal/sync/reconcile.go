package sync

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"knowledge-sync/internal/exec"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// Reconciler orchestrates a full authoritative reconciliation.
type Reconciler struct {
	Service *Service
}

// NewReconciler builds a Reconciler.
func NewReconciler(s *Service) *Reconciler { return &Reconciler{Service: s} }

// ReconcileProtected runs the complete safety flow against an owned committed
// policy snapshot (§11). It:
//
//  1. stable-generation preflight using the owned active set;
//  2. authoritative dry-run over the active list (no --delete-excluded, so
//     suppressed remote objects are protected);
//  3. if source changed during preflight → discard, restart;
//  4. if proven-delete count exceeds the budget → fail closed;
//  5. run the live non-destructive upload for the active set;
//  6. delete only the proven ordinary local deletions within the budget;
//  7. mark profile dirty if local changes occurred during live sync.
//
// Suppressed objects are never counted as ordinary deletion candidates (§11.4).
func (r *Reconciler) ReconcileProtected(ctx context.Context, p *state.Profile, snap *policy.Snapshot, options SyncOptions, provenDeletes []string) (*PreflightResult, error) {
	return r.reconcileProtected(ctx, p, snap, options, provenDeletes, nil, nil)
}

// ReconcileProtectedProgress is ReconcileProtected with progress callbacks.
func (r *Reconciler) ReconcileProtectedProgress(ctx context.Context, p *state.Profile, snap *policy.Snapshot, options SyncOptions, provenDeletes []string, onStats func(exec.ProgressStats), onPhase func(string)) (*PreflightResult, error) {
	return r.reconcileProtected(ctx, p, snap, options, provenDeletes, onStats, onPhase)
}

func (r *Reconciler) reconcileProtected(ctx context.Context, p *state.Profile, snap *policy.Snapshot, options SyncOptions, provenDeletes []string, onStats func(exec.ProgressStats), onPhase func(string)) (*PreflightResult, error) {
	if onPhase != nil {
		onPhase(state.PhasePlanning)
	}
	pre, err := r.PreflightProtected(ctx, p, snap, options)
	if err != nil {
		return nil, err
	}
	if pre.RemoteDup {
		return nil, fmt.Errorf("remote duplicates detected; fail closed (run repair-duplicates)")
	}
	if !pre.SourceStable {
		return nil, ErrSourceUnstable
	}
	// The delete budget guards proven ordinary deletions only. Suppressed
	// objects are not candidates (§11.4).
	effective := effectiveDeleteLimit(p, options)
	if len(provenDeletes) > effective {
		return pre, ErrDeleteBudgetExceeded
	}
	if onPhase != nil {
		onPhase(state.PhaseUploading)
	}
	if _, err := r.Service.FullSyncProtected(ctx, p, snap, options, onStats); err != nil {
		return pre, err
	}
	if len(provenDeletes) > 0 {
		if onPhase != nil {
			onPhase(state.PhaseDeleting)
		}
		if err := r.Service.DeleteRemotePaths(ctx, p, provenDeletes); err != nil {
			return pre, err
		}
	}
	if err := r.markDirtyIfChangedProtected(ctx, p, snap, pre); err != nil {
		return pre, err
	}
	return pre, nil
}

// PreflightProtected performs the dry-run against a stable source generation
// under the owned policy snapshot.
func (r *Reconciler) PreflightProtected(ctx context.Context, p *state.Profile, snap *policy.Snapshot, options SyncOptions) (*PreflightResult, error) {
	active1, err := ScanActiveEntries(p.SourcePath, p.MaxFileSize, snap)
	if err != nil {
		return nil, err
	}
	fp1 := fingerprintActiveEntries(active1)
	duplicates, err := r.Service.remoteDuplicates(ctx, p)
	if err != nil {
		return nil, err
	}
	if duplicates {
		return &PreflightResult{
			SourceFiles:       len(active1),
			RemoteDup:         true,
			SourceFingerprint: fp1,
		}, nil
	}

	dry, err := r.Service.DryRunSyncProtected(ctx, p, snap, options)
	if err != nil {
		return nil, err
	}
	dry.SourceFiles = len(active1)
	dry.SourceFingerprint = fp1

	active2, err := ScanActiveEntries(p.SourcePath, p.MaxFileSize, snap)
	if err != nil {
		return nil, err
	}
	dry.SourceStable = fp1 == fingerprintActiveEntries(active2)
	return dry, nil
}

// fingerprintActiveEntries is a stable, order-independent fingerprint of the
// active entries including high-resolution size and mtime (§9.3). A same-path
// content change that preserves the path (size or mtime) is detected without a
// content hash. Ignored churn never appears in the entry set, so it cannot
// destabilize the fingerprint.
func fingerprintActiveEntries(entries []ActiveEntry) string {
	if len(entries) == 0 {
		return "empty"
	}
	sorted := append([]ActiveEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RelPath < sorted[j].RelPath })
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d:", len(sorted))
	for _, e := range sorted {
		fmt.Fprintf(&sb, "%s:%d:%d;", e.RelPath, e.Size, e.ModTimeNS)
	}
	return sb.String()
}

// markDirtyIfChangedProtected re-scans after the live sync and advances desired
// reconciliation intent if the source changed during sync (§15.1.7).
func (r *Reconciler) markDirtyIfChangedProtected(ctx context.Context, p *state.Profile, snap *policy.Snapshot, pre *PreflightResult) error {
	active, err := ScanActiveEntries(p.SourcePath, p.MaxFileSize, snap)
	if err != nil {
		return err
	}
	if fingerprintActiveEntries(active) != pre.SourceFingerprint {
		gen, err := r.Service.DB.BumpGeneration(p.ID)
		if err != nil {
			return err
		}
		return r.Service.DB.EnsureReconcileGeneration(p.ID, gen)
	}
	return nil
}
