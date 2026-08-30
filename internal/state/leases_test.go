package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoteLeaseBoundsSharedRemoteAndRecoversExpiry(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "leases.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.AcquireRemoteLease(ctx, "example-remote", 1, 1, 1, "first"); err != nil {
		t.Fatal(err)
	}
	second := make(chan error, 1)
	go func() {
		second <- db.AcquireRemoteLease(ctx, "example-remote", 10, 1, 2, "second")
	}()
	if err := db.ReleaseRemoteLease("first"); err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second lease did not proceed after release: %v", err)
	}
	if err := db.ReleaseRemoteLease("second"); err != nil {
		t.Fatal(err)
	}

	if err := db.AcquireRemoteLease(ctx, "example-remote", 1, 1, 1, "expired"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE remote_operation_leases SET lease_until = ? WHERE id = 'expired'`,
		Now().Add(-time.Minute).Format(timeFmt)); err != nil {
		t.Fatal(err)
	}
	if err := db.AcquireRemoteLease(ctx, "example-remote", 1, 1, 2, "recovered"); err != nil {
		t.Fatalf("expired lease was not recoverable: %v", err)
	}
	_ = db.ReleaseRemoteLease("recovered")
}

func TestRemoteLeasesAllowIndependentRemotes(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "leases.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.AcquireRemoteLease(ctx, "remote-a", 1, 1, 1, "a"); err != nil {
		t.Fatal(err)
	}
	if err := db.AcquireRemoteLease(ctx, "remote-b", 1, 1, 1, "b"); err != nil {
		t.Fatal(err)
	}
	_ = db.ReleaseRemoteLease("a")
	_ = db.ReleaseRemoteLease("b")
}
