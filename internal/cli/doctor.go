package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/paths"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Validate harness prerequisites (rclone, fswatch, config path)",
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				fmt.Println("knowledge-sync doctor")
				fmt.Println("  rclone:", execPath("rclone"))
				fmt.Println("  fswatch:", execPath("fswatch"))
				return err
			}
			defer app.Close()
			fmt.Println("knowledge-sync doctor")
			fmt.Printf("  rclone:            %s\n", execPath("rclone"))
			fmt.Printf("  fswatch:           %s\n", execPath("fswatch"))
			fmt.Printf("  rclone config:     %s\n", app.ConfigPath)
			dbPath, _ := paths.DBPath()
			fmt.Printf("  sqlite database:   %s\n", dbPath)
			fmt.Printf("  logs:              %s\n", app.LogDir)

			if fi, err := os.Stat(app.ConfigPath); err == nil {
				mode := fi.Mode().Perm()
				if mode&0o077 != 0 {
					fmt.Printf("  WARN rclone config permissions: %o (should be 0600/0400, §5.2)\n", mode)
				} else {
					fmt.Printf("  rclone config perms: %o (ok)\n", mode)
				}
			}

			ctx, cancel := app.Context()
			defer cancel()
			remotes, err := rcexec.ListRemotes(ctx, app.Rclone)
			if err != nil {
				fmt.Printf("  remotes:           error: %v\n", err)
			} else if len(remotes) == 0 {
				fmt.Println("  remotes:           none configured")
			} else {
				fmt.Printf("  remotes:           %d configured\n", len(remotes))
			}
			return nil
		},
	}
}

func execPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return "(not found)"
	}
	return p
}
