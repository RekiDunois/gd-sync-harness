package cli

import (
	"fmt"

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
			fmt.Printf("stopped jobs for %d profile(s); remote data untouched\n", count)
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
