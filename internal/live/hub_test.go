package live

import "testing"

func TestHubDeliversNewestSnapshotAndHoldsInitialRace(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe("profile")
	hub.Publish(StatusSnapshot{ProfileID: "profile", SnapshotSeq: 1})
	hub.Publish(StatusSnapshot{ProfileID: "profile", SnapshotSeq: 2})
	if got := <-ch; got.SnapshotSeq != 2 {
		t.Fatalf("latest snapshot sequence = %d, want 2", got.SnapshotSeq)
	}

	// A publish between registration and the initial snapshot must wait for the
	// handoff and may be restamped by the server.
	sub := hub.beginSubscribe("profile")
	hub.Publish(StatusSnapshot{ProfileID: "profile", SnapshotSeq: 3})
	hub.finishSubscribe(sub, func(s StatusSnapshot) StatusSnapshot {
		s.SnapshotSeq = 4
		return s
	})
	if got := <-sub.ch; got.SnapshotSeq != 4 {
		t.Fatalf("restamped initial snapshot sequence = %d, want 4", got.SnapshotSeq)
	}
	hub.cancelSubscribe(sub)
	if got := <-ch; got.SnapshotSeq != 3 {
		t.Fatalf("first subscriber snapshot sequence = %d, want 3", got.SnapshotSeq)
	}

	hub.Unsubscribe("profile", ch)
	hub.Publish(StatusSnapshot{ProfileID: "profile", SnapshotSeq: 5})
	select {
	case got := <-ch:
		t.Fatalf("unsubscribed channel received snapshot %d", got.SnapshotSeq)
	default:
	}
}

func TestHubUnsubscribeIgnoresUnknownChannel(t *testing.T) {
	hub := NewHub()
	hub.Subscribe("profile")
	unknown := make(chan StatusSnapshot, 1)
	hub.Unsubscribe("profile", unknown)
	if len(hub.subscribers["profile"]) != 1 {
		t.Fatalf("unknown channel removed a subscriber")
	}
}
