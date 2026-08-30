package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/config"
	rcexec "knowledge-sync/internal/exec"
	"knowledge-sync/internal/flock"
	"knowledge-sync/internal/live"
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

	// LiveReader is the durable-state cache backing the worker status socket
	// (§6.1). It is created for every command but only the worker's server
	// consumes it; observers use the live client package.
	LiveReader *liveDurableReader

	// LiveServer is the worker's Unix socket status server. It is nil for
	// non-worker commands and when socket startup failed.
	LiveServer *live.Server
	// Activities holds the worker's ephemeral live activity state.
	Activities *activityTracker

	ConfigPath string
	RcloneBin  string
	FSWatchBin string
	LogDir     string
	LockDir    string
	Config     config.Config
}

// Context returns a cancellable lifecycle context. Long data-plane operations
// must not inherit an arbitrary wall-clock deadline.
func (a *App) Context() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// NewApp initializes the shared app from the environment. Tool paths are read
// from persisted settings first (so launchd jobs do not depend on interactive
// PATH, §31.2), falling back to exec.LookPath, then persisted for next time.
func NewApp() (*App, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	dbPath, _ := paths.DBPath()
	db, err := state.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	rcloneBin, err := resolveToolFromDB(db, "rclone")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("rclone not found: %w (run 'knowledge-sync doctor')", err)
	}
	fswatchBin, _ := resolveToolFromDB(db, "fswatch")

	probe := rcexec.NewRclone(rcloneBin, "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	configPath, err := probe.DiscoverConfigPath(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("discover rclone config: %w", err)
	}

	// Persist resolved tool paths so launchd-spawned processes reuse them.
	_ = db.SetSetting(state.SettingRcloneBin, rcloneBin)
	if fswatchBin != "" {
		_ = db.SetSetting(state.SettingFSWatchBin, fswatchBin)
	}
	_ = db.SetSetting(state.SettingRcloneCfg, configPath)
	appConfigPath, err := paths.AppConfigPath()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("resolve app config: %w", err)
	}
	appConfig, err := config.Load(appConfigPath)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("load knowledge-sync config %s: %w", appConfigPath, err)
	}

	rclone := rcexec.NewRclone(rcloneBin, configPath)
	rm := remote.New(rclone, db)
	svc := sync.New(rclone, db, appConfig.Rclone)
	rec := sync.NewReconciler(svc)

	logDir, _ := paths.LogsDir()

	return &App{
		DB: db, Rclone: rclone, Remote: rm, Sync: svc, Reconciler: rec,
		LiveReader: newLiveDurableReader(db), Activities: newActivityTracker(),
		ConfigPath: configPath, RcloneBin: rcloneBin, FSWatchBin: fswatchBin,
		LogDir: logDir, LockDir: filepath.Join(mustStateDir(), "locks"), Config: appConfig,
	}, nil
}

// resolveToolFromDB resolves a binary path from persisted settings first,
// falling back to PATH lookup.
func resolveToolFromDB(db *state.DB, name string) (string, error) {
	key := ""
	switch name {
	case "rclone":
		key = state.SettingRcloneBin
	case "fswatch":
		key = state.SettingFSWatchBin
	}
	if key != "" {
		if v, _ := db.GetSetting(key); v != "" {
			return v, nil
		}
	}
	return exec.LookPath(name)
}

// Close releases the database handle.
func (a *App) Close() {
	if a.DB != nil {
		_ = a.DB.Close()
	}
}

// activities returns the live activity tracker, creating one lazily so command
// paths (including tests that build App structs directly) always have a valid
// tracker.
func (a *App) activities() *activityTracker {
	if a.Activities == nil {
		a.Activities = newActivityTracker()
	}
	return a.Activities
}

// liveReader returns the durable live reader, creating one lazily. Worker and
// observer command paths both rely on it.
func (a *App) liveReader() *liveDurableReader {
	if a.LiveReader == nil {
		a.LiveReader = newLiveDurableReader(a.DB)
	}
	return a.LiveReader
}

// outputWriter is the destination for status/observer rendering. Commands write
// to it so tests can capture output without spawning processes.
var outputWriter io.Writer = os.Stdout

// out returns the current output writer.
func out() io.Writer {
	if outputWriter == nil {
		return os.Stdout
	}
	return outputWriter
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
	lockDir := a.LockDir
	if lockDir == "" {
		stateDir, _ := paths.StateDir()
		lockDir = filepath.Join(stateDir, "locks")
	}
	lock, err := flock.Acquire(lockDir, p.ID)
	if err != nil {
		return err
	}
	defer lock.Release()
	return fn()
}

func mustStateDir() string {
	d, _ := paths.StateDir()
	return d
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
		Version:      version.Details(),
	}

	root.AddCommand(
		newDoctorCmd(),
		newStatusCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newProfileCmd(),
		newSyncNowCmd(),
		newSyncCmd(),
		newReconcileNowCmd(),
		newReconcileScheduledCmd(),
		newWorkerCmd(),
		newWatchCmd(),
		newVerifyCmd(),
		newRepairDuplicatesCmd(),
		newPurgeRemoteCmd(),
		newProbeCmd(),
		newStopCmd(),
		newConfigCmd(),
	)

	return root
}
