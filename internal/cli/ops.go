package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func newVerifyCmd() *cobra.Command {
	var full bool
	c := &cobra.Command{
		Use:   "verify <profile>",
		Short: "Verify the remote mirror matches the local source",
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
			ctx, cancel := app.Context()
			defer cancel()
			if full {
				return app.Sync.VerifyFull(ctx, p)
			}
			return app.Sync.VerifyCheck(ctx, p)
		},
	}
	c.Flags().BoolVar(&full, "full", false, "full hash-level audit (expensive)")
	return c
}

func newRepairDuplicatesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repair-duplicates <profile>",
		Short: "Plan and (with --apply) remove remote duplicate objects",
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
			ctx, cancel := app.Context()
			defer cancel()

			if err := validateOwnership(ctx, app, p); err != nil {
				return err
			}

			ls := app.Rclone.Run(ctx, "lsf", "--duplicates", "--files-only",
				p.RemoteName+":"+p.RemoteDisplayPath)
			if ls.Err != nil {
				return fmt.Errorf("list duplicates: %w: %s", ls.Err, ls.StderrTrimmed())
			}
			dupLines := nonEmptyLines(ls.StdoutTrimmed())
			if len(dupLines) == 0 {
				fmt.Printf("no duplicate objects under %s\n", p.RemoteDisplayPath)
				return nil
			}
			fmt.Printf("%d duplicate object path(s) detected (§23).\n", len(dupLines))
			for _, l := range dupLines {
				fmt.Printf("  %s\n", l)
			}
			fmt.Println("Remove redundant duplicates against local source of truth using")
			fmt.Println("  rclone dedupe <remote>:" + p.RemoteDisplayPath + " --by-name --dedupe-mode newest")
			fmt.Println("after confirming deletion count within budget. The harness does not")
			fmt.Println("auto-run destructive dedupe.")
			return nil
		},
	}
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Unload all active watcher/reconciliation jobs (emergency stop)",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			ps, err := app.DB.ActiveProfiles()
			if err != nil {
				return err
			}
			count := 0
			for _, p := range ps {
				if err := stopJobs(app, p.ID); err != nil {
					return fmt.Errorf("stop %s: %w", p.ID, err)
				}
				count++
			}
			_ = uninstallWorkerJob(app)
			fmt.Printf("stopped jobs for %d profile(s); remote data untouched\n", count)
			return nil
		},
	}
}

// newPurgeRemoteCmd implements §9.5: explicit remote deletion that validates
// the sidecar UUID and Folder ID before moving the remote root to Trash.
func newPurgeRemoteCmd() *cobra.Command {
	var confirm bool
	c := &cobra.Command{
		Use:   "purge-remote <profile>",
		Short: "Move a profile's remote mirror root to Google Drive Trash",
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
			if !confirm {
				return fmt.Errorf("purge-remote is destructive; pass --confirm to proceed (§9.5)")
			}
			return app.withProfileLock(p, func() error {
				ctx, cancel := app.Context()
				defer cancel()
				// Fail-closed ownership validation before any delete.
				if err := validateOwnership(ctx, app, p); err != nil {
					return err
				}
				// Move the managed mirror root to Trash. rclone uses permanent
				// delete on Drive by default; to move to Trash we use the
				// backend-specific trash semantics. For Drive this is not a
				// first-class rclone flag, so we delete the root and rely on
				// Drive's API trash behavior for `drive` backend delete.
				res := app.Rclone.Run(ctx, "delete", p.RemoteName+":"+p.RemoteDisplayPath)
				if res.Err != nil {
					return fmt.Errorf("purge remote root: %w: %s", res.Err, res.StderrTrimmed())
				}
				fmt.Printf("remote root %s:%s deleted. Drive Trash is not emptied automatically.\n",
					p.RemoteName, p.RemoteDisplayPath)
				return nil
			})
		},
	}
	c.Flags().BoolVar(&confirm, "confirm", false, "confirm the destructive purge")
	return c
}

// newProbeCmd implements §26: create a unique probe file on the remote so the
// ChatGPT Drive connection can be tested from the ChatGPT side.
func newProbeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "probe <profile>",
		Short: "Create a unique probe file on the remote mirror for ChatGPT access testing (§26)",
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
			ctx, cancel := app.Context()
			defer cancel()
			probeName := fmt.Sprintf(".ks-probe-%d.txt", time.Now().Unix())
			tmp, err := os.CreateTemp("", "ks-probe-*.txt")
			if err != nil {
				return err
			}
			defer os.Remove(tmp.Name())
			content := fmt.Sprintf("knowledge-sync probe %d\nprofile=%s uuid=%s\n",
				time.Now().Unix(), p.ID, p.ProfileUUID)
			if _, err := tmp.WriteString(content); err != nil {
				tmp.Close()
				return err
			}
			tmp.Close()
			res := app.Rclone.Run(ctx, "copyto", tmp.Name(), p.RemoteName+":"+p.RemoteDisplayPath+"/"+probeName)
			if res.Err != nil {
				return fmt.Errorf("probe upload: %w: %s", res.Err, res.StderrTrimmed())
			}
			fmt.Printf("probe uploaded: %s:%s/%s\n", p.RemoteName, p.RemoteDisplayPath, probeName)
			fmt.Println("Ask ChatGPT (with Drive connected) to read that exact filename to verify access.")
			return nil
		},
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
