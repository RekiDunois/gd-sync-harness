package watch

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"knowledge-sync/internal/filter"
	"knowledge-sync/internal/state"
)

// FastSettings tune the debounce/batching behavior (§12).
type FastSettings struct {
	SettleSeconds   int
	MaxDelaySeconds int
}

// DefaultFastSettings returns the §12 initial defaults.
func DefaultFastSettings() FastSettings {
	return FastSettings{SettleSeconds: 3, MaxDelaySeconds: 30}
}

// Watcher supervises one profile's fswatch child (§19.1). Events are persisted
// to SQLite before any batching. It is NOT the correctness authority — it only
// provides low-latency change hints. The same structured filter engine governs
// watcher eligibility, manifest scans, fast-path lists, and reconciliation
// (§10.1).
type Watcher struct {
	ProfileID   string
	SourcePath  string
	FSWatchBin  string
	DB          *state.DB
	Log         *log.Logger
	Settings    FastSettings
	Filter      *filter.Engine
	OnBatch     func(ctx context.Context, changed []string) error
	OnReconcile func(ctx context.Context) error

	cmd      *exec.Cmd
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	running  bool
	firstSeen map[string]time.Time
}

// Start launches fswatch and begins reading events. SourcePath is resolved to
// its physical path (macOS /tmp → /private/tmp) so fswatch event paths match
// the source prefix exactly (§11).
func (w *Watcher) Start(ctx context.Context) error {
	w.mu.Lock()
	w.done = make(chan struct{})
	w.firstSeen = make(map[string]time.Time)
	w.mu.Unlock()

	// Resolve symlinks so event paths from fswatch line up with SourcePath.
	if resolved, err := filepath.EvalSymlinks(w.SourcePath); err == nil {
		w.SourcePath = resolved
	}

	cctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	cmd := exec.CommandContext(cctx, w.FSWatchBin, "-0", "-r", w.SourcePath)
	w.cmd = cmd
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return err
	}
	w.mu.Lock()
	w.running = true
	w.mu.Unlock()
	w.Log.Printf("fswatch started for %s pid=%d", w.ProfileID, cmd.Process.Pid)

	go w.readLoop(cctx, stdout)
	go w.debouncer(cctx)

	return nil
}

// Stop terminates the fswatch child and waits for cleanup.
func (w *Watcher) Stop() error {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	w.mu.Unlock()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		w.mu.Lock()
		if w.cmd != nil && w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		w.mu.Unlock()
	}
	return nil
}

// IsRunning reports whether the watcher is active.
func (w *Watcher) IsRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

func (w *Watcher) readLoop(ctx context.Context, stdout io.ReadCloser) {
	defer close(w.done)
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		_ = w.cmd.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	scanner.Split(splitNUL)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		ev := scanner.Text()
		if ev == "" {
			continue
		}
		w.handleEvent(ctx, ev)
	}
}

func (w *Watcher) handleEvent(ctx context.Context, path string) {
	rel := relPathUnder(w.SourcePath, path)
	if rel == "" {
		return
	}
	kind := w.classify(path, rel)
	if kind == "" {
		return
	}
	gen, _ := w.DB.BumpGeneration(w.ProfileID)
	if err := w.DB.UpsertPendingEvent(w.ProfileID, rel, kind, gen); err != nil {
		w.Log.Printf("persist event: %v", err)
		return
	}
	w.mu.Lock()
	if _, ok := w.firstSeen[rel]; !ok {
		w.firstSeen[rel] = time.Now()
	}
	w.mu.Unlock()
	if kind == state.EventDelete || kind == state.EventRename {
		_ = w.DB.RequestReconcile(w.ProfileID)
	}
}

// classify converts a path to an event kind, or "" if excluded by the
// structured filter (§10.1). rel is the path relative to the profile source.
func (w *Watcher) classify(path, rel string) string {
	if w.Filter != nil {
		if excluded, _ := w.Filter.Excluded(rel); excluded {
			return ""
		}
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return state.EventDelete
		}
		return state.EventOther
	}
	return state.EventModify
}

// debouncer implements the §12 batch semantics: fire when quiet for SettleSeconds,
// or force a batch by MaxDelaySeconds after the first pending event.
func (w *Watcher) debouncer(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var lastFire time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.mu.Lock()
			firstSeen := w.firstSeen
			w.mu.Unlock()
			if len(firstSeen) == 0 {
				continue
			}
			now := time.Now()
			var oldest time.Time
			for _, t := range firstSeen {
				if oldest.IsZero() || t.Before(oldest) {
					oldest = t
				}
			}
			force := now.Sub(oldest) >= time.Duration(w.Settings.MaxDelaySeconds)*time.Second
			quiet := now.Sub(lastFire) >= time.Duration(w.Settings.SettleSeconds)*time.Second
			if force || quiet {
				if err := w.fireBatch(ctx); err != nil {
					w.Log.Printf("batch error: %v", err)
				}
				lastFire = now
				w.mu.Lock()
				w.firstSeen = make(map[string]time.Time)
				w.mu.Unlock()
			}
		}
	}
}

// fireBatch aggregates the pending changed files and calls OnBatch (fast upsert).
// If any pending event is destructive, calls OnReconcile instead (§14).
func (w *Watcher) fireBatch(ctx context.Context) error {
	destructive, err := w.DB.HasDestructivePending(w.ProfileID)
	if err != nil {
		return err
	}
	if destructive {
		if w.OnReconcile != nil {
			_ = w.DB.RequestReconcile(w.ProfileID)
			return w.OnReconcile(ctx)
		}
		return nil
	}
	pending, err := w.DB.ListPending(w.ProfileID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	changed := make([]string, 0, len(pending))
	for _, e := range pending {
		changed = append(changed, e.Path)
	}
	if len(changed) > 500 {
		_ = w.DB.RequestReconcile(w.ProfileID)
		if w.OnReconcile != nil {
			return w.OnReconcile(ctx)
		}
		return nil
	}
	if w.OnBatch != nil {
		if err := w.OnBatch(ctx, changed); err != nil {
			return err
		}
	}
	_ = w.DB.ClearPendingPaths(w.ProfileID, changed)
	_ = w.DB.MarkFastSuccess(w.ProfileID)
	return nil
}

// splitNUL is a bufio.SplitFunc for NUL-separated records (fswatch -0).
func splitNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i := 0; i < len(data); i++ {
		if data[i] == 0 {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// relPathUnder returns the slash-separated path of `path` relative to `source`.
// It tolerates macOS /tmp vs /private/tmp style symlink discrepancies by
// resolving the event's directory prefix when the direct prefix match fails
// (the file itself may already be deleted, so we resolve the parent dir).
func relPathUnder(source, path string) string {
	rel := strings.TrimPrefix(path, source+"/")
	if rel != path && rel != "" {
		return rel
	}
	// Direct prefix failed: resolve the event's parent directory symlinks.
	if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		resolved := filepath.Join(resolvedDir, filepath.Base(path))
		rel = strings.TrimPrefix(resolved, source+"/")
		if rel != resolved && rel != "" {
			return rel
		}
	}
	// Also try the reverse: event came in resolved form but source is not.
	if resolvedSrc, err := filepath.EvalSymlinks(source); err == nil {
		rel = strings.TrimPrefix(path, resolvedSrc+"/")
		if rel != path && rel != "" {
			return rel
		}
	}
	return ""
}
