package cli

import (
	"testing"

	"knowledge-sync/internal/live"
	"knowledge-sync/internal/state"
)

func TestActivityToProgressPreservesCountersAndOptionalItem(t *testing.T) {
	got := activityToProgress(&live.ActivityS{
		FilesCompleted:   2,
		BytesCompleted:   3,
		BytesTotal:       4,
		ChecksCompleted:  5,
		ChecksTotal:      6,
		ItemsListed:      7,
		ErrorsCount:      8,
		CurrentItem:      "notes/a.md",
		CurrentItemBytes: 9,
		CurrentItemSize:  10,
		ActiveTransfers:  1,
	})
	if got.FilesCompleted != 2 || got.BytesCompleted != 3 || got.BytesTotal != 4 ||
		got.ChecksCompleted != 5 || got.ChecksTotal != 6 || got.ItemsListed != 7 ||
		got.ErrorsCount != 8 || got.CurrentItem == nil || *got.CurrentItem != "notes/a.md" ||
		got.CurrentItemBytes != 9 || got.CurrentItemSize != 10 || got.ActiveTransfers != 1 {
		t.Fatalf("activity conversion lost fields: %+v", got)
	}
	if got := activityToProgress(&live.ActivityS{}); got.CurrentItem != nil {
		t.Fatalf("empty current item = %q, want nil", *got.CurrentItem)
	}
}

func TestTerminalFromSnapshotLifecycleAndRetryBranches(t *testing.T) {
	terminalRetry := state.RetryTerminal
	cases := []struct {
		name    string
		snap    live.StatusSnapshot
		wantOK  bool
		wantErr bool
	}{
		{name: "ready", snap: live.StatusSnapshot{ProfileID: "ready", Sync: live.SyncS{State: state.StateReady}}, wantOK: true},
		{name: "tombstoned", snap: live.StatusSnapshot{ProfileID: "gone", Profile: live.ProfileS{Tombstoned: true}}, wantErr: true},
		{name: "deleting", snap: live.StatusSnapshot{ProfileID: "deleting", Profile: live.ProfileS{DeletionRequested: true}}, wantErr: true},
		{name: "disabled", snap: live.StatusSnapshot{ProfileID: "disabled", Profile: live.ProfileS{Enabled: false}}, wantErr: true},
		{name: "terminal retry", snap: live.StatusSnapshot{ProfileID: "broken", Sync: live.SyncS{State: state.StateError, RetryClassification: &terminalRetry}}, wantErr: true},
		{name: "nonterminal", snap: live.StatusSnapshot{ProfileID: "running", Profile: live.ProfileS{Enabled: true}, Sync: live.SyncS{State: state.StateSyncing}}, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOK, err := terminalFromSnapshot(&tc.snap)
			if gotOK != tc.wantOK || (err != nil) != tc.wantErr {
				t.Fatalf("terminalFromSnapshot() = (%v, %v), want (%v, error=%v)", gotOK, err, tc.wantOK, tc.wantErr)
			}
		})
	}
}

func TestStringOrUsesFallbackOnlyForMissingValues(t *testing.T) {
	if got := stringOr(nil, "fallback"); got != "fallback" {
		t.Fatalf("nil string = %q", got)
	}
	empty := ""
	if got := stringOr(&empty, "fallback"); got != "fallback" {
		t.Fatalf("empty string = %q", got)
	}
	value := "value"
	if got := stringOr(&value, "fallback"); got != "value" {
		t.Fatalf("value string = %q", got)
	}
}
