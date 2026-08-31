package cli

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path"
	"strconv"
	"strings"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

const pruneDeleteBatchSize = 512

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
	pending := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.State == state.PruneTargetDeleted || t.State == state.PruneTargetMissing {
			continue
		}
		if err := validatePruneTargetPath(t.RelPath); err != nil {
			return fmt.Errorf("prune %s invalid frozen target: %w", req.RequestID, err)
		}
		pending = append(pending, t.RelPath)
	}

	// Resolve current existence in one filtered listing. This preserves the
	// deleted-vs-missing summary without paying for one rclone process per
	// target. --files-from-raw is an exact allow-list rooted at the managed
	// profile path; missing names are not treated as an rclone error.
	existing, missing, err := classifyPruneRemotePaths(ctx, app, p, pending)
	if err != nil {
		_ = app.DB.SetPruneRetrying(req.RequestID, err.Error())
		return err
	}
	if len(missing) > 0 {
		if err := app.DB.MarkPruneTargetsResultBatch(req.RequestID, missing, state.PruneTargetMissing); err != nil {
			return err
		}
		app.liveReader().Refresh(p.ID)
		publishPrune()
	}

	// Delete the exact immutable target set in bounded batches. rclone's delete
	// implementation fans deletions out through its checker pool, so each batch
	// reuses one authenticated process while preserving durable checkpoints
	// between batches. A failed batch is intentionally not marked successful;
	// on retry, the existence pass above classifies any partial deletes as
	// already missing and safely resumes the remainder.
	for start := 0; start < len(existing); start += pruneDeleteBatchSize {
		end := start + pruneDeleteBatchSize
		if end > len(existing) {
			end = len(existing)
		}
		batch := existing[start:end]
		if err := deletePruneBatch(ctx, app, p, batch); err != nil {
			_ = app.DB.SetPruneRetrying(req.RequestID, err.Error())
			return err
		}
		if err := app.DB.MarkPruneTargetsResultBatch(req.RequestID, batch, state.PruneTargetDeleted); err != nil {
			return err
		}
		app.liveReader().Refresh(p.ID)
		publishPrune()
	}

	// All targets confirmed absent; compact and complete (§14.7).
	if err := app.DB.CommitPruneComplete(req.RequestID); err != nil {
		return err
	}
	completed, _ := app.DB.GetPruneRequest(req.RequestID)
	if completed != nil {
		lg.Printf("prune #%s completed (%d deleted, %d missing)", completed.RequestID, completed.DeletedCount, completed.MissingCount)
	} else {
		lg.Printf("prune #%s completed", req.RequestID)
	}
	app.liveReader().Refresh(p.ID)
	publishPrune()
	return nil
}

func classifyPruneRemotePaths(ctx context.Context, app *App, p *state.Profile, relPaths []string) (existing, missing []string, err error) {
	if len(relPaths) == 0 {
		return nil, nil, nil
	}
	listPath, err := writePrunePathList(relPaths)
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(listPath)

	args := []string{"lsf", "--recursive", "--files-only", "--files-from-raw", listPath}
	args = append(args, app.Config.Rclone.GlobalArgs...)
	args = append(args, p.RemoteName+":"+p.RemoteDisplayPath)
	res := app.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return nil, nil, fmt.Errorf("list prune targets: %w: %s", res.Err, res.StderrTrimmed())
	}

	found := make(map[string]struct{}, len(relPaths))
	scanner := bufio.NewScanner(strings.NewReader(string(res.Stdout)))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		rel := scanner.Text()
		if rel != "" {
			found[rel] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("parse prune target listing: %w", err)
	}

	existing = make([]string, 0, len(relPaths))
	missing = make([]string, 0)
	for _, rel := range relPaths {
		if _, ok := found[rel]; ok {
			existing = append(existing, rel)
		} else {
			missing = append(missing, rel)
		}
	}
	return existing, missing, nil
}

func deletePruneBatch(ctx context.Context, app *App, p *state.Profile, relPaths []string) error {
	if len(relPaths) == 0 {
		return nil
	}
	listPath, err := writePrunePathList(relPaths)
	if err != nil {
		return err
	}
	defer os.Remove(listPath)

	args := []string{
		"delete",
		"--files-from-raw", listPath,
		"--max-delete", strconv.Itoa(len(relPaths)),
	}
	args = append(args, app.Config.Rclone.GlobalArgs...)
	args = append(args, p.RemoteName+":"+p.RemoteDisplayPath)
	res := app.Rclone.Run(ctx, args...)
	if res.Err != nil {
		return fmt.Errorf("delete prune batch (%d targets): %w: %s", len(relPaths), res.Err, res.StderrTrimmed())
	}
	return nil
}

func writePrunePathList(relPaths []string) (string, error) {
	f, err := os.CreateTemp("", "knowledge-sync-prune-*.files")
	if err != nil {
		return "", fmt.Errorf("create prune target list: %w", err)
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()

	for _, rel := range relPaths {
		if err := validatePruneTargetPath(rel); err != nil {
			return "", err
		}
		if _, err := f.WriteString(rel + "\n"); err != nil {
			return "", fmt.Errorf("write prune target list: %w", err)
		}
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close prune target list: %w", err)
	}
	ok = true
	return name, nil
}

func validatePruneTargetPath(rel string) error {
	if rel == "" || strings.ContainsAny(rel, "\r\n\x00") || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("unsafe relative path %q", rel)
	}
	clean := path.Clean(rel)
	if clean != rel || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("unsafe relative path %q", rel)
	}
	return nil
}
