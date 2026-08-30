package cli

import (
	"context"
	"fmt"
	"log"
	"strings"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/live"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// runAuthorizedPrune executes an authorized, policy-valid prune request under
// the profile lock and remote lease (§14). It is a worker-owned data-plane
// operation: only pending/running/retrying requests are executed, never a
// re-derived candidate set.
func runAuthorizedPrune(ctx context.Context, app *App, p *state.Profile, snap *policy.Snapshot, lg *log.Logger) error {
	lg = workerLog(lg)

	// Recover an interrupted prune first (§20.4).
	req, err := app.DB.ClaimRetryingPrune(p.ID)
	if err != nil {
		return err
	}
	if req == nil {
		req, err = app.DB.ClaimPrune(p.ID)
		if err != nil {
			return err
		}
	}
	if req == nil {
		return nil
	}
	if req.State == state.PruneStateStale {
		lg.Printf("prune #%s stale (policy changed); delete zero files", req.RequestID)
		return nil
	}

	pol, _ := app.DB.GetCommittedPolicy(p.ID)
	if pol == nil || pol.PolicyHash != req.PolicyHash || pol.RefreshState != state.PolicyRefreshReady {
		// Policy no longer matches the request's immutable authorization.
		lg.Printf("prune #%s policy mismatch; delete zero files", req.RequestID)
		return nil
	}

	// Publish prune activity.
	reqID := req.RequestID
	app.activities().start(p.ID, live.ActivityPrune, &reqID)
	defer app.activities().finish(p.ID)
	app.activities().setPhase(p.ID, state.PhaseDeleting)
	publishPrune := func() {
		if app.LiveServer != nil {
			app.LiveServer.PublishActivity(p.ID, app.activities().snapshot(p.ID))
		}
	}
	publishPrune()

	targets, err := app.DB.PruneTargets(req.RequestID)
	if err != nil {
		return err
	}

	// Delete only the immutable managed target rows for this request (§14.5).
	for _, t := range targets {
		if t.State == state.PruneTargetDeleted || t.State == state.PruneTargetMissing {
			continue
		}
		res := app.Rclone.Run(ctx, "deletefile", p.RemoteName+":"+p.RemoteDisplayPath+"/"+t.RelPath)
		if res.Err != nil {
			// A missing remote object converges to success (§14.5).
			if pruneRemoteMissing(res) {
				if err := app.DB.MarkPruneTargetResult(req.RequestID, t.RelPath, state.PruneTargetMissing, ""); err != nil {
					return err
				}
				continue
			}
			// Retryable provider errors move the request to retrying; durable
			// authorization is preserved (§14.6).
			_ = app.DB.SetPruneRetrying(req.RequestID, res.StderrTrimmed())
			return fmt.Errorf("prune %s target %s: %w: %s", req.RequestID, t.RelPath, res.Err, res.StderrTrimmed())
		}
		if err := app.DB.MarkPruneTargetResult(req.RequestID, t.RelPath, state.PruneTargetDeleted, ""); err != nil {
			return err
		}
	}
	// All targets confirmed; compact and complete (§14.7).
	if err := app.DB.CommitPruneComplete(req.RequestID); err != nil {
		return err
	}
	lg.Printf("prune #%s completed (%d deleted, %d missing)", req.RequestID, req.DeletedCount, req.MissingCount)
	app.liveReader().Refresh(p.ID)
	publishPrune()
	return nil
}

// pruneRemoteMissing classifies a deletefile error as the remote object being
// absent. Most backends treat a missing object as success; when deletefile
// errors, a message mentioning not-exist/missing means the object is already
// gone and the target converges to success.
func pruneRemoteMissing(res rcexec.Result) bool {
	msg := strings.ToLower(res.StderrTrimmed())
	return strings.Contains(msg, "not exist") || strings.Contains(msg, "file not found") ||
		strings.Contains(msg, "missing") || strings.Contains(msg, "not found")
}
