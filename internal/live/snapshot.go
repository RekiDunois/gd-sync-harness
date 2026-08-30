package live

import "time"

// StatusSnapshot is the full, replacement-style public worker status for one
// profile (§5.2). It combines durable profile/sync state (SQLite) with the
// ephemeral live activity state (worker memory). Observers treat each snapshot
// as a complete replacement, never as a delta.
type StatusSnapshot struct {
	ProtocolVersion int        `json:"protocol_version"`
	Type            string     `json:"type"`
	ProfileID       string     `json:"profile_id"`
	SnapshotSeq     int64      `json:"snapshot_seq"`
	SampledAt       time.Time  `json:"sampled_at"`
	Profile         ProfileS   `json:"profile"`
	Sync            SyncS      `json:"sync"`
	Activity        *ActivityS `json:"activity,omitempty"`
}

// ProfileS is the durable profile lifecycle (§5.2).
type ProfileS struct {
	Enabled           bool `json:"enabled"`
	Tombstoned        bool `json:"tombstoned"`
	DeletionRequested bool `json:"deletion_requested"`
}

// SyncS is the durable synchronization lifecycle (§5.2).
type SyncS struct {
	Initialized           bool       `json:"initialized"`
	State                 string     `json:"state"`
	Phase                 *string    `json:"phase,omitempty"`
	DesiredGeneration     int64      `json:"desired_generation"`
	LastSuccessGeneration *int64     `json:"last_success_generation,omitempty"`
	CurrentRunID          *string    `json:"current_run_id,omitempty"`
	LastSuccessAt         *time.Time `json:"last_success_at,omitempty"`
	LastError             *string    `json:"last_error,omitempty"`
	RetryClassification   *string    `json:"retry_classification,omitempty"`
	NextRetryAt           *time.Time `json:"next_retry_at,omitempty"`
}

// ActivityS is the ephemeral live activity state (§5.2, §6.2).
type ActivityS struct {
	Kind                     string    `json:"kind"`
	RunID                    *string   `json:"run_id,omitempty"`
	Phase                    string    `json:"phase"`
	FilesCompleted           int64     `json:"files_completed"`
	FilesTotal               int64     `json:"files_total"`
	BytesCompleted           int64     `json:"bytes_completed"`
	BytesTotal               int64     `json:"bytes_total"`
	ChecksCompleted          int64     `json:"checks_completed"`
	ChecksTotal              int64     `json:"checks_total"`
	ItemsListed              int64     `json:"items_listed"`
	ErrorsCount              int64     `json:"errors_count"`
	CurrentItem              string    `json:"current_item,omitempty"`
	CurrentItemBytes         int64     `json:"current_item_bytes"`
	CurrentItemSize          int64     `json:"current_item_size"`
	SpeedKnown               bool      `json:"speed_known"`
	SpeedBytesPerSecond      float64   `json:"speed_bytes_per_second"`
	LastMeasurableProgressAt time.Time `json:"last_measurable_progress_at,omitempty"`
	PossibleStall            bool      `json:"possible_stall"`
	ActiveTransfers          int64     `json:"active_transfers"`
}

// VersionedSnapshot returns the snapshot with protocol fields set, ready for
// serialization.
func (s StatusSnapshot) Versioned() StatusSnapshot {
	s.ProtocolVersion = ProtocolVersion
	s.Type = MsgTypeStatus
	return s
}
