package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/state"
)

// newSyncCmd implements §18: the explicit control-plane request to reconcile
// now. It submits durable manual intent (which reopens terminal/retry gates and
// bypasses the destructive debounce for a one-attempt opportunity) and wakes the
// worker; it never executes a competing transfer inside the CLI process.
func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync <profile>",
		Short: "Request reconciliation for a profile now (worker-owned execution)",
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
			if p.DeletionRequestedAt != nil {
				return fmt.Errorf("profile %q is being deleted; sync request rejected (§18.4)", p.ID)
			}
			// Submit durable manual intent and wake the worker.
			if _, err := app.DB.SubmitManualReconcile(p.ID, state.ManualReconcileIntent{
				BypassDebounce: true,
			}); err != nil {
				return err
			}
			wakeWorker(app, p.ID)
			backupNow(app)
			fmt.Printf("Reconciliation requested for %q.\n", p.ID)
			fmt.Printf("The worker owns execution; check progress with:\n")
			fmt.Printf("  knowledge-sync profile status %s\n", p.ID)
			return nil
		},
	}
}
