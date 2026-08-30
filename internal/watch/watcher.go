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

type FastSettings struct {
	SettleSeconds   int
	MaxDelaySeconds int
}

func DefaultFastSettings() FastSettings { return FastSettings{SettleSeconds: 3, MaxDelaySeconds: 30} }

// fswatch's numeric masks are stable bit flags. Keep them named at the
// boundary instead of scattering magic numbers through event classification.
const (
	EventPlatformSpecific  uint64 = 1
	EventCreated           uint64 = 2
	EventUpdated           uint64 = 4
	EventRemoved           uint64 = 8
	EventRenamed           uint64 = 16
	EventOwnerModified     uint64 = 32
	EventAttributeModified uint64 = 64
	EventMovedFrom         uint64 = 128
	EventMovedTo           uint64 = 256
	EventIsFile            uint64 = 512
	EventIsDir             uint64 = 1024
	EventIsSymLink         uint64 = 2048
	EventLink              uint64 = 4096
	EventOverflow          uint64 = 8192
)

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
	OnCatchUp   func(ctx context.Context) error

	cmd       *exec.Cmd
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	running   bool
	firstSeen map[string]time.Time
	lastSeen  map[string]time.Time
}

func (w *Watcher) Start(ctx context.Context) error {
	w.mu.Lock()
	w.done = make(chan struct{})
	w.firstSeen = make(map[string]time.Time)
	w.lastSeen = make(map[string]time.Time)
	w.mu.Unlock()

	if resolved, err := filepath.EvalSymlinks(w.SourcePath); err == nil {
		w.SourcePath = resolved
	}
	cctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	cmd := exec.CommandContext(cctx, w.FSWatchBin, "-0", "-r", "-n", "--format=%p%0%f", w.SourcePath)
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
	// The watcher is live before catch-up starts, so the scan cannot create a
	// gap in which an event is missed.
	if w.OnCatchUp != nil {
		go func() {
			if err := w.OnCatchUp(cctx); err != nil {
				w.Log.Printf("startup catch-up failed: %v", err)
			}
		}()
	}
	return nil
}

func (w *Watcher) Stop() error {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
	}
	done := w.done
	w.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		w.mu.Lock()
		if w.cmd != nil && w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		w.mu.Unlock()
	}
	return nil
}

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
	scanner.Buffer(make([]byte, 4096), 1<<20)
	scanner.Split(splitNUL)
	var path string
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		record := scanner.Text()
		if path == "" {
			path = record
			continue
		}
		flags := parseFlags(record)
		w.handleEvent(ctx, path, flags)
		path = ""
	}
	if path != "" {
		// A missing numeric companion makes the event unsafe to classify.
		w.handleEvent(ctx, path, 0)
	}
}

func parseFlags(s string) uint64 {
	var n uint64
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + uint64(r-'0')
	}
	return n
}

func (w *Watcher) handleEvent(ctx context.Context, path string, flags uint64) {
	rel := relPathUnder(w.SourcePath, path)
	if rel == "" {
		return
	}
	kind := w.classify(path, rel, flags)
	if kind == "" {
		return
	}
	full := kind == state.EventDelete || kind == state.EventRename || kind == state.EventOther
	if _, err := w.DB.RecordEvent(w.ProfileID, rel, kind, full); err != nil {
		w.Log.Printf("persist event: %v", err)
		return
	}
	w.mu.Lock()
	if _, ok := w.firstSeen[rel]; !ok {
		w.firstSeen[rel] = time.Now()
	}
	w.lastSeen[rel] = time.Now()
	w.mu.Unlock()
}

func (w *Watcher) classify(path, rel string, flags uint64) string {
	if w.Filter != nil {
		if excluded, _ := w.Filter.Excluded(rel); excluded {
			return ""
		}
	}
	if flags == 0 || flags&EventOverflow != 0 || flags&EventPlatformSpecific != 0 {
		return state.EventOther
	}
	if flags&EventIsSymLink != 0 {
		return state.EventOther
	}
	if flags&EventIsDir != 0 {
		if flags&(EventCreated|EventRemoved|EventRenamed|EventMovedFrom|EventMovedTo) != 0 {
			return state.EventOther
		}
		return ""
	}
	if flags&(EventRemoved|EventRenamed|EventMovedFrom) != 0 {
		return state.EventRename
	}
	if flags&(EventCreated|EventUpdated|EventMovedTo) != 0 && flags&EventIsFile != 0 {
		return state.EventModify
	}
	// Attribute/owner-only or otherwise incomplete records are unsafe for fast
	// upload and therefore deliberately promote to a full reconciliation.
	_ = path
	return state.EventOther
}

