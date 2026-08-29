package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/state"
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
	health := computeHealth(app, p, rt)
	pending, _ := app.DB.CountPending(p.ID)
	manifestCount, _ := app.DB.ManifestCount(p.ID)

	fmt.Printf("%s  [%s]\n", p.ID, health)
	fmt.Printf("  type:              %s\n", p.Type)
	fmt.Printf("  source:            %s\n", p.SourcePath)
	fmt.Printf("  remote:            %s:%s (folder %s)\n", p.RemoteName, p.RemoteDisplayPath, p.RemoteFolderID)
	fmt.Printf("  enabled:           %t\n", p.Enabled)
	fmt.Printf("  watcher:           %s\n", rt.WatcherStatus)
	fmt.Printf("  pending events:    %d\n", pending)
	fmt.Printf("  manifest entries:  %d\n", manifestCount)
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

func computeHealth(app *App, p *state.Profile, rt *state.Runtime) string {
	if p.Tombstoned {
		return "TOMBSTONED"
	}
	if !p.Enabled {
		return "DISABLED"
	}
	if rt == nil {
		return "STALE"
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
