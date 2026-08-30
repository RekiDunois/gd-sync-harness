package live

import (
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// clientTestServer starts a real server with the fake reader.
func clientTestServer(t *testing.T) (string, *fakeReader, *Server) {
	t.Helper()
	dir := shortTempDir(t)
	path := filepath.Join(dir, "worker.sock")
	reader := newFakeReader("example-profile")
	srv := NewServer(ServerOptions{Path: path, Reader: reader, Clock: func() time.Time {
		return time.Unix(1_700_000_000, 0).UTC()
	}})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return path, reader, srv
}

func TestObserverGetsImmediateSnapshot(t *testing.T) {
	path, _, _ := clientTestServer(t)
	obs := &Observer{Path: path, ProfileID: "example-profile"}
	stream, err := obs.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	snap, err := stream.Next()
	if err != nil {
		t.Fatal(err)
	}
	if snap.ProfileID != "example-profile" {
		t.Fatalf("initial snapshot profile = %s", snap.ProfileID)
	}
}

func TestObserverUnavailableWhenNoSocket(t *testing.T) {
	obs := &Observer{Path: filepath.Join(shortTempDir(t), "missing.sock"), ProfileID: "example-profile", ConnectTimeout: 300 * time.Millisecond}
	_, err := obs.Connect()
	if err == nil {
		t.Fatal("connect to missing socket must fail")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

func TestObserverProtocolMismatchFallsBack(t *testing.T) {
	path, _, _ := clientTestServer(t)
	obs := &Observer{
		Path: path, ProfileID: "example-profile",
		Dial: func(p string) (net.Conn, error) {
			conn, err := net.Dial("unix", p)
			if err != nil {
				return nil, err
			}
			// Send an incompatible subscribe then a valid one; the server
			// replies protocol_mismatch and closes. The client must surface
			// ErrUnavailable, not panic.
			conn.Write([]byte(`{"protocol_version":99,"type":"subscribe","payload":{}` + "\n"))
			return conn, nil
		},
	}
	stream, err := obs.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next()
	if err == nil {
		t.Fatal("protocol mismatch must produce an error")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
}

func TestObserverUnknownProfileReturnsUnavailable(t *testing.T) {
	path, _, _ := clientTestServer(t)
	obs := &Observer{Path: path, ProfileID: "no-such-profile"}
	stream, err := obs.Connect()
	if err != nil {
		t.Fatal(err) // connect succeeds; the error arrives on first read
	}
	defer stream.Close()
	_, err = stream.Next()
	if err == nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown profile error = %v", err)
	}
}

func TestObserverStreamsLiveUpdates(t *testing.T) {
	path, reader, srv := clientTestServer(t)
	obs := &Observer{Path: path, ProfileID: "example-profile"}
	stream, err := obs.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	reader.mu.Lock()
	reader.activity = &ActivityS{Kind: ActivityFullReconcile, FilesCompleted: 7}
	reader.mu.Unlock()
	srv.PublishActivity("example-profile", reader.activity)

	// The published snapshot arrives on the stream.
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, err := stream.Next()
		if err != nil {
			t.Fatal(err)
		}
		if snap.Activity != nil && snap.Activity.FilesCompleted == 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for live update")
		}
	}
}

func (r *fakeReader) refreshCount() int { r.mu.Lock(); defer r.mu.Unlock(); return r.refreshes }

func TestSendInvalidateReachesServer(t *testing.T) {
	path, reader, _ := clientTestServer(t)
	before := reader.refreshCount()
	SendInvalidate(path, "example-profile")
	deadline := time.Now().Add(3 * time.Second)
	for reader.refreshCount() <= before {
		if time.Now().After(deadline) {
			t.Fatal("invalidate did not reach server")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSendPing(t *testing.T) {
	path, _, _ := clientTestServer(t)
	if !SendPing(path) {
		t.Fatal("ping to live server must succeed")
	}
	if SendPing(filepath.Join(shortTempDir(t), "missing.sock")) {
		t.Fatal("ping to missing socket must fail")
	}
}

func TestObserverReconnectAfterServerRestart(t *testing.T) {
	path, _, _ := clientTestServer(t)
	obs := &Observer{Path: path, ProfileID: "example-profile"}
	stream, err := obs.Connect()
	if err != nil {
		t.Fatal(err)
	}
	stream.Close()

	// New server on the same path (stale socket replaced).
	reader := newFakeReader("example-profile")
	srv2 := NewServer(ServerOptions{Path: path, Reader: reader})
	if err := srv2.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv2.Stop()
	stream2, err := obs.Connect()
	if err != nil {
		t.Fatalf("reconnect after restart: %v", err)
	}
	stream2.Close()
}
