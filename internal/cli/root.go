package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/flock"
	"knowledge-sync/internal/logger"
	"knowledge-sync/internal/paths"
	"knowledge-sync/internal/remote"
	"knowledge-sync/internal/state"
	"knowledge-sync/internal/sync"
	"knowledge-sync/pkg/version"
)

// App carries the shared dependencies for all commands.
type App struct {
	DB         *state.DB
	Rclone     *rcexec.Rclone
	Remote     *remote.Manager
	Sync       *sync.Service
	Reconciler *sync.Reconciler

	ConfigPath string
	RcloneBin  string
	FSWatchBin string
	LogDir     string
}

// Context returns a root context with default timeout.
func (a *App) Context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Minute)
}

// NewApp initializes the shared app from the environment.
func NewApp() (*App, error) {
	rcloneBin, err := exec.LookPath("rclone")
	if err != nil {
		return nil, fmt.Errorf("rclone not found: %w (run 'knowledge-sync doctor')", err)
	}
	fswatchBin, _ := exec.LookPath("fswatch")

	probe := rcexec.NewRclone(rcloneBin, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	configPath, err := probe.DiscoverConfigPath(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover rclone config: %w", err)
	}

	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	dbPath, _ := paths.DBPath()
	db, err := state.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	rclone := rcexec.NewRclone(rcloneBin, configPath)
	rm := remote.New(rclone, db)
	svc := sync.New(rclone, db)
	rec := sync.NewReconciler(svc)

	logDir, _ := paths.LogsDir()

	return &App{
		DB: db, Rclone: rclone, Remote: rm, Sync: svc, Reconciler: rec,
		ConfigPath: configPath, RcloneBin: rcloneBin, FSWatchBin: fswatchBin,
		LogDir: logDir,
	}, nil
}

// Close releases the database handle.
func (a *App) Close() {
	if a.DB != nil {
		_ = a.DB.Close()
	}
}

// requireProfile loads a non-tombstoned profile by ID.
func (a *App) requireProfile(id string) (*state.Profile, error) {
	p, err := a.DB.GetProfile(id)
	if err != nil {
		return nil, err
	}
	if p.Tombstoned {
		return nil, fmt.Errorf("profile %q is tombstoned", id)
	}
	return p, nil
}

// withProfileLock acquires the per-profile writer lock and releases on done.
func (a *App) withProfileLock(p *state.Profile, fn func() error) error {
	stateDir, _ := paths.StateDir()
	lockDir := filepath.Join(stateDir, "locks")
	lock, err := flock.Acquire(lockDir, p.ID)
	if err != nil {
		return err
	}
	defer lock.Release()
	return fn()
}

// logPathFor returns a per-profile log path.
func (a *App) logPathFor(profileID, kind string) string {
	return filepath.Join(a.LogDir, logger.SanitizePath(profileID)+"."+kind+".log")
}

// NewRootCmd builds the full CLI.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:          "knowledge-sync",
		Short:        "Local knowledge sources -> Google Drive via rclone",
		SilenceUsage: true,
		Version:      version.Version,
	}

	root.AddCommand(
		newDoctorCmd(),
		newStatusCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newProfileCmd(),
		newSyncNowCmd(),
		newReconcileNowCmd(),
		newReconcileScheduledCmd(),
		newWatchCmd(),
		newVerifyCmd(),
		newRepairDuplicatesCmd(),
		newStopCmd(),
	)

	return root
}
