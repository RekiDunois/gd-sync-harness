package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/paths"
)

func newInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install [profile]",
		Short: "Install launchd jobs for active profiles",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			_ = paths.Ensure()

			targets := map[string]bool{}
			if len(args) == 1 {
				targets[args[0]] = true
			}
			ps, err := app.DB.ActiveProfiles()
			if err != nil {
				return err
			}
			installed := 0
			for _, p := range ps {
				if len(targets) > 0 && !targets[p.ID] {
					continue
				}
				if !p.Enabled {
					continue
				}
				if err := installJobs(app, p); err != nil {
					return fmt.Errorf("install %s: %w", p.ID, err)
				}
				installed++
			}
			fmt.Printf("installed launchd jobs for %d profile(s)\n", installed)
			return nil
		},
	}
}

func newUninstallCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "uninstall [profile]",
		Short: "Remove launchd jobs (unload + delete plists)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()

			var ids []string
			if len(args) == 1 {
				ids = []string{args[0]}
			} else if all {
				ps, err := app.DB.ListProfiles()
				if err != nil {
					return err
				}
				for _, p := range ps {
					ids = append(ids, p.ID)
				}
			} else {
				return fmt.Errorf("specify a profile or --all")
			}
			for _, id := range ids {
				if err := uninstallJobs(app, id); err != nil {
					return fmt.Errorf("uninstall %s: %w", id, err)
				}
				fmt.Printf("uninstalled %s\n", id)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "uninstall jobs for all profiles")
	return c
}
