package cli

import (
	"sync"
	"time"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

// activityTracker holds the ephemeral live activity state for the currently
// executing activity per profile (§6.2). Because the worker executes
// synchronously and is globally serialized, a small map keyed by profile ID is
// sufficient; no distributed registry is needed.
type activityTracker struct {
	mu   sync.Mutex
	live map[string]*liveActivity
	// now returns the current time; defaulting to time.Now, injectable for
	// deterministic tests.
	now func() time.Time
}

// newActivityTracker builds a tracker using the real clock.
func newActivityTracker() *activityTracker {
	return &activityTracker{live: map[string]*liveActivity{}, now: time.Now}
}

// liveActivity is the live, ephemeral state for one activity.
type liveActivity struct {
	profileID string
	kind      string
	runID     *string
	phase     string
	estimator live.ThroughputEstimator
	fileRate  live.FileRateEstimator

	filesCompleted, bytesCompleted, bytesTotal int64
	checksCompleted, checksTotal               int64
	itemsListed, errorsCount, activeTransfers  int64
	currentItem                                string
	currentItemBytes, currentItemSize          int64

	speedKnown        bool
	speedBytesPerSec  float64
	filesPerMinKnown  bool
	filesPerMinute    float64
	uploadStartedAt   *time.Time
	lastProgress      time.Time
	lastProgressValid bool
}

// start begins a new activity, resetting the throughput estimator so a prior
// run's last sample never leaks into this one (§7 rule 7/8).
func (t *activityTracker) start(profileID, kind string, runID *string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := &liveActivity{profileID: profileID, kind: kind, runID: runID}
	a.estimator.Reset()
	a.fileRate.Reset()
	t.live[profileID] = a
}

// setPhase updates the activity phase and clears any speed display when leaving
// a transfer phase (§0.1 speed semantics).
func (t *activityTracker) setPhase(profileID, phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.live[profileID]
	if a == nil {
		return
	}
	a.phase = phase
	if !transferPhase(phase) {
		a.speedKnown = false
		a.speedBytesPerSec = 0
		a.filesPerMinKnown = false
		a.filesPerMinute = 0
		return
	}
	// Record the upload-start timestamp once when the transfer phase begins,
	// mirroring the durable upload_started_at semantics (§9.3).
	if phase == state.PhaseUploading {
		a.recordUploadStart(t.now())
	}
}

// recordUploadStart stamps the activity's upload start time once. Repeated
// uploading phase transitions and out-of-band observe() initialization never
// reset the baseline (§3.2).
func (a *liveActivity) recordUploadStart(now time.Time) {
	if a.uploadStartedAt == nil {
		t0 := now
		a.uploadStartedAt = &t0
	}
}

// observe ingests one rclone progress frame and updates live state + throughput.
// phase is the current activity phase.
func (t *activityTracker) observe(profileID string, s state.ProgressSnapshot, measurable bool, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.live[profileID]
	if a == nil {
		return
	}
	a.filesCompleted = s.FilesCompleted
	a.bytesCompleted = s.BytesCompleted
	a.bytesTotal = s.BytesTotal
	a.checksCompleted = s.ChecksCompleted
	a.checksTotal = s.ChecksTotal
	a.itemsListed = s.ItemsListed
	a.errorsCount = s.ErrorsCount
	a.activeTransfers = s.ActiveTransfers
	if s.CurrentItem != nil {
		a.currentItem = *s.CurrentItem
	}
	a.currentItemBytes = s.CurrentItemBytes
	a.currentItemSize = s.CurrentItemSize
	if a.phase == "" {
		a.phase = state.PhaseUploading
		a.recordUploadStart(t.now())
	}
	if transferPhase(a.phase) {
		rate := a.estimator.Observe(s.BytesCompleted, now)
		a.speedKnown = rate.Known
		a.speedBytesPerSec = rate.BytesPerSecond

		fr := a.fileRate.Observe(s.FilesCompleted, now)
		a.filesPerMinKnown = fr.Known
		a.filesPerMinute = fr.FilesPerSec * 60
	}
	if measurable {
		a.lastProgress = now
		a.lastProgressValid = true
	}
}

// snapshot returns the current live activity view.
func (t *activityTracker) snapshot(profileID string) *live.ActivityS {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.live[profileID]
	if a == nil {
		return nil
	}
	out := &live.ActivityS{
		Kind:                a.kind,
		Phase:               a.phase,
		FilesCompleted:      a.filesCompleted,
		FilesTotal:          0,
		BytesCompleted:      a.bytesCompleted,
		BytesTotal:          a.bytesTotal,
		ChecksCompleted:     a.checksCompleted,
		ChecksTotal:         a.checksTotal,
		ItemsListed:         a.itemsListed,
		ErrorsCount:         a.errorsCount,
		CurrentItem:         a.currentItem,
		CurrentItemBytes:    a.currentItemBytes,
		CurrentItemSize:     a.currentItemSize,
		SpeedKnown:          a.speedKnown,
		SpeedBytesPerSecond: a.speedBytesPerSec,
		FilesPerMinuteKnown: a.filesPerMinKnown,
		FilesPerMinute:      a.filesPerMinute,
		ActiveTransfers:     a.activeTransfers,
	}
	if a.uploadStartedAt != nil {
		t0 := *a.uploadStartedAt
		out.UploadStartedAt = &t0
	}
	if a.runID != nil {
		rid := *a.runID
		out.RunID = &rid
	}
	if a.lastProgressValid {
		out.LastMeasurableProgressAt = a.lastProgress
		// Possible stall is a live-channel diagnosis only. It uses the
		// monotonic progress timestamp; 30m without measurable progress.
		out.PossibleStall = t.now().Sub(a.lastProgress) >= possibleStallAfter
	}
	return out
}

// finish removes the live activity for a profile.
func (t *activityTracker) finish(profileID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.live, profileID)
}

// transferPhase reports whether a phase can display live speed (§0.1).
func transferPhase(phase string) bool {
	switch phase {
	case state.PhaseUploading, state.PhaseDownloading, state.PhaseDeleting, state.PhaseReconciling:
		return true
	}
	return false
}

// activityToProgress converts a live activity snapshot into the durable
// ProgressSnapshot shape for coarse checkpoints (§9.3). Speed is deliberately
// omitted: durable checkpoints must not resurrect a stale live-looking speed.
func activityToProgress(a *live.ActivityS) state.ProgressSnapshot {
	var item *string
	if a.CurrentItem != "" {
		v := a.CurrentItem
		item = &v
	}
	return state.ProgressSnapshot{
		FilesCompleted:   a.FilesCompleted,
		BytesCompleted:   a.BytesCompleted,
		BytesTotal:       a.BytesTotal,
		ChecksCompleted:  a.ChecksCompleted,
		ChecksTotal:      a.ChecksTotal,
		ItemsListed:      a.ItemsListed,
		ErrorsCount:      a.ErrorsCount,
		CurrentItem:      item,
		CurrentItemBytes: a.CurrentItemBytes,
		CurrentItemSize:  a.CurrentItemSize,
		ActiveTransfers:  a.ActiveTransfers,
	}
}

// possibleStallAfter matches the existing 30m no-progress concept (§0.1).
const possibleStallAfter = 30 * time.Minute
