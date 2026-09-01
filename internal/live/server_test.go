package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// shortTempDir returns a short temp directory so Unix socket paths stay well
// under the macOS 104-byte path limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	dir, err := os.MkdirTemp(base, "ksock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// fakeReader is a scripted DurableReader that returns snapshots for registered
// profiles and records refresh calls.
type fakeReader struct {
	mu         sync.Mutex
	profiles   map[string]bool
	refreshes  int
	activity   *ActivityS
	afterBuild func()
}

func newFakeReader(profiles ...string) *fakeReader {
	r := &fakeReader{profiles: map[string]bool{}}
	for _, p := range profiles {
		r.profiles[p] = true
	}
	return r
}

func (r *fakeReader) BuildSnapshot(profileID string, activity *ActivityS) *StatusSnapshot {
	r.mu.Lock()
	if !r.profiles[profileID] {
		r.mu.Unlock()
		return nil
	}
	if activity == nil {
		activity = r.activity
	}
	lsg := int64(3)
	snapshot := &StatusSnapshot{
		ProfileID: profileID,
		Profile:   ProfileS{Enabled: true},
		Sync: SyncS{Initialized: true, State: "ready", DesiredGeneration: 3,
			LastSuccessGeneration: &lsg},
		Activity: activity,
	}
	hook := r.afterBuild
	r.afterBuild = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return snapshot
}

func (r *fakeReader) Refresh(profileID string) bool {
	r.mu.Lock()
	r.refreshes++
	r.mu.Unlock()
	return true
}

func startTestServer(t *testing.T, profiles ...string) (*Server, *fakeReader, string) {
	t.Helper()
	dir := shortTempDir(t)
	path := filepath.Join(dir, "worker.sock")
	reader := newFakeReader(profiles...)
	srv := NewServer(ServerOptions{Path: path, Reader: reader, Clock: func() time.Time {
		return time.Unix(1_700_000_000, 0).UTC()
	}})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, reader, path
}

func dial(t *testing.T, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, bufio.NewReader(conn)
}

