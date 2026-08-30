package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// Server is the worker's Unix socket status server (§6). It serves one full,
// replacement-style status snapshot per profile on demand and fans live updates
// out to subscribers. The server never performs SQLite reads for live frames:
// the DurableReader is only invoked on subscribe (one fresh durable read) and
// on explicit refresh/invalidate events.
type Server struct {
	path      string
	isDefault bool
	reader    DurableReader
	hub       *Hub
	seq       *seqCounter
	clock     SampleClock
	log       *log.Logger

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   bool
}

// ServerOptions configures a Server.
type ServerOptions struct {
	Path      string
	IsDefault bool
	Reader    DurableReader
	Hub       *Hub
	Clock     SampleClock
	Log       *log.Logger
}

// NewServer builds a server that resolves the socket path from the persisted
// setting via the shared resolver.
func NewServer(opts ServerOptions) *Server {
	if opts.Hub == nil {
		opts.Hub = NewHub()
	}
	if opts.Clock == nil {
		opts.Clock = NowClock
	}
	if opts.Log == nil {
		opts.Log = log.New(os.Stderr, "live: ", 0)
	}
	return &Server{
		path: opts.Path, isDefault: opts.IsDefault,
		reader: opts.Reader, hub: opts.Hub, seq: &seqCounter{},
		clock: opts.Clock, log: opts.Log, conns: map[net.Conn]struct{}{},
	}
}

// Path returns the socket path the server listens on.
func (s *Server) Path() string { return s.path }

// Start prepares the runtime directory, acquires the listener with stale-socket
// protection (§4.4), and begins accepting connections. Start must be called
// only after the worker singleton lock is held.
func (s *Server) Start() error {
	if err := PrepareSocketDir(s.path, s.isDefault); err != nil {
		return err
	}
	switch ClassifySocketPath(s.path) {
	case StaleSocketMissing:
		// nothing to clean up
	case StaleSocketUnixSocket:
		// The worker singleton lock is held, so an existing socket cannot
		// belong to another legitimate worker using the same state database.
		if err := os.Remove(s.path); err != nil {
			return fmt.Errorf("remove stale socket %s: %w", s.path, err)
		}
	case StaleSocketUnsafe:
		return fmt.Errorf("refusing to replace non-socket path %s (regular file, symlink, or directory)", s.path)
	}
	l, err := net.Listen("unix", s.path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.path, err)
	}
	_ = os.Chmod(s.path, 0o600)
	s.mu.Lock()
	s.listener = l
	s.mu.Unlock()
	s.wg.Add(1)
	go s.acceptLoop(l)
	return nil
}

