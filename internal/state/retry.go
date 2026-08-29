package state

import (
	"math/rand"
	"time"
)

// Retry classification values.
const (
	RetryRetryable = "retryable"
	RetryTerminal  = "terminal"
)

// RetryBackoff computes the next retry delay for the given consecutive failure
// count (1-based) using capped exponential backoff with jitter (§18.2).
func RetryBackoff(consecutive int) time.Duration {
	if consecutive < 1 {
		consecutive = 1
	}
	base := time.Second
	cap := 30 * time.Minute
	mult := uint(consecutive - 1)
	if mult > 10 {
		mult = 10
	}
	delay := base << mult
	if delay > cap {
		delay = cap
	}
	// 20% jitter.
	delay += time.Duration(rand.Int63n(int64(delay) / 5))
	return delay
}
