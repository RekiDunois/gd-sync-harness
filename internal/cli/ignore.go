package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/policy"
	"knowledge-sync/internal/state"
)

// newIgnoreCmd builds the `profile ignore` subcommand tree.
func newIgnoreCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ignore",
		Short: "Manage the committed .gitignore policy snapshot",
	}
	c.AddCommand(ignoreUpdateCmd(), ignoreStatusCmd())
	return c
}

func ignoreUpdateCmd() *cobra.Command {
	var acceptLegacyDrop bool
	c := &cobra.Command{
		Use:   "update <profile>",
		Short: "Commit the current disk .gitignore snapshot as synchronization policy",
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
			// Explicit policy commit is a profile-lock-owned mutation so it
			// serializes with prune execution and reconciliation (§8.4).
			return app.withProfileLock(p, func() error {
				return runIgnoreUpdate(app, p, acceptLegacyDrop)
			})
		},
	}
	c.Flags().BoolVar(&acceptLegacyDrop, "accept-legacy-drop", false,
		"confirm switching a legacy-migrated profile to the disk .gitignore policy source")
	return c
}

func runIgnoreUpdate(app *App, p *state.Profile, acceptLegacyDrop bool) error {
	// Stable double-capture with a final hash verification before the DB
	// commit (§5.2, §5.3). If lock acquisition waited behind another
	// operation, the final pass still guards against a changed disk policy.
	snap, err := policy.CollectSnapshot(p.SourcePath)
	if err != nil {
		return err
	}
	cur, _ := app.DB.GetCommittedPolicy(p.ID)
	if cur != nil && cur.PolicySource == state.PolicySourceLegacyMigrated && !acceptLegacyDrop {
		legacyRules, _ := app.DB.GetExcludes(p.ID)
		suppressed, _ := app.DB.ManifestSuppressedCount(p.ID)
		fmt.Printf("This profile is using policy migrated from legacy excludes.\n")
		fmt.Printf("The current disk .gitignore snapshot will become the sole policy source.\n\n")
		fmt.Printf("legacy rules:          %d\n", len(legacyRules))
		fmt.Printf("disk ignore files:     %d\n", len(snap.Files))
		fmt.Printf("currently suppressed:  %d\n\n", suppressed)
		fmt.Printf("Re-run with --accept-legacy-drop to commit this policy source change.\n")
		return nil
	}
	// Verify profile still enabled for policy mutation as appropriate.
	res, err := app.DB.CommitIgnoreSnapshot(p.ID, snap, acceptLegacyDrop)
	if err != nil {
		if err == state.ErrLegacyDropRequired {
			legacyRules, _ := app.DB.GetExcludes(p.ID)
			suppressed, _ := app.DB.ManifestSuppressedCount(p.ID)
			fmt.Printf("This profile is using policy migrated from legacy excludes.\n")
			fmt.Printf("The current disk .gitignore snapshot will become the sole policy source.\n\n")
			fmt.Printf("legacy rules:          %d\n", len(legacyRules))
			fmt.Printf("disk ignore files:     %d\n", len(snap.Files))
			fmt.Printf("currently suppressed:  %d\n\n", suppressed)
			fmt.Printf("Re-run with --accept-legacy-drop to commit this policy source change.\n")
			return nil
		}
		return err
	}
	if !res.Changed {
		fmt.Printf("policy: unchanged\n")
		fmt.Printf("ignore files captured: %d\n", len(snap.Files))
		fmt.Printf("policy hash: %s\n", res.PolicyHash)
		fmt.Printf("matcher warnings: %d\n", res.MatcherWarnings)
		fmt.Printf("policy refresh: unchanged\n")
		return nil
	}
	fmt.Printf("policy: changed\n")
	fmt.Printf("ignore files captured: %d\n", len(snap.Files))
	fmt.Printf("policy hash: %s\n", res.PolicyHash)
	fmt.Printf("generation: %d (new)\n", res.Generation)
	fmt.Printf("matcher warnings: %d\n", res.MatcherWarnings)
	fmt.Printf("policy refresh: pending (worker will apply it)\n")
	// Wake the worker so the policy refresh is scheduled promptly.
	wakeWorker(app, p.ID)
	return nil
}

func ignoreStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <profile>",
		Short: "Show detailed committed vs disk ignore policy state (read-only)",
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
			return renderIgnoreStatus(app, p)
		},
	}
}

func renderIgnoreStatus(app *App, p *state.Profile) error {
	pol, err := app.DB.GetCommittedPolicy(p.ID)
	if err != nil {
		if err == state.ErrNotFound {
			fmt.Printf("profile %s: no committed policy\n", p.ID)
			return nil
		}
		return err
	}
	// Compare current disk snapshot (read-only; never commits) (§16.3).
	disk, diskErr := policy.CollectSnapshot(p.SourcePath)
	diskStatus := "unreadable"
	if diskErr == nil {
		if disk.Hash() == pol.PolicyHash {
			diskStatus = "clean"
		} else {
			diskStatus = "modified"
		}
	}
	active, suppressed, _ := app.DB.ManifestCounts(p.ID)

	fmt.Printf("policy_source:          %s\n", pol.PolicySource)
	fmt.Printf("policy_hash:            %s\n", pol.PolicyHash)
	fmt.Printf("committed_generation:   %d\n", pol.CommittedGeneration)
	fmt.Printf("committed_at:           %s\n", pol.CommittedAt)
	files, _ := app.DB.GetPolicySnapshotFiles(p.ID)
	fmt.Printf("committed ignore files: %d\n", len(files))
	fmt.Printf("current disk snapshot:  %s\n", diskStatus)
	fmt.Printf("matcher warnings:       %d\n", pol.MatcherWarningCount)
	fmt.Printf("refresh state:          %s\n", pol.RefreshState)
	if pol.RefreshedPolicyHash != nil {
		fmt.Printf("refreshed hash:         %s\n", *pol.RefreshedPolicyHash)
	}
	fmt.Printf("active managed:         %d\n", active)
	fmt.Printf("suppressed managed:     %d\n", suppressed)
	req, _ := app.DB.GetActivePruneRequest(p.ID)
	if req != nil {
		fmt.Printf("latest prune request:   #%s (%s, %d targets)\n", req.RequestID, req.State, req.CandidateCount)
	} else {
		fmt.Printf("latest prune request:   none\n")
	}
	return nil
}
