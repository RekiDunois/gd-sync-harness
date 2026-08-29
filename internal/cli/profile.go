package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/profile"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
)

func newProfileCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "profile",
		Short: "Manage sync profiles",
	}
	c.AddCommand(
		profileAddCmd(),
		profileShowCmd(),
		profileListCmd(),
		profileDisableCmd(),
		profileEnableCmd(),
		profileRemoveCmd(),
		profileRestoreCmd(),
		profileForgetCmd(),
		profileMigrateCmd(),
		profileExcludeCmd(),
	)
	return c
}

func profileAddCmd() *cobra.Command {
	var (
		typ         string
		maxDelete   int
		maxFileSize int64
		dryRun      bool
	)
	c := &cobra.Command{
		Use:   "add <id> <source> <remote> <remote-path>",
		Short: "Add a new sync profile (creates managed Drive root + sidecar)",
		Args:  cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			svc := profile.NewService(app.DB, app.Rclone, app.Remote)
			ctx, cancel := app.Context()
			defer cancel()

			if _, err := app.DB.GetProfile(args[0]); err == nil {
				return state.ErrIDExists
			}

			p, err := svc.Add(ctx, profile.AddOptions{
				ID: args[0], SourcePath: args[1], RemoteName: args[2],
				RemotePath: args[3], Type: typ, MaxDelete: maxDelete,
				MaxFileSize: maxFileSize, DryRun: dryRun,
			})
			if err != nil {
				return err
			}
			backupNow(app)
			fmt.Printf("added profile %s (uuid %s)\n", p.ID, p.ProfileUUID)
			fmt.Printf("  source:   %s\n", p.SourcePath)
			fmt.Printf("  remote:   %s:%s\n", p.RemoteName, p.RemoteDisplayPath)
			fmt.Printf("  folder:   %s\n", p.RemoteFolderID)
			fmt.Printf("  type:     %s\n", p.Type)
			fmt.Printf("  max_delete: %d\n", p.MaxDelete)
			return nil
		},
	}
	c.Flags().StringVar(&typ, "type", "generic", "profile type: obsidian | generic")
	c.Flags().IntVar(&maxDelete, "max-delete", 0, "per-profile deletion budget (default 100)")
	c.Flags().Int64Var(&maxFileSize, "max-file-size", 0, "max file size in bytes (default 512 MiB; 0 = unlimited)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "validate only; do not create remote root or sidecar")
	return c
}

func profileShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show profile details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			p, err := app.DB.GetProfile(args[0])
			if err != nil {
				return err
			}
			rt, _ := app.DB.GetRuntime(args[0])
			fmt.Printf("id:                 %s\n", p.ID)
			fmt.Printf("uuid:               %s\n", p.ProfileUUID)
			fmt.Printf("type:               %s\n", p.Type)
			fmt.Printf("source:             %s\n", p.SourcePath)
			fmt.Printf("remote:             %s\n", p.RemoteName)
			fmt.Printf("remote path:        %s\n", p.RemoteDisplayPath)
			fmt.Printf("folder id:          %s\n", p.RemoteFolderID)
			fmt.Printf("enabled:            %t\n", p.Enabled)
			fmt.Printf("tombstoned:         %t\n", p.Tombstoned)
			fmt.Printf("max_delete:         %d\n", p.MaxDelete)
			fmt.Printf("max_file_size:      %d\n", p.MaxFileSize)
			if rt != nil {
				fmt.Printf("source_generation:  %d\n", rt.SourceGeneration)
				fmt.Printf("reconcile_requested:%t\n", rt.ReconcileRequested)
				fmt.Printf("last_fast_success:  %s\n", nullOr(rt.LastFastSuccess))
				fmt.Printf("last_reconcile:     %s\n", nullOr(rt.LastReconcileSuccess))
				fmt.Printf("watcher_status:     %s\n", rt.WatcherStatus)
				fmt.Printf("last_error:         %s\n", nullOr(rt.LastError))
			}
			if len(p.Excludes) > 0 {
				fmt.Printf("excludes:           %d rules\n", len(p.Excludes))
				for _, e := range p.Excludes {
					fmt.Printf("                     %s\n", e)
				}
			}
			return nil
		},
	}
}

func profileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			ps, err := app.DB.ListProfiles()
			if err != nil {
				return err
			}
			if len(ps) == 0 {
				fmt.Println("no profiles")
				return nil
			}
			fmt.Printf("%-24s %-8s %-10s %-10s %s\n", "ID", "TYPE", "STATE", "ENABLED", "REMOTE")
			for _, p := range ps {
				stateStr := "active"
				if p.Tombstoned {
					stateStr = "tombstoned"
				}
				en := "no"
				if p.Enabled {
					en = "yes"
				}
				fmt.Printf("%-24s %-8s %-10s %-10s %s:%s\n", p.ID, p.Type, stateStr, en, p.RemoteName, p.RemoteDisplayPath)
			}
			return nil
		},
	}
}

func profileDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <id>",
		Short: "Disable a profile (stops jobs, keeps data)",
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
			return app.withProfileLock(p, func() error {
				if err := stopJobs(app, p.ID); err != nil {
					return err
				}
				if err := app.DB.SetProfileEnabled(p.ID, false); err != nil {
					return err
				}
				backupNow(app)
				return nil
			})
		},
	}
}

func profileEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <id>",
		Short: "Enable a profile (installs jobs)",
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
			return app.withProfileLock(p, func() error {
				if err := app.DB.SetProfileEnabled(p.ID, true); err != nil {
					return err
				}
				if err := startJobs(app, p); err != nil {
					return err
				}
				backupNow(app)
				return nil
			})
		},
	}
}

func profileRemoveCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a profile (tombstone; keeps remote data)",
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
			return app.withProfileLock(p, func() error {
				if err := stopJobs(app, p.ID); err != nil {
					if !force {
						return err
					}
				}
				if err := app.DB.ClearPending(p.ID); err != nil {
					return err
				}
				if err := app.DB.TombstoneProfile(p.ID); err != nil {
					return err
				}
				backupNow(app)
				return nil
			})
		},
	}
	c.Flags().BoolVar(&force, "force", false, "tombstone even if job uninstall fails")
	return c
}

func profileRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <id>",
		Short: "Restore a tombstoned profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			p, err := app.DB.GetProfile(args[0])
			if err != nil {
				return err
			}
			if !p.Tombstoned {
				return state.ErrNotTombstoned
			}
			if _, err := os.Stat(p.SourcePath); err != nil {
				return fmt.Errorf("source missing: %w", err)
			}
			if err := app.DB.RestoreProfile(p.ID); err != nil {
				return err
			}
			backupNow(app)
			fmt.Printf("restored profile %s (run 'profile enable %s' to start jobs)\n", p.ID, p.ID)
			return nil
		},
	}
}

func profileForgetCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "forget <id>",
		Short: "Permanently delete a tombstoned profile so the ID can be reused",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			p, err := app.DB.GetProfile(args[0])
			if err != nil {
				return err
			}
			if !p.Tombstoned {
				return fmt.Errorf("profile %q is not tombstoned (use 'profile remove' first)", args[0])
			}
			_ = force
			if err := app.DB.ForgetProfile(p.ID); err != nil {
				return err
			}
			backupNow(app)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "skip confirmation")
	return c
}

func profileMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate <id> <new-remote> <new-remote-path>",
		Short: "Migrate a profile to a new storage owner (copy, verify, cutover)",
		Args:  cobra.ExactArgs(3),
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
			return app.withProfileLock(p, func() error {
				return runMigrate(app, p, args[1], args[2])
			})
		},
	}
}

func profileExcludeCmd() *cobra.Command {
	var remove bool
	c := &cobra.Command{
		Use:   "exclude <id> <rule-type> <rule-value>",
		Short: "Add or remove an exclude rule (exclude_path_prefix|exclude_dir_name|exclude_filename|exclude_extension)",
		Args:  cobra.ExactArgs(3),
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
			ruleType := args[1]
			if !state.ValidRuleType(ruleType) {
				return fmt.Errorf("invalid rule type %q", ruleType)
			}
			if remove {
				if err := app.DB.RemoveExclude(p.ID, ruleType, args[2]); err != nil {
					return err
				}
			} else {
				if err := app.DB.AddExclude(p.ID, ruleType, args[2]); err != nil {
					return err
				}
			}
			_ = app.DB.RequestReconcile(p.ID)
			action := "added"
			if remove {
				action = "removed"
			}
			fmt.Printf("exclude rule %s (%s:%s); full reconciliation requested\n", action, ruleType, args[2])
			return nil
		},
	}
	c.Flags().BoolVar(&remove, "remove", false, "remove the rule instead of adding")
	return c
}

func nullOr(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// runMigrate implements §22: copy, verify (size-only), cutover, trash old.
func runMigrate(app *App, p *state.Profile, newRemote, newPath string) error {
	ctx, cancel := app.Context()
	defer cancel()

	if _, err := app.Remote.ValidateRemote(ctx, newRemote); err != nil {
		return err
	}
	newFolderID, err := app.Remote.CreateManagedRoot(ctx, newRemote, newPath)
	if err != nil {
		return err
	}

	newP := *p
	newP.RemoteName = newRemote
	newP.RemoteDisplayPath = newPath
	newP.RemoteFolderID = newFolderID
	if err := writeSidecar(app, &newP); err != nil {
		return err
	}

	copyArgs := []string{
		"copy", p.SourcePath, newRemote + ":" + newPath,
		"--fast-list", "--transfers", "4",
	}
	res := app.Rclone.Run(ctx, copyArgs...)
	if res.Err != nil {
		return fmt.Errorf("migration copy: %w: %s", res.Err, res.StderrTrimmed())
	}

	if err := app.Sync.VerifyCheck(ctx, &newP); err != nil {
		return fmt.Errorf("migration verify: %w", err)
	}

	if err := app.DB.UpdateProfileFields(&newP); err != nil {
		return err
	}
	backupNow(app)

	pre, err := app.Reconciler.Reconcile(ctx, &newP, sync.SyncOptions{})
	if err != nil {
		return fmt.Errorf("post-migration reconcile: %w", err)
	}
	_ = pre

	fmt.Printf("migration complete. Old remote root %s:%s was left in place;\n", p.RemoteName, p.RemoteDisplayPath)
	fmt.Printf("move it to Trash manually (harness does not auto-empty Trash).\n")
	return nil
}

func writeSidecar(app *App, p *state.Profile) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return writeSidecarCtx(ctx, app, p)
}
