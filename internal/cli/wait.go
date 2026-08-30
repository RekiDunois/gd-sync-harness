package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

// waiterPollInterval is the readiness-observation poll interval (§10.3).
const waiterPollInterval = 2 * time.Second

func newProfileWaitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wait <id>",
		Short: "Wait until a profile's sync reaches ready (machine-friendly)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := NewApp()
			if err != nil {
				return err
			}
			defer app.Close()
			return waitForReady(app, args[0])
		},
	}
}

// waitForReady observes until ready or a blocking terminal condition (§13, §14).
// It is socket-first with SQLite fallback. It is a read-only observer:
// interrupting the waiter never cancels worker-owned reconciliation. It crosses
// retryable attempts without failing early.
func waitForReady(app *App, id string) error {
	// Validate the profile exists up front.
	if _, err := app.requireProfile(id); err != nil {
		return err
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-sig:
			// Interrupting the waiter exits only this observer; worker-owned
			// reconciliation continues (§13).
			close(stop)
		case <-stop:
		}
	}()

	obs := socketObserver(app, id)
	for {
		stream, err := obs.Connect()
		if err == nil {
			term, serr := waitStreamReady(stream, stop)
			stream.Close()
			if serr != nil {
				return serr
			}
			if term {
				return nil
			}
			// Stream ended without a terminal condition (worker restart):
			// reconnect after a short delay.
			select {
			case <-stop:
				return fmt.Errorf("wait interrupted; reconciliation continues in the worker")
			case <-time.After(waiterPollInterval):
			}
			continue
		}
		// Socket unavailable: SQLite fallback, keep retrying the socket.
		term, derr := waitDurableReady(app, id, stop)
		if derr != nil {
			return derr
		}
		if term {
			return nil
		}
	}
}

// waitStreamReady consumes snapshots until ready or a blocking terminal
// condition. It does not render every frame (§14.3).
func waitStreamReady(stream *live.Stream, stop <-chan struct{}) (bool, error) {
	type res struct {
		snap *live.StatusSnapshot
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		for {
			snap, err := stream.Next()
			select {
			case ch <- res{snap, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-stop:
			return false, fmt.Errorf("wait interrupted; reconciliation continues in the worker")
		case r := <-ch:
			if r.err != nil {
				return false, nil // not terminal; caller reconnects/falls back
			}
			if r.snap == nil {
				continue
			}
			done, err := terminalFromSnapshot(r.snap)
			if err != nil {
				return false, err
			}
			if done {
				return true, nil
			}
		case <-time.After(waiterPollInterval):
			// The stream may be silent while the worker idles between attempts;
			// that is fine — terminal transitions are published.
		}
	}
}

// waitDurableReady is the SQLite fallback waiter (§9.4). Terminal success and
// blocking conditions mirror the current semantics.
func waitDurableReady(app *App, id string, stop <-chan struct{}) (bool, error) {
	ticker := time.NewTicker(waiterPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return false, fmt.Errorf("wait interrupted; reconciliation continues in the worker")
		case <-ticker.C:
		}
		ss, err := app.DB.GetSyncState(id)
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				return false, fmt.Errorf("profile %q is no longer present (deleted)", id)
			}
			return false, err
		}
		p, err := app.DB.GetProfile(id)
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				return false, fmt.Errorf("profile %q is no longer present (deleted)", id)
			}
			return false, err
		}
		if p.Tombstoned {
			return false, fmt.Errorf("profile %q was deleted while waiting", id)
		}
		if p.DeletionRequestedAt != nil {
			return false, fmt.Errorf("profile %q deletion was requested while waiting", id)
		}
		if !p.Enabled {
			return false, fmt.Errorf("profile %q was disabled while waiting", id)
		}
		switch ss.State {
		case state.StateReady:
			return true, nil
		case state.StateError:
			if ss.RetryClassification != nil && *ss.RetryClassification == state.RetryTerminal {
				return false, fmt.Errorf("sync failed with terminal error: %s", stringOr(ss.LastError, "unknown"))
			}
		}
	}
}

// terminalFromSnapshot evaluates a snapshot for a wait/terminal condition
// (§14.2).
func terminalFromSnapshot(snap *live.StatusSnapshot) (bool, error) {
	switch {
	case snap.Sync.State == state.StateReady && !snap.Profile.Tombstoned && !snap.Profile.DeletionRequested:
		return true, nil
	case snap.Profile.Tombstoned:
		return false, fmt.Errorf("profile %q was deleted while waiting", snap.ProfileID)
	case snap.Profile.DeletionRequested:
		return false, fmt.Errorf("profile %q deletion was requested while waiting", snap.ProfileID)
	case !snap.Profile.Enabled:
		return false, fmt.Errorf("profile %q was disabled while waiting", snap.ProfileID)
	case snap.Sync.State == state.StateError:
		if snap.Sync.RetryClassification != nil && *snap.Sync.RetryClassification == state.RetryTerminal {
			return false, fmt.Errorf("sync failed with terminal error: %s", stringOr(snap.Sync.LastError, "unknown"))
		}
	}
	return false, nil
}

func stringOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}