func (w *Watcher) debouncer(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			w.mu.Lock()
			if len(w.firstSeen) == 0 {
				w.mu.Unlock()
				continue
			}
			first, last := time.Time{}, time.Time{}
			for rel, seen := range w.firstSeen {
				if first.IsZero() || seen.Before(first) {
					first = seen
				}
				if seen = w.lastSeen[rel]; last.IsZero() || seen.After(last) {
					last = seen
				}
			}
			due := now.Sub(last) >= time.Duration(w.Settings.SettleSeconds)*time.Second ||
				now.Sub(first) >= time.Duration(w.Settings.MaxDelaySeconds)*time.Second
			if !due {
				w.mu.Unlock()
				continue
			}
			window := make(map[string]struct{}, len(w.firstSeen))
			for rel := range w.firstSeen {
				window[rel] = struct{}{}
			}
			w.firstSeen = make(map[string]time.Time)
			w.lastSeen = make(map[string]time.Time)
			w.mu.Unlock()
			if err := w.fireBatch(ctx, window); err != nil {
				w.Log.Printf("batch error: %v", err)
			}
		}
	}
}

func (w *Watcher) fireBatch(ctx context.Context, window map[string]struct{}) error {
	pending, err := w.DB.ListPending(w.ProfileID)
	if err != nil {
		return err
	}
	var batch []state.PendingEvent
	for _, event := range pending {
		if _, ok := window[event.Path]; ok {
			batch = append(batch, event)
		}
	}
	// The watcher is an event producer only (§13.1): execution decisions
	// (full-vs-fast, debounce evaluation, event clearing) belong to the worker.
	// The durable pending_events rows carry first_seen/last_seen for the
	// worker's due-batch evaluation; a restart never changes eligibility.
	//
	// We still notify the worker so the full/fast decision happens promptly.
	// Destructive/uncertain events promote to full reconciliation as before —
	// this is a durable intent decision, not an execution.
	full, err := w.DB.HasDestructivePending(w.ProfileID)
	if err != nil {
		return err
	}
	manifest, err := w.DB.ManifestCount(w.ProfileID)
	if err != nil {
		return err
	}
	if full || len(pending) > 500 || (manifest > 0 && len(pending)*100 > manifest*5) {
		runtime, err := w.DB.GetRuntime(w.ProfileID)
		if err != nil {
			return err
		}
		if err := w.DB.PromoteToFullReconcile(w.ProfileID, runtime.SourceGeneration, state.Now()); err != nil {
			return err
		}
		if w.OnReconcile != nil {
			return w.OnReconcile(ctx)
		}
		return nil
	}
	if len(batch) == 0 {
		// No fast-path batch, but durable full-debt may exist (e.g. a
		// destructive event promoted directly to a full reconcile). Notify the
		// worker so it schedules the full reconcile promptly.
		ss, _ := w.DB.GetSyncState(w.ProfileID)
		if ss != nil && ss.HasDebt() {
			if w.OnReconcile != nil {
				return w.OnReconcile(ctx)
			}
			return nil
		}
		return nil
	}
	if w.OnBatch != nil {
		if err := w.OnBatch(ctx, batchPaths(batch)); err != nil {
			return err
		}
	}
	return nil
}

func batchPaths(batch []state.PendingEvent) []string {
	out := make([]string, 0, len(batch))
	for _, e := range batch {
		out = append(out, e.Path)
	}
	return out
}

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

func relPathUnder(source, path string) string {
	rel := strings.TrimPrefix(path, source+"/")
	if rel != path && rel != "" {
		return rel
	}
	if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		resolved := filepath.Join(resolvedDir, filepath.Base(path))
		rel = strings.TrimPrefix(resolved, source+"/")
		if rel != resolved && rel != "" {
			return rel
		}
	}
	if resolvedSrc, err := filepath.EvalSymlinks(source); err == nil {
		rel = strings.TrimPrefix(path, resolvedSrc+"/")
		if rel != path && rel != "" {
			return rel
		}
	}
	return ""
}

// Keep os imported for source compatibility with callers that use the package
// alongside the old stat-based watcher implementation.
var _ = os.IsNotExist
