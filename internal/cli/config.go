package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/launchd"
	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

// newConfigCmd exposes the persisted settings surface, including the worker
// socket-path configuration (§4.5).
func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage persisted worker settings",
	}
	c.AddCommand(
		configGetCmd(),
		configSetCmd(),
		configUnsetCmd(),
	)
	return c
}

// configGetCmd prints the socket-path setting and the resolved effective path.
func configGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get socket-path",
		Short: "Show the socket-path setting and resolved effective path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "socket-path" {
				return fmt.Errorf("unsupported config key %q (only socket-path is exposed)", args[0])
			}
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			configured, err := app.DB.GetSetting(state.SettingWorkerSocketPath)
			if err != nil {
				return err
			}
			if configured == "" {
				fmt.Println("socket-path: (not set)")
			} else {
				fmt.Printf("socket-path: %s\n", configured)
			}
			fmt.Printf("effective socket-path: %s\n", live.ResolveSocketPath(configured))
			return nil
		},
	}
}

// configSetCmd persists a socket-path override and restarts the managed worker
// so worker and clients do not remain on different paths (§15.3).
func configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set socket-path <path>",
		Short: "Persist a worker socket path override (restarts the managed worker)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "socket-path" {
				return fmt.Errorf("unsupported config key %q (only socket-path is exposed)", args[0])
			}
			path := strings.TrimSpace(args[1])
			if path == "" {
				return fmt.Errorf("socket-path must not be empty")
			}
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.DB.SetSetting(state.SettingWorkerSocketPath, path); err != nil {
				return err
			}
			fmt.Printf("socket-path set to %s\n", path)
			if err := restartManagedWorker(app); err != nil {
				return err
			}
			return nil
		},
	}
}

// configUnsetCmd clears the persisted socket-path override and restarts the
// managed worker back onto the default path.
func configUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset socket-path",
		Short: "Clear the socket-path override (restarts the managed worker on the default path)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "socket-path" {
				return fmt.Errorf("unsupported config key %q (only socket-path is exposed)", args[0])
			}
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			if err := app.DB.UnsetSetting(state.SettingWorkerSocketPath); err != nil {
				return err
			}
			fmt.Println("socket-path cleared")
			if err := restartManagedWorker(app); err != nil {
				return err
			}
			return nil
		},
	}
}

// restartManagedWorker reloads the managed global worker launchd job when one
// is installed. Without a managed worker the new value takes effect on the next
// worker start (§15.3).
func restartManagedWorker(app *App) error {
	if !workerJobInstalled(app) {
		fmt.Println("no managed worker job installed; the new value takes effect on the next worker start")
		return nil
	}
	if err := installWorkerJob(app); err != nil {
		return fmt.Errorf("restart managed worker: %w", err)
	}
	fmt.Println("managed worker restarted")
	return nil
}

// workerJobInstalled reports whether the global worker launchd plist exists.
func workerJobInstalled(app *App) bool {
	agentsDir := launchAgentsDir(app)
	if agentsDir == "" {
		return false
	}
	cfg := launchd.Config{LabelPrefix: launchLabelPrefix, Kind: launchd.JobWorker, Binary: mustSelfPath()}
	_, err := os.Stat(cfg.PlistPath(agentsDir))
	return err == nil
}
