package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

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

// waitForReady observes durable profile state until ready or a blocking
// terminal condition (§13, §14). It is a read-only observer: interrupting the
// waiter never cancels worker-owned reconciliation. It crosses retryable
// attempts without failing early.
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

	for {
		select {
		case <-stop:
			return fmt.Errorf("wait interrupted; reconciliation continues in the worker")
		case <-time.After(waiterPollInterval):
		}

		ss, err := app.DB.GetSyncState(id)
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				return fmt.Errorf("profile %q is no longer present (deleted)", id)
			}
			return err
		}
		p, err := app.DB.GetProfile(id)
		if err != nil {
			if errors.Is(err, state.ErrNotFound) {
				return fmt.Errorf("profile %q is no longer present (deleted)", id)
			}
			return err
		}

		if p.Tombstoned {
			return fmt.Errorf("profile %q was deleted while waiting", id)
		}
		if p.DeletionRequestedAt != nil {
			return fmt.Errorf("profile %q deletion was requested while waiting", id)
		}
		if !p.Enabled {
			return fmt.Errorf("profile %q was disabled while waiting", id)
		}

		switch ss.State {
		case state.StateReady:
			return nil
		case state.StateError:
			// Terminal error blocks progress. A retryable error keeps the
			// waiter alive while automatic retry remains scheduled.
			if ss.RetryClassification != nil && *ss.RetryClassification == state.RetryTerminal {
				return fmt.Errorf("sync failed with terminal error: %s", stringOr(ss.LastError, "unknown"))
			}
		}
	}
}

func stringOr(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}