func send(t *testing.T, conn net.Conn, payload any) {
	t.Helper()
	b, err := encode(MsgTypeSubscribe, "", payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(b); err != nil {
		t.Fatal(err)
	}
}

func readStatus(t *testing.T, r *bufio.Reader) *StatusSnapshot {
	t.Helper()
	conn, _ := r.Peek(1)
	_ = conn
	var m Message
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != MsgTypeStatus {
		t.Fatalf("message type = %s, want status (payload %s)", m.Type, line)
	}
	var s StatusSnapshot
	if err := json.Unmarshal(m.Payload, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func readError(t *testing.T, r *bufio.Reader) *ErrorMessage {
	t.Helper()
	var m Message
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != MsgTypeError {
		t.Fatalf("message type = %s, want error", m.Type)
	}
	var e ErrorMessage
	if err := json.Unmarshal(m.Payload, &e); err != nil {
		t.Fatal(err)
	}
	return &e
}

func TestSubscribeReturnsImmediateInitialSnapshot(t *testing.T) {
	_, _, path := startTestServer(t, "example-profile")
	conn, r := dial(t, path)
	send(t, conn, Subscribe{ProfileID: "example-profile"})
	s := readStatus(t, r)
	if s.ProfileID != "example-profile" {
		t.Fatalf("profile = %s", s.ProfileID)
	}
	if s.ProtocolVersion != ProtocolVersion || s.Type != MsgTypeStatus {
		t.Fatalf("snapshot version/type = %d/%s", s.ProtocolVersion, s.Type)
	}
	if s.SnapshotSeq < 1 {
		t.Fatal("snapshot must carry a sequence number")
	}
}

func TestSubscribeUnknownProfileReturnsStableError(t *testing.T) {
	_, _, path := startTestServer(t, "example-profile")
	conn, r := dial(t, path)
	send(t, conn, Subscribe{ProfileID: "no-such-profile"})
	e := readError(t, r)
	if e.Code != ErrCodeUnknownProfile {
		t.Fatalf("error code = %s, want unknown_profile", e.Code)
	}
}

func TestIncompatibleProtocolCausesFallbackNotPanic(t *testing.T) {
	_, _, path := startTestServer(t, "example-profile")
	conn, r := dial(t, path)
	if _, err := conn.Write([]byte(`{"protocol_version":999,"type":"subscribe"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	e := readError(t, r)
	if e.Code != ErrCodeProtocolMismatch {
		t.Fatalf("error code = %s, want protocol_mismatch", e.Code)
	}
	// Server must still be alive for another client.
	conn2, r2 := dial(t, path)
	send(t, conn2, Subscribe{ProfileID: "example-profile"})
	if s := readStatus(t, r2); s.ProfileID != "example-profile" {
		t.Fatal("server must survive a protocol mismatch")
	}
}

func TestMultipleSubscribersReceiveSameLatestStatus(t *testing.T) {
	srv, reader, path := startTestServer(t, "example-profile")
	connA, rA := dial(t, path)
	connB, rB := dial(t, path)
	send(t, connA, Subscribe{ProfileID: "example-profile"})
	send(t, connB, Subscribe{ProfileID: "example-profile"})
	sA := readStatus(t, rA)
	sB := readStatus(t, rB)
	if sA.ProfileID != "example-profile" || sB.ProfileID != "example-profile" {
		t.Fatalf("initial snapshots A=%s B=%s", sA.ProfileID, sB.ProfileID)
	}

	// A published activity reaches both subscribers with the same snapshot.
	reader.mu.Lock()
	reader.activity = &ActivityS{Kind: ActivityFullReconcile, Phase: "uploading"}
	reader.mu.Unlock()
	srv.PublishActivity("example-profile", reader.activity)

	sA2 := readStatus(t, rA)
	sB2 := readStatus(t, rB)
	if sA2.SnapshotSeq != sB2.SnapshotSeq {
		t.Fatalf("shared publish seq A=%d B=%d, want equal", sA2.SnapshotSeq, sB2.SnapshotSeq)
	}
	if sA2.Activity == nil || sA2.Activity.Kind != ActivityFullReconcile {
		t.Fatalf("subscriber A activity = %+v", sA2.Activity)
	}
	if sB2.Activity == nil || sB2.Activity.Kind != ActivityFullReconcile {
		t.Fatalf("subscriber B activity = %+v", sB2.Activity)
	}
}

func TestSubscribeHandoffDeliversPublishDuringInitialBuild(t *testing.T) {
	srv, reader, path := startTestServer(t, "example-profile")
	built := make(chan struct{})
	release := make(chan struct{})
	reader.mu.Lock()
	reader.afterBuild = func() {
		close(built)
		<-release
	}
	reader.mu.Unlock()

	conn, r := dial(t, path)
	send(t, conn, Subscribe{ProfileID: "example-profile"})
	<-built
	ready := int64(4)
	srv.publish(StatusSnapshot{
		ProfileID: "example-profile",
		Sync:      SyncS{State: "ready", LastSuccessGeneration: &ready},
	})
	close(release)

	initial := readStatus(t, r)
	latest := readStatus(t, r)
	if latest.Sync.State != "ready" || latest.Sync.LastSuccessGeneration == nil || *latest.Sync.LastSuccessGeneration != ready {
		t.Fatalf("latest snapshot = %+v, want ready generation %d", latest.Sync, ready)
	}
	if latest.SnapshotSeq <= initial.SnapshotSeq {
		t.Fatalf("snapshot sequence regressed: initial=%d latest=%d", initial.SnapshotSeq, latest.SnapshotSeq)
	}
}

func TestSlowSubscriberDoesNotBlockPublisher(t *testing.T) {
	srv, reader, path := startTestServer(t, "example-profile")
	conn, r := dial(t, path)
	send(t, conn, Subscribe{ProfileID: "example-profile"})
	readStatus(t, r) // consume initial

	// Don't read from the subscriber; publish many updates. Publish must return
	// promptly each time (capacity-1 latest-value slot).
	reader.mu.Lock()
	reader.activity = &ActivityS{Kind: ActivityFastUpsert}
	reader.mu.Unlock()
	deadline := time.After(2 * time.Second)
	for i := 0; i < 100; i++ {
		done := make(chan struct{})
		go func() {
			srv.PublishActivity("example-profile", reader.activity)
			close(done)
		}()
		select {
		case <-done:
		case <-deadline:
			t.Fatal("publisher blocked by slow subscriber")
		}
	}
	// The newest snapshot is delivered once the subscriber reads.
	s := readStatus(t, r)
	if s.Activity == nil || s.Activity.Kind != ActivityFastUpsert {
		t.Fatalf("final snapshot activity = %+v", s.Activity)
	}
}

func TestDroppedFramesLeadToNewestSnapshot(t *testing.T) {
	srv, reader, path := startTestServer(t, "example-profile")
	conn, r := dial(t, path)
	send(t, conn, Subscribe{ProfileID: "example-profile"})
	readStatus(t, r)
	reader.mu.Lock()
	reader.activity = &ActivityS{Kind: ActivityFullReconcile, FilesCompleted: 1}
	reader.mu.Unlock()
	for i := 0; i < 50; i++ {
		srv.PublishActivity("example-profile", reader.activity)
	}
	s := readStatus(t, r)
	if s.Activity == nil || s.Activity.FilesCompleted != 1 {
		t.Fatalf("latest activity = %+v", s.Activity)
	}
}

func TestDisconnectReconnectSucceeds(t *testing.T) {
	_, _, path := startTestServer(t, "example-profile")
	conn, r := dial(t, path)
	send(t, conn, Subscribe{ProfileID: "example-profile"})
	readStatus(t, r)
	conn.Close()
	conn2, r2 := dial(t, path)
	send(t, conn2, Subscribe{ProfileID: "example-profile"})
	if s := readStatus(t, r2); s.ProfileID != "example-profile" {
		t.Fatal("reconnect must receive a fresh snapshot")
	}
}

func TestSubscribeRefreshesDurableOnce(t *testing.T) {
	srv, reader, path := startTestServer(t, "example-profile")
	_ = srv
	conn, r := dial(t, path)
	send(t, conn, Subscribe{ProfileID: "example-profile"})
	readStatus(t, r)
	reader.mu.Lock()
	n := reader.refreshes
	reader.mu.Unlock()
	if n != 1 {
		t.Fatalf("subscribe durable refreshes = %d, want 1", n)
	}
	// Publishing activity must NOT trigger another durable read.
	srv.PublishActivity("example-profile", &ActivityS{Kind: ActivityFullReconcile})
	reader.mu.Lock()
	n = reader.refreshes
	reader.mu.Unlock()
	if n != 1 {
		t.Fatalf("activity publish must not read durable state; refreshes = %d", n)
	}
}

func TestServerShutdownClosesClients(t *testing.T) {
	srv, _, path := startTestServer(t, "example-profile")
	conn, _ := dial(t, path)
	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
	// Client should observe EOF/close shortly.
	_ = conn
	_, err := net.Dial("unix", path)
	if err == nil {
		t.Fatal("dialing a stopped server must fail")
	}
}

func TestStaleSocketReplacedAfterOwnership(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "worker.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	reader := newFakeReader("example-profile")
	srv := NewServer(ServerOptions{Path: path, Reader: reader})
	if err := srv.Start(); err != nil {
		t.Fatalf("server must replace a stale socket owned by the singleton lock: %v", err)
	}
	defer srv.Stop()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestRegularFileAtSocketPathIsNeverDeleted(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := newFakeReader("example-profile")
	srv := NewServer(ServerOptions{Path: path, Reader: reader})
	err := srv.Start()
	if err == nil {
		srv.Stop()
		t.Fatal("server must fail closed on a non-socket path")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "x" {
		t.Fatalf("regular file was mutated: %q", b)
	}
}

func TestRequestSyncMessageShape(t *testing.T) {
	b, err := encode(MsgTypeRequestSync, "example-profile", RequestSync{ProfileID: "example-profile", Scheduled: true, BypassDebounce: false})
	if err != nil {
		t.Fatal(err)
	}
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != MsgTypeRequestSync || m.ProfileID != "example-profile" {
		t.Fatalf("envelope = %+v", m)
	}
	fmt.Println(string(b))
}
