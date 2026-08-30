package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/live"
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
			if watch {
				return watchProfileSyncStatus(app, args[0])
			}
			return renderProfileSyncStatus(app, args[0])
		},
	}
	c.Flags().BoolVar(&watch, "watch", false, "observe until ready or a blocking terminal condition (§10.2)")
	return c
}

// socketObserver builds the unified live observer for a profile using the
// persisted socket setting. If the socket is unavailable, the caller falls back
// to SQLite and retries periodically.
func socketObserver(app *App, id string) *live.Observer {
	configured, _ := app.DB.GetSetting(state.SettingWorkerSocketPath)
	return &live.Observer{Path: live.ResolveSocketPath(configured), ProfileID: id}
}

// renderProfileSyncStatus prints durable sync state. When live telemetry is
// available it renders the live snapshot (including Speed); otherwise it falls
// back to SQLite and omits Speed and the live stall diagnosis (§9.4).
func renderProfileSyncStatus(app *App, id string) error {
	snapshot, err := tryOneShotStatus(app, id)
	if err == nil {
		return renderStatusSnapshot(snapshot)
	}
	return renderProfileSyncStatusDB(app, id)
}

// tryOneShotStatus subscribes, renders the initial full snapshot, and closes.
// It never sleeps for a telemetry tick (§14.1).
func tryOneShotStatus(app *App, id string) (*live.StatusSnapshot, error) {
	obs := socketObserver(app, id)
	stream, err := obs.Connect()
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	snap, err := stream.Next()
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// renderStatusSnapshot renders a live worker snapshot (§5.2, §14.1).
func renderStatusSnapshot(s *live.StatusSnapshot) error {
	fmt.Fprintf(out(), "Profile: %s\n", s.ProfileID)
	fmt.Fprintf(out(), "CLI binary: %s\n", version.String())
	initStr := "no"
	if s.Sync.Initialized {
		initStr = "yes"
	}
	stateStr := s.Sync.State
	if s.Profile.DeletionRequested {
		stateStr = "deleting"
	} else if s.Profile.Tombstoned {
		stateStr = "deleted"
	} else if !s.Profile.Enabled {
		stateStr = "disabled"
	}
	fmt.Fprintf(out(), "Initialized: %s\n", initStr)
	fmt.Fprintf(out(), "State:   %s\n", stateStr)
	if s.Sync.Phase != nil {
		fmt.Fprintf(out(), "Phase:   %s\n", *s.Sync.Phase)
	}
	if s.Sync.LastSuccessAt != nil {
		fmt.Fprintf(out(), "Last successful sync: %s\n", s.Sync.LastSuccessAt.Format(time.RFC3339))
	}
	if stateStr == state.StateError && s.Sync.LastError != nil {
		fmt.Fprintf(out(), "Last error: %s\n", *s.Sync.LastError)
	}

	if act := s.Activity; act != nil {
		runID := ""
		if act.RunID != nil {
			runID = *act.RunID
		}
		fmt.Fprintf(out(), "Run:     %s (%s)\n", runID, act.Kind)
		if act.FilesCompleted > 0 {
			fmt.Fprintf(out(), "Transferred files: %d\n", act.FilesCompleted)
		}
		if act.ActiveTransfers > 0 {
			fmt.Fprintf(out(), "Active transfers:   %d\n", act.ActiveTransfers)
		}
		fmt.Fprintf(out(), "Listed: %d\n", act.ItemsListed)
		fmt.Fprintf(out(), "Checked: %d", act.ChecksCompleted)
		if act.ChecksTotal > 0 {
			fmt.Fprintf(out(), " / %d", act.ChecksTotal)
		}
		fmt.Fprintln(out())
		if act.BytesCompleted > 0 {
			total := "calculating"
			if act.BytesTotal > 0 {
				total = humanBytes(act.BytesTotal)
			}
			fmt.Fprintf(out(), "Transferred:      %s / %s\n", humanBytes(act.BytesCompleted), total)
		}
		// Live speed is only rendered while in a transfer phase (§0.1).
		if act.SpeedKnown && transferPhase(act.Phase) {
			fmt.Fprintf(out(), "Speed:             %s/s\n", humanBytes(int64(act.SpeedBytesPerSecond)))
		}
		if act.CurrentItem != "" {
			fmt.Fprintf(out(), "Current:           %s (%s / %s)\n", act.CurrentItem,
				humanBytes(act.CurrentItemBytes), humanBytes(act.CurrentItemSize))
		}
		if act.PossibleStall {
			fmt.Fprintln(out(), "Warning: possible stall")
			fmt.Fprintln(out(), "No measurable progress for 30m; rclone was not cancelled")
		}
	}
	return nil
}

// renderProfileSyncStatusDB is the SQLite fallback (§9.4). It omits Speed and
// the live stall diagnosis and labels coarse counters as such.
func renderProfileSyncStatusDB(app *App, id string) error {
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

	fmt.Fprintf(out(), "Profile: %s\n", p.ID)
	fmt.Fprintf(out(), "CLI binary: %s\n", version.String())
	fmt.Fprintf(out(), "Initialized: %s\n", initStr)
	fmt.Fprintf(out(), "State:   %s\n", ss.State)
	if ss.Phase != "" {
		fmt.Fprintf(out(), "Phase:   %s\n", ss.Phase)
	}
	if ss.IsInitialized() && ss.InitializedAt != nil {
		fmt.Fprintf(out(), "Initialized at: %s\n", *ss.InitializedAt)
	}
	if ss.LastSuccessAt != nil {
		fmt.Fprintf(out(), "Last successful sync: %s\n", *ss.LastSuccessAt)
	}
	if ss.State == state.StateError {
		retry := "no"
		if ss.RetryClassification != nil && *ss.RetryClassification == state.RetryRetryable {
			retry = "yes"
		}
		fmt.Fprintf(out(), "Retryable: %s\n", retry)
		if ss.NextRetryAt != nil {
			if t, err := parseTime(*ss.NextRetryAt); err == nil {
				remaining := time.Until(t)
				if remaining <= 0 {
					fmt.Fprintf(out(), "Next retry: due now\n")
				} else {
					fmt.Fprintf(out(), "Next retry: %s\n", humanDuration(remaining))
				}
			} else {
				fmt.Fprintf(out(), "Next retry: %s\n", *ss.NextRetryAt)
			}
		}
		if ss.LastError != nil {
			fmt.Fprintf(out(), "Last error: %s\n", *ss.LastError)
		}
	}

	if run := currentRun(app, id, ss); run != nil {
		fmt.Fprintf(out(), "Run:     %s (%s, generation %d)\n", run.ID, run.Kind, run.TargetGeneration)
		if run.FilesDiscovered > 0 {
			fmt.Fprintf(out(), "Local discovered: %d\n", run.FilesDiscovered)
		}
		if run.FilesCompleted > 0 {
			fmt.Fprintf(out(), "Transferred files: %d\n", run.FilesCompleted)
		}
		if run.Phase == state.PhaseUploading {
			if run.UploadStartedAt != nil {
				if t, err := parseTime(*run.UploadStartedAt); err == nil {
					minutes := time.Since(t).Minutes()
					if minutes > 0 {
						fmt.Fprintf(out(), "Files/min:          %.1f\n", float64(run.FilesCompleted)/minutes)
					}
				}
			}
			fmt.Fprintf(out(), "Active transfers:   %d\n", run.ActiveTransfers)
		}
		fmt.Fprintf(out(), "Listed: %d\n", run.ItemsListed)
		fmt.Fprintf(out(), "Checked: %d", run.ChecksCompleted)
		if run.ChecksTotal > 0 {
			fmt.Fprintf(out(), " / %d", run.ChecksTotal)
		}
		fmt.Fprintln(out())
		if run.BytesCompleted > 0 {
			total := "calculating"
			if run.BytesTotal > 0 {
				total = humanBytes(run.BytesTotal)
			}
			fmt.Fprintf(out(), "Transferred:      %s / %s (coarse snapshot)\n", humanBytes(run.BytesCompleted), total)
		}
		if run.CurrentItem != nil {
			fmt.Fprintf(out(), "Current:           %s (%s / %s)\n", *run.CurrentItem,
				humanBytes(run.CurrentItemBytes), humanBytes(run.CurrentItemSize))
		}
		if t, err := parseTime(run.StartedAt); err == nil {
			fmt.Fprintf(out(), "Started: %s ago\n", humanDuration(time.Since(t)))
		}
	}
	if ss.LastProgressAt != nil {
		if t, err := parseTime(*ss.LastProgressAt); err == nil {
			fmt.Fprintf(out(), "Last progress: %s ago (coarse checkpoint)\n", humanDuration(time.Since(t)))
		}
	}
	if ss.LastHeartbeatAt != nil {
		if t, err := parseTime(*ss.LastHeartbeatAt); err == nil {
			fmt.Fprintf(out(), "Last heartbeat: %s ago\n", humanDuration(time.Since(t)))
		}
	}
	fmt.Fprintln(out(), "(live worker telemetry unavailable; showing durable state)")
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

// watchProfileSyncStatus observes until ready or a blocking terminal condition
// (§14.2). It is socket-first; on disconnect it falls back to SQLite and keeps
// retrying the socket at the observer poll interval.
func watchProfileSyncStatus(app *App, id string) error {
	obs := socketObserver(app, id)
	for {
		stream, err := obs.Connect()
		if err != nil {
			// Socket unavailable: fall back to DB and retry the socket later.
			term, derr := observeDurable(app, id)
			if derr != nil {
				return derr
			}
			if term {
				return nil
			}
			time.Sleep(waiterPollInterval)
			continue
		}
		term, derr := observeStream(stream)
		stream.Close()
		if derr == nil && term {
			return nil
		}
		// Socket dropped or protocol error: DB fallback then retry.
		time.Sleep(waiterPollInterval)
	}
}

// observeStream consumes snapshots, rendering each, until a terminal condition.
func observeStream(stream *live.Stream) (bool, error) {
	for {
		snap, err := stream.Next()
		if err != nil {
			return false, err
		}
		if err := renderStatusSnapshot(snap); err != nil {
			return false, err
		}
		switch {
		case snap.Sync.State == state.StateReady && !snap.Profile.Tombstoned && !snap.Profile.DeletionRequested:
			return true, nil
		case snap.Profile.DeletionRequested || snap.Profile.Tombstoned:
			return false, fmt.Errorf("profile %q is deleted or being deleted", snap.ProfileID)
		case snap.Sync.State == state.StateError:
			if snap.Sync.RetryClassification != nil && *snap.Sync.RetryClassification == state.RetryTerminal {
				return false, fmt.Errorf("profile %q sync blocked by terminal error: %s", snap.ProfileID, stringOr(snap.Sync.LastError, "unknown"))
			}
		}
	}
}

// observeDurable is the SQLite fallback for watch mode (§9.4, §14.2). It renders
// the durable snapshot and returns whether a terminal success condition is met.
func observeDurable(app *App, id string) (bool, error) {
	if err := renderProfileSyncStatusDB(app, id); err != nil {
		return false, err
	}
	ss, err := app.DB.GetSyncState(id)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return false, fmt.Errorf("profile %q is no longer present (deleted)", id)
		}
		return false, err
	}
	p, err := app.DB.GetProfile(id)
	if err != nil {
		return false, err
	}
	switch {
	case ss.State == state.StateReady && !p.Tombstoned && p.DeletionRequestedAt == nil:
		return true, nil
	case p.DeletionRequestedAt != nil || p.Tombstoned:
		return false, fmt.Errorf("profile %q is deleted or being deleted", id)
	case ss.State == state.StateError:
		if ss.RetryClassification != nil && *ss.RetryClassification == state.RetryTerminal {
			return false, fmt.Errorf("profile %q sync blocked by terminal error: %s", id, stringOr(ss.LastError, "unknown"))
		}
	}
	return false, nil
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
