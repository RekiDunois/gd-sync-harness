package cli

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

func pruneDiscoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "discover <profile>",
		Short: "Discover ignored remote files missing from the managed ledger and freeze a prune request",
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
				ctx, cancel := app.Context()
				defer cancel()
				if err := validateOwnership(ctx, app, p); err != nil {
					return err
				}
				pol, err := app.DB.GetCommittedPolicy(p.ID)
				if err != nil {
					return err
				}
				if pol.RefreshState != state.PolicyRefreshReady || pol.RefreshedPolicyHash == nil ||
					*pol.RefreshedPolicyHash != pol.PolicyHash {
					return fmt.Errorf("policy refresh pending; orphan discovery not ready")
				}
				snap, err := app.DB.GetCommittedSnapshot(p.ID)
				if err != nil {
					return err
				}
				if snap == nil {
					snap = &policy.Snapshot{}
				}
				paths, err := discoverIgnoredRemoteOrphans(ctx, app, p, snap)
				if err != nil {
					return err
				}
				req, err := app.DB.CreatePrunePreviewFromUnmanagedPaths(p.ID, newRunID(), pol.PolicyHash, paths)
				if err != nil {
					return err
				}
				fmt.Printf("prune orphan preview #%s\n", req.RequestID)
				fmt.Printf("policy hash:   %s\n", req.PolicyHash)
				fmt.Printf("candidate set: %d ignored unmanaged remote file(s)\n", req.CandidateCount)
				fmt.Printf("default limit: %d\n", req.DefaultMaxDelete)
				if req.CandidateCount > 0 {
					fmt.Println("frozen targets:")
					targets, err := app.DB.PruneTargets(req.RequestID)
					if err != nil {
						return err
					}
					for _, target := range targets {
						fmt.Printf("  %s\n", target.RelPath)
					}
				}
				fmt.Println()
				fmt.Println("This request is durable and immutable. To delete the frozen set:")
				fmt.Printf("  knowledge-sync profile prune execute %s\n", req.RequestID)
				fmt.Printf("  knowledge-sync profile prune execute %s --allow-deletes N\n", req.RequestID)
				return nil
			})
		},
	}
}

func discoverIgnoredRemoteOrphans(ctx context.Context, app *App, p *state.Profile, snap *policy.Snapshot) ([]string, error) {
	res := app.Rclone.Run(ctx, "lsf", "--recursive", "--files-only", p.RemoteName+":"+p.RemoteDisplayPath)
	if res.Err != nil {
		return nil, fmt.Errorf("list remote files for orphan discovery: %w: %s", res.Err, res.StderrTrimmed())
	}
	managedRows, err := app.DB.ManifestAll(p.ID)
	if err != nil {
		return nil, err
	}
	managed := make(map[string]struct{}, len(managedRows))
	for _, entry := range managedRows {
		managed[entry.RelPath] = struct{}{}
	}
	matcher := snap.Matcher()
	set := make(map[string]struct{})
	for _, line := range nonEmptyLines(res.StdoutTrimmed()) {
		relPath := strings.TrimSpace(strings.ReplaceAll(line, "\\", "/"))
		if relPath == "" || strings.HasPrefix(relPath, "/") {
			continue
		}
		relPath = path.Clean(relPath)
		if relPath == "." || relPath == ".." || strings.HasPrefix(relPath, "../") {
			continue
		}
		if _, ok := managed[relPath]; ok {
			continue
		}
		if matcher.MatchPath(relPath, false) {
			set[relPath] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for relPath := range set {
		out = append(out, relPath)
	}
	sort.Strings(out)
	return out, nil
}
