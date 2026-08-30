package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"knowledge-sync/internal/state"
)

// startLeaseRenewal keeps a remote operation lease alive for the duration of an
// operation so long transfers do not expire their concurrency slot (§20.2).
func startLeaseRenewal(ctx context.Context, db *state.DB, id string) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = db.RenewRemoteLease(id)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func leaseID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "lease-fallback"
	}
	return hex.EncodeToString(b)
}