// Stop closes the listener and removes the socket path only if it still refers
// to the instance owned by this process (§4.4). Cleanup failure is logged, not
// fatal.
func (s *Server) Stop() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	l := s.listener
	s.listener = nil
	s.mu.Unlock()

	var firstErr error
	if l != nil {
		if err := l.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// Close all active connections so connection goroutines unblock and exit;
	// the reader loop ends and the client falls back to SQLite.
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	// Remove the socket path only if it is still a socket owned by us.
	fi, err := os.Lstat(s.path)
	if err == nil && fi.Mode()&os.ModeSocket != 0 {
		if err := os.Remove(s.path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.wg.Wait()
	return firstErr
}

func (s *Server) acceptLoop(l net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			// Transient accept error; brief backoff then continue.
			time.Sleep(20 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// PublishActivity assembles and broadcasts a snapshot for a profile from the
// current activity. It performs no durable read. A nil activity still produces
// a snapshot from the cached durable state.
func (s *Server) PublishActivity(profileID string, activity *ActivityS) {
	snapshot := s.reader.BuildSnapshot(profileID, activity)
	if snapshot == nil {
		return
	}
	s.publish(*snapshot)
}

// PublishDurableRefresh re-validates the durable cache and broadcasts a fresh
// snapshot (used after worker-owned durable mutations and external invalidate
// messages).
func (s *Server) PublishDurableRefresh(profileID string) {
	s.reader.Refresh(profileID)
	s.PublishActivity(profileID, nil)
}

func (s *Server) publish(snapshot StatusSnapshot) {
	s.hub.Publish(s.versioned(snapshot))
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		conn.Close()
	}()
	connDone := make(chan struct{})
	defer close(connDone)
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		msg, err := parseMessage(line)
		if err != nil {
			// Incompatible protocol: report it and close so the client falls
			// back to SQLite (§5.5). Never crash the worker.
			resp, _ := encodeError("", ErrCodeProtocolMismatch, err.Error())
			_ = writeLine(conn, resp)
			return
		}
		switch msg.Type {
		case MsgTypeSubscribe:
			// Run the subscription stream in its own goroutine so the scanner
			// keeps watching the connection. When the peer disconnects, Scan
			// returns, handleConn closes connDone, and the subscriber loop
			// exits instead of blocking forever on the hub channel.
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleSubscribe(conn, msg, connDone)
			}()
		case MsgTypeInvalidate:
			s.handleInvalidate(msg)
		case MsgTypePing:
			_ = writeLine(conn, mustEncodePong())
		default:
			resp, _ := encodeError(msg.ProfileID, ErrCodeUnknownType, "unknown message type "+msg.Type)
			_ = writeLine(conn, resp)
		}
	}
}

func (s *Server) handleSubscribe(conn net.Conn, msg *Message, done <-chan struct{}) {
	sub := Subscribe{}
	if err := json.Unmarshal(msg.Payload, &sub); err != nil || sub.ProfileID == "" {
		resp, _ := encodeError(msg.ProfileID, ErrCodeBadRequest, "malformed subscribe request")
		_ = writeLine(conn, resp)
		return
	}
	// One fresh durable read for this profile, then an immediate full snapshot
	// (§6.4). The subscriber must not wait for the next rclone frame. The
	// initial snapshot is written directly to this connection (not broadcast
	// through the hub, so other subscribers are not spammed with it).
	s.reader.Refresh(sub.ProfileID)
	snapshot := s.reader.BuildSnapshot(sub.ProfileID, nil)
	if snapshot == nil {
		resp, _ := encodeError(sub.ProfileID, ErrCodeUnknownProfile, "no such profile")
		_ = writeLine(conn, resp)
		return
	}
	initial := s.versioned(*snapshot)
	b, err := statusPayload(initial)
	if err != nil {
		return
	}
	if err := writeLine(conn, b); err != nil {
		return
	}
	ch := s.hub.Subscribe(sub.ProfileID)
	defer s.hub.Unsubscribe(sub.ProfileID, ch)
	for {
		select {
		case sn, ok := <-ch:
			if !ok {
				return
			}
			b, err := statusPayload(sn)
			if err != nil {
				return
			}
			if err := writeLine(conn, b); err != nil {
				// A slow/failed client disconnects only that client (§5.6).
				return
			}
		case <-done:
			return
		}
	}
}

// versioned returns the snapshot with protocol fields, sequence number, and
// sample time stamped.
func (s *Server) versioned(snapshot StatusSnapshot) StatusSnapshot {
	snapshot = snapshot.Versioned()
	snapshot.SnapshotSeq = s.seq.Next()
	snapshot.SampledAt = s.clock()
	return snapshot
}

func (s *Server) handleInvalidate(msg *Message) {
	// The reader Refresh is atomic; an invalidate simply marks the durable
	// cache dirty and re-broadcasts. Durable SQLite remains authoritative.
	if msg.ProfileID == "" {
		return
	}
	s.PublishDurableRefresh(msg.ProfileID)
}

func writeLine(conn net.Conn, b []byte) error {
	_, err := conn.Write(b)
	return err
}

func mustEncodePong() []byte {
	b, err := encode(MsgTypePong, "", nil)
	if err != nil {
		return []byte("{}\n")
	}
	return b
}
