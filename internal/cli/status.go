package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

func newStatusCmd() *cobra.Command {
	var refreshQuota bool
	c := &cobra.Command{
		Use:   "status [profile]",
		Short: "Show sync and health status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()

			if refreshQuota {
				ctx, cancel := app.Context()
				defer cancel()
				ps, _ := app.DB.ActiveProfiles()
				seen := map[string]bool{}
				for _, p := range ps {
					if seen[p.RemoteName] {
						continue
					}
					seen[p.RemoteName] = true
					app.Remote.CheckQuota(ctx, p.RemoteName, 2<<30)
				}
			}

			if len(args) == 1 {
				p, err := app.requireProfile(args[0])
				if err != nil {
					return err
				}
				return renderProfileStatus(app, p)
			}
			ps, err := app.DB.ActiveProfiles()
			if err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Println("no active profiles")
				return nil
			}
			for _, p := range ps {
				if err := renderProfileStatus(app, p); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&refreshQuota, "refresh-quota", false, "re-query Google Drive quota before rendering")
	return c
}

func renderProfileStatus(app *App, p *state.Profile) error {
	rt, err := app.DB.GetRuntime(p.ID)
	if err != nil {
		return err
	}
	ss, _ := app.DB.GetSyncState(p.ID)
	health := computeHealth(app, p, rt, ss)
	pending, _ := app.DB.CountPending(p.ID)
	manifestCount, _ := app.DB.ManifestCount(p.ID)

	// §10.4: report skipped symlinks and oversize files in status.
	scan, scanErr := sync.ScanLocal(p)

	fmt.Printf("%s  [%s]\n", p.ID, health)
	fmt.Printf("  type:              %s\n", p.Type)
	fmt.Printf("  source:            %s\n", p.SourcePath)
	fmt.Printf("  remote:            %s:%s (folder %s)\n", p.RemoteName, p.RemoteDisplayPath, p.RemoteFolderID)
	fmt.Printf("  enabled:           %t\n", p.Enabled)
	fmt.Printf("  watcher:           %s\n", rt.WatcherStatus)
	fmt.Printf("  pending events:    %d\n", pending)
	fmt.Printf("  manifest entries:  %d\n", manifestCount)
	if ss != nil {
		fmt.Printf("  sync state:        %s", ss.State)
		if ss.Phase != "" {
			fmt.Printf(" (%s)", ss.Phase)
		}
		fmt.Println()
		if ss.IsInitialized() && ss.InitializedAt != nil {
			fmt.Printf("  initialized:       %s\n", *ss.InitializedAt)
		}
		if ss.LastSuccessAt != nil {
			fmt.Printf("  last successful:   %s\n", *ss.LastSuccessAt)
		}
		if ss.CurrentRunID != nil {
			fmt.Printf("  active run:        %s\n", *ss.CurrentRunID)
		}
	}
	if scanErr == nil {
		fmt.Printf("  local files:       %d (symlinks skipped: %d, oversize skipped: %d)\n",
			len(scan.Entries), len(scan.SkippedSymlinks), len(scan.SkippedOversize))
	}
	fmt.Printf("  last fast sync:    %s\n", nullOr(rt.LastFastSuccess))
	fmt.Printf("  last reconcile:    %s\n", nullOr(rt.LastReconcileSuccess))
	fmt.Printf("  last error:        %s\n", nullOr(rt.LastError))
	if rt.ReconcileRequested {
		fmt.Println("  reconcile:         REQUESTED")
	}
	if q, err := app.DB.GetRemote(p.RemoteName); err == nil && q.LastQuotaCheck != "" {
		fmt.Printf("  quota %s:         %s used / %s total / %s free [%s]\n",
			p.RemoteName, humanBytes(q.UsedBytes), humanBytes(q.TotalBytes), humanBytes(q.FreeBytes), q.QuotaStatus)
	}
	if scanErr == nil && len(scan.SkippedSymlinks) > 0 {
		fmt.Println("  skipped symlinks (§10.4):")
		for i, s := range scan.SkippedSymlinks {
			if i >= 10 {
				fmt.Printf("    ... and %d more\n", len(scan.SkippedSymlinks)-10)
				break
			}
			fmt.Printf("    %s\n", s)
		}
	}
	return nil
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func computeHealth(app *App, p *state.Profile, rt *state.Runtime, ss *state.ProfileSyncState) string {
	if p.Tombstoned {
		return "TOMBSTONED"
	}
	if p.DeletionRequestedAt != nil {
		return "DELETING"
	}
	if !p.Enabled {
		return "DISABLED"
	}
	if rt == nil {
		return "STALE"
	}
	if ss != nil {
		switch ss.State {
		case state.StateInitializing:
			return "INITIALIZING"
		case state.StateSyncing:
			return "SYNCING"
		case state.StateError:
			return "BROKEN"
		}
	}
	if rt.ReconcileRequested {
		return "RECONCILE_REQUESTED"
	}
	const staleAfter = 26 * time.Hour
	if rt.LastReconcileSuccess != nil {
		t, err := time.Parse("2006-01-02T15:04:05.000Z07:00", *rt.LastReconcileSuccess)
		if err == nil && time.Since(t) > staleAfter {
			return "STALE"
		}
	}
	if rt.LastError != nil && *rt.LastError != "" {
		return "BROKEN"
	}
	if q, err := app.DB.GetRemote(p.RemoteName); err == nil {
		if q.QuotaStatus == state.QuotaLow {
			return "QUOTA_LOW"
		}
		if q.QuotaStatus == state.QuotaFull {
			return "QUOTA_FULL"
		}
	}
	return "HEALTHY"
}
