package state

import "errors"

// DurableSnapshot bundles the durable data needed to build a live status
// snapshot (§6.1). The worker caches this per profile and only refreshes it on
// subscribe/invalidate/rescan, never per telemetry frame.
type DurableSnapshot struct {
	Profile    *Profile
	SyncState  *ProfileSyncState
	CurrentRun *SyncRun
	Runtime    *Runtime
}

// LoadDurableSnapshot reads the durable state for a profile in one call. It
// returns ErrNotFound when the profile does not exist.
func (d *DB) LoadDurableSnapshot(profileID string) (*DurableSnapshot, error) {
	p, err := d.GetProfile(profileID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	ss, err := d.GetSyncState(profileID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			ss = nil
		} else {
			return nil, err
		}
	}
	var run *SyncRun
	if ss != nil && ss.CurrentRunID != nil {
		run, _ = d.GetRun(*ss.CurrentRunID)
	}
	rt, _ := d.GetRuntime(profileID)
	return &DurableSnapshot{Profile: p, SyncState: ss, CurrentRun: run, Runtime: rt}, nil
}
