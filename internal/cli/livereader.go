package cli

import (
	"sync"
	"time"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

// liveDurableReader implements live.DurableReader from SQLite durable state
// (§6.1, §6.3). It caches per-profile durable snapshots and rebuilds them only
// on refresh (subscribe, invalidate, worker-owned mutation, periodic rescan) —
// never per telemetry frame.
type liveDurableReader struct {
	db *state.DB

	mu    sync.Mutex
	cache map[string]*state.DurableSnapshot
	// activityFor returns the current live activity for a profile, or nil. It
	// lets the initial subscribe snapshot merge activity that is already in
	// worker memory (§6.4) without the server owning activity state.
	activityFor func(profileID string) *live.ActivityS
}

func newLiveDurableReader(db *state.DB) *liveDurableReader {
	return &liveDurableReader{db: db, cache: map[string]*state.DurableSnapshot{}}
}

// SetActivityProvider registers the live activity source (set by the worker).
func (r *liveDurableReader) SetActivityProvider(fn func(profileID string) *live.ActivityS) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.activityFor = fn
}

// BuildSnapshot merges the cached durable state with the current live activity
// and returns a full replacement snapshot. A nil return means the profile does
// not exist (the server replies unknown_profile).
func (r *liveDurableReader) BuildSnapshot(profileID string, activity *live.ActivityS) *live.StatusSnapshot {
	r.mu.Lock()
	durable, ok := r.cache[profileID]
	activityProvider := r.activityFor
	r.mu.Unlock()
	if !ok || durable == nil || durable.Profile == nil {
		// Cache miss: load once (a fresh subscriber path already refreshed, but
		// the periodic rescan may not have visited this profile yet).
		if !r.Refresh(profileID) {
			return nil
		}
		r.mu.Lock()
		durable = r.cache[profileID]
		r.mu.Unlock()
		if durable == nil {
			return nil
		}
	}
	if activity == nil && activityProvider != nil {
		activity = activityProvider(profileID)
	}
	return buildSnapshotFromDurable(durable, activity)
}

// Refresh reloads the durable state for a profile from SQLite. It returns
// whether the profile exists.
func (r *liveDurableReader) Refresh(profileID string) bool {
	snap, err := r.db.LoadDurableSnapshot(profileID)
	if err != nil {
		return false
	}
	r.mu.Lock()
	r.cache[profileID] = snap
	r.mu.Unlock()
	return true
}

// buildSnapshotFromDurable composes the public status snapshot (§5.2).
func buildSnapshotFromDurable(d *state.DurableSnapshot, activity *live.ActivityS) *live.StatusSnapshot {
	p := d.Profile
	s := live.StatusSnapshot{
		ProfileID: p.ID,
		Profile: live.ProfileS{
			Enabled:           p.Enabled,
			Tombstoned:        p.Tombstoned,
			DeletionRequested: p.DeletionRequestedAt != nil,
		},
	}
	if d.SyncState != nil {
		ss := d.SyncState
		s.Sync = live.SyncS{
			Initialized:       ss.IsInitialized(),
			State:             ss.State,
			DesiredGeneration: ss.DesiredGeneration,
			LastSuccessAt:     parseStateTime(ss.LastSuccessAt),
			LastError:         ss.LastError,
		}
		if ss.LastSuccessGeneration != nil {
			v := *ss.LastSuccessGeneration
			s.Sync.LastSuccessGeneration = &v
		}
		if ss.CurrentRunID != nil {
			runID := *ss.CurrentRunID
			s.Sync.CurrentRunID = &runID
		}
		if ss.RetryClassification != nil {
			rc := *ss.RetryClassification
			s.Sync.RetryClassification = &rc
		}
		s.Sync.NextRetryAt = parseStateTime(ss.NextRetryAt)
		if ss.Phase != "" {
			ph := ss.Phase
			s.Sync.Phase = &ph
		}
	}
	if d.CurrentRun != nil {
		// The run row is authoritative for the durable counters while a run is
		// active; live activity overrides them when present.
		run := d.CurrentRun
		if activity == nil {
			activity = &live.ActivityS{
				Kind:            live.ActivityFullReconcile,
				Phase:           run.Phase,
				FilesCompleted:  run.FilesCompleted,
				BytesCompleted:  run.BytesCompleted,
				BytesTotal:      run.BytesTotal,
				ChecksCompleted: run.ChecksCompleted,
				ChecksTotal:     run.ChecksTotal,
				ItemsListed:     run.ItemsListed,
				ErrorsCount:     run.ErrorsCount,
				ActiveTransfers: run.ActiveTransfers,
			}
			if run.ID != "" {
				rid := run.ID
				activity.RunID = &rid
			}
			if run.CurrentItem != nil {
				activity.CurrentItem = *run.CurrentItem
			}
			activity.CurrentItemBytes = run.CurrentItemBytes
			activity.CurrentItemSize = run.CurrentItemSize
		}
	}
	s.Activity = activity
	return &s
}

// parseStateTime converts a durable timestamp string to time.Time when parseable.
func parseStateTime(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(stateTimeLayout, *s)
	if err != nil {
		return nil
	}
	return &t
}

const stateTimeLayout = "2006-01-02T15:04:05.000Z07:00"
