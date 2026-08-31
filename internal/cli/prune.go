package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/state"
)

func newPruneCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "prune",
		Short: "Preview and explicitly delete suppressed managed objects",
	}
	c.AddCommand(prunePreviewCmd(), pruneDiscoverCmd(), pruneExecuteCmd(), pruneStatusCmd())
	return c
}

func prunePreviewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "preview <profile>",
		Short: "Create a durable immutable prune request for the current suppressed set",
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
			// Serialize with the worker's profile lock so the suppressed set is
			// stable during preview capture (§13.3).
			return app.withProfileLock(p, func() error {
				req, err := app.DB.CreatePrunePreview(p.ID, newRunID())
				if err != nil {
					return err
				}
				fmt.Printf("prune preview #%s\n", req.RequestID)
				fmt.Printf("policy hash:   %s\n", req.PolicyHash)
				fmt.Printf("candidate set: %d suppressed object(s)\n", req.CandidateCount)
				fmt.Printf("default limit: %d\n", req.DefaultMaxDelete)
				fmt.Println()
				fmt.Println("This request is durable and immutable. To delete the frozen set:")
				fmt.Printf("  knowledge-sync profile prune execute %s\n", req.RequestID)
				fmt.Printf("  knowledge-sync profile prune execute %s --allow-deletes N\n", req.RequestID)
				return nil
			})
		},
	}
}

func pruneExecuteCmd() *cobra.Command {
	var allowDeletes int
	c := &cobra.Command{
		Use:   "execute <request-id>",
		Short: "Authorize and queue a durable prune request for worker execution",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			req, err := app.DB.GetPruneRequest(args[0])
			if err != nil {
				return err
			}
			p, err := app.requireProfile(req.ProfileID)
			if err != nil {
				return err
			}
			// Serialize authorization with worker ownership (§14.4).
			return app.withProfileLock(p, func() error {
				updated, err := app.DB.AuthorizePrune(req.RequestID, allowDeletes)
				if err != nil {
					return err
				}
				if updated.State == state.PruneStatePending {
					wakeWorker(app, p.ID)
					fmt.Printf("prune #%s authorized (limit %d) and queued; worker owns execution\n",
						req.RequestID, *updated.AuthorizedLimit)
					return nil
				}
				return fmt.Errorf("prune #%s not authorized (state %s)", req.RequestID, updated.State)
			})
		},
	}
	c.Flags().IntVar(&allowDeletes, "allow-deletes", 0,
		"one-request delete ceiling override; 0 uses the profile default")
	return c
}

func pruneStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <profile-or-request>",
		Short: "Show prune request state and progress",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			// Try as a request ID first.
			if req, err := app.DB.GetPruneRequest(args[0]); err == nil {
				renderPruneRequest(req)
				return nil
			}
			p, err := app.requireProfile(args[0])
			if err != nil {
				return err
			}
			req, err := app.DB.GetActivePruneRequest(p.ID)
			if err != nil {
				return err
			}
			if req == nil {
				fmt.Printf("profile %s: no active prune request\n", p.ID)
				return nil
			}
			renderPruneRequest(req)
			return nil
		},
	}
}

func renderPruneRequest(req *state.PruneRequest) {
	fmt.Printf("request:         #%s\n", req.RequestID)
	fmt.Printf("profile:         %s\n", req.ProfileID)
	fmt.Printf("policy hash:     %s\n", req.PolicyHash)
	fmt.Printf("state:           %s\n", req.State)
	fmt.Printf("candidate count: %d\n", req.CandidateCount)
	if req.AuthorizedLimit != nil {
		fmt.Printf("authorized limit: %d\n", *req.AuthorizedLimit)
	}
	fmt.Printf("deleted:         %d\n", req.DeletedCount)
	fmt.Printf("missing:         %d\n", req.MissingCount)
	if req.LastError != nil {
		fmt.Printf("last error:      %s\n", *req.LastError)
	}
	if req.CompletedAt != nil {
		fmt.Printf("completed at:    %s\n", *req.CompletedAt)
	}
	fmt.Printf("created at:      %s\n", req.CreatedAt)
	fmt.Printf("updated at:      %s\n", req.UpdatedAt)
}
