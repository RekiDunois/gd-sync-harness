package live

import (
	"sync"
	"time"
)

// DurableReader is the worker's view into durable SQLite state, provided by
// the host process so the live package stays transport-only (§3). Subscribe
// uses it for the one durable refresh per profile; the periodic worker rescan
// invalidates the cache through Refresh.
type DurableReader interface {
	// BuildSnapshot assembles a full status snapshot for a profile from
	// durable state plus the current live activity. It returns nil when the
	// profile does not exist.
	BuildSnapshot(profileID string, activity *ActivityS) *StatusSnapshot
	// Refresh marks the durable cache entry for a profile dirty. It reports
	// whether the profile exists.
	Refresh(profileID string) bool
}

// Hub is the latest-value subscriber registry (§6.4). Publishing is
// non-blocking: each subscriber has a capacity-1 slot whose stale value is
// replaced by the newest snapshot. No socket client can block the worker
// reconciliation path or rclone progress callback (§5.6).
type Hub struct {
	mu          sync.Mutex
	subscribers map[string][]*subscriber
}

type subscriber struct {
	profileID string
	ch        chan StatusSnapshot
}

// NewHub builds an empty hub.
func NewHub() *Hub {
	return &Hub{subscribers: map[string][]*subscriber{}}
}

// Subscribe registers a subscriber for a profile and returns a channel that
// receives full replacement snapshots.
func (h *Hub) Subscribe(profileID string) <-chan StatusSnapshot {
	ch := make(chan StatusSnapshot, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers[profileID] = append(h.subscribers[profileID], &subscriber{profileID: profileID, ch: ch})
	return ch
}

// Unsubscribe removes a subscriber channel.
func (h *Hub) Unsubscribe(profileID string, ch <-chan StatusSnapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subscribers[profileID]
	for i, s := range subs {
		if s.ch == ch {
			h.subscribers[profileID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(h.subscribers[profileID]) == 0 {
		delete(h.subscribers, profileID)
	}
}

// Publish fans the snapshot out to subscribers of the matching profile,
// replacing the stale pending snapshot when a subscriber has not consumed it
// yet (latest-value delivery, §5.6).
func (h *Hub) Publish(s StatusSnapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subscribers[s.ProfileID] {
		select {
		case sub.ch <- s:
		default:
			// Slot full: drop the stale pending snapshot, retain the newest.
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- s:
			default:
			}
		}
	}
}

// BroadcastSeq assigns the next monotonic snapshot sequence number.
type seqCounter struct {
	mu   sync.Mutex
	next int64
}

func (c *seqCounter) Next() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	return c.next
}

// SampleClock is an injectable clock for the server so tests can control time.
type SampleClock func() time.Time

// NowClock returns time.Now.
func NowClock() time.Time { return time.Now() }
