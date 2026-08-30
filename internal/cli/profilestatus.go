package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/state"
	"knowledge-sync/pkg/version"
)

func profileStatusCmd() *cobra.Command {
	var watch bool
	c := &cobra.Command{
		Use:   "status <id>",
		Short: "Show durable sync state and progress for a profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			if _, err := app.requireProfile(args[0]); err != nil {
				return err
			}
			if err := renderProfileSyncStatus(app, args[0]); err != nil {
				return err
			}
			if watch {
				return watchProfileSyncStatus(app, args[0])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&watch, "watch", false, "observe until ready or a blocking terminal condition (§10.2)")
	return c
}

// renderProfileSyncStatus prints the durable sync state. A successful one-shot
// query exits 0 even when the reported sync state is error (§10.1).
func renderProfileSyncStatus(app *App, id string) error {
	p, err := app.DB.GetProfile(id)
	if err != nil {
		return err
	}
	ss, err := app.DB.GetSyncState(id)
	if err != nil {
		return err
	}
	if p.DeletionRequestedAt != nil {
		ss.State = "deleting"
	} else if p.Tombstoned {
		ss.State = "deleted"
	} else if !p.Enabled {
		ss.State = "disabled"
	}

	initStr := "no"
	if ss.IsInitialized() {
		initStr = "yes"
	}

	fmt.Printf("Profile: %s\n", p.ID)
	fmt.Printf("CLI binary: %s\n", version.String())
	fmt.Printf("Initialized: %s\n", initStr)
	fmt.Printf("State:   %s\n", ss.State)
	if ss.Phase != "" {
		fmt.Printf("Phase:   %s\n", ss.Phase)
	}
	if ss.IsInitialized() && ss.InitializedAt != nil {
		fmt.Printf("Initialized at: %s\n", *ss.InitializedAt)
	}
	if ss.LastSuccessAt != nil {
		fmt.Printf("Last successful sync: %s\n", *ss.LastSuccessAt)
	}
	if ss.State == state.StateError {
		retry := "no"
		if ss.RetryClassification != nil && *ss.RetryClassification == state.RetryRetryable {
			retry = "yes"
		}
		fmt.Printf("Retryable: %s\n", retry)
		if ss.NextRetryAt != nil {
			if t, err := parseTime(*ss.NextRetryAt); err == nil {
				remaining := time.Until(t)
				if remaining <= 0 {
					fmt.Printf("Next retry: due now\n")
				} else {
					fmt.Printf("Next retry: %s\n", humanDuration(remaining))
				}
			} else {
				fmt.Printf("Next retry: %s\n", *ss.NextRetryAt)
			}
		}
		if ss.LastError != nil {
			fmt.Printf("Last error: %s\n", *ss.LastError)
		}
	}

	if run := currentRun(app, id, ss); run != nil {
		fmt.Printf("Run:     %s (%s, generation %d)\n", run.ID, run.Kind, run.TargetGeneration)
		if run.FilesDiscovered > 0 {
			fmt.Printf("Local discovered: %d\n", run.FilesDiscovered)
		}
		if run.FilesCompleted > 0 {
			fmt.Printf("Transferred files: %d\n", run.FilesCompleted)
		}
		if run.Phase == state.PhaseUploading {
			if run.UploadStartedAt != nil {
				if t, err := parseTime(*run.UploadStartedAt); err == nil {
					minutes := time.Since(t).Minutes()
					if minutes > 0 {
						fmt.Printf("Files/min:          %.1f\n", float64(run.FilesCompleted)/minutes)
					}
				}
			}
			fmt.Printf("Active transfers:   %d\n", run.ActiveTransfers)
		}
		fmt.Printf("Listed: %d\n", run.ItemsListed)
		fmt.Printf("Checked: %d", run.ChecksCompleted)
		if run.ChecksTotal > 0 {
			fmt.Printf(" / %d", run.ChecksTotal)
		}
		fmt.Println()
		if run.BytesCompleted > 0 {
			total := "calculating"
			if run.BytesTotal > 0 {
				total = humanBytes(run.BytesTotal)
			}
			fmt.Printf("Transferred:      %s / %s\n", humanBytes(run.BytesCompleted), total)
		}
		if run.SpeedBytesPerSecond > 0 {
			fmt.Printf("Speed:             %s/s\n", humanBytes(int64(run.SpeedBytesPerSecond)))
		}
		if run.CurrentItem != nil {
			fmt.Printf("Current:           %s (%s / %s)\n", *run.CurrentItem,
				humanBytes(run.CurrentItemBytes), humanBytes(run.CurrentItemSize))
		}
		if t, err := parseTime(run.StartedAt); err == nil {
			fmt.Printf("Started: %s ago\n", humanDuration(time.Since(t)))
		}
	}
	if ss.LastProgressAt != nil {
		if t, err := parseTime(*ss.LastProgressAt); err == nil {
			fmt.Printf("Last progress: %s ago\n", humanDuration(time.Since(t)))
		}
	}
	if ss.LastHeartbeatAt != nil {
		if t, err := parseTime(*ss.LastHeartbeatAt); err == nil {
			fmt.Printf("Last heartbeat: %s ago\n", humanDuration(time.Since(t)))
		}
	}
	if run := currentRun(app, id, ss); run != nil && ss.LastProgressAt != nil {
		if t, err := parseTime(*ss.LastProgressAt); err == nil && time.Since(t) >= 30*time.Minute {
			fmt.Println("Warning: possible stall")
			fmt.Println("No measurable progress for 30m; rclone was not cancelled")
		}
	}
	return nil
}

func currentRun(app *App, id string, ss *state.ProfileSyncState) *state.SyncRun {
	if ss.CurrentRunID == nil {
		return nil
	}
	run, err := app.DB.GetRun(*ss.CurrentRunID)
	if err != nil {
		return nil
	}
	return run
}

func watchProfileSyncStatus(app *App, id string) error {
	for {
		ss, err := app.DB.GetSyncState(id)
		if err != nil {
			return err
		}
		p, err := app.DB.GetProfile(id)
		if err != nil {
			return err
		}
		if err := renderProfileSyncStatus(app, id); err != nil {
			return err
		}
		switch {
		case ss.State == state.StateReady && !p.Tombstoned && p.DeletionRequestedAt == nil:
			return nil
		case p.DeletionRequestedAt != nil || p.Tombstoned:
			return fmt.Errorf("profile %q is deleted or being deleted", id)
		case ss.State == state.StateError:
			if ss.RetryClassification != nil && *ss.RetryClassification == state.RetryTerminal {
				return fmt.Errorf("profile %q sync blocked by terminal error: %s", id, stringOr(ss.LastError, "unknown"))
			}
		}
		time.Sleep(waiterPollInterval)
	}
}

func parseTime(s string) (time.Time, error) {
	return time.Parse("2006-01-02T15:04:05.000Z07:00", s)
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
