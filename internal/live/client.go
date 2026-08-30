package live

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// Observer connects to the worker's status socket and consumes the live status
// stream for one profile (§14). It is used by profile status/watch, wait, and
// generation waiters. When the socket is unavailable or protocol-incompatible,
// callers fall back to SQLite and periodically retry.
type Observer struct {
	// Path is the resolved socket path.
	Path string
	// ProfileID to subscribe to.
	ProfileID string
	// ConnectTimeout bounds the initial connection attempt.
	ConnectTimeout time.Duration
	// Dial is injectable for tests.
	Dial func(path string) (net.Conn, error)
}

// ErrUnavailable reports that live telemetry is unavailable (socket missing,
// connection refused, protocol mismatch, or server closed).
var ErrUnavailable = errors.New("live telemetry unavailable")

// Connect dials the socket and subscribes to the profile, returning a stream
// that immediately yields the initial full snapshot.
func (o *Observer) Connect() (*Stream, error) {
	if o.Dial == nil {
		o.Dial = func(path string) (net.Conn, error) {
			return net.DialTimeout("unix", path, o.connectTimeout())
		}
	}
	conn, err := o.Dial(o.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: connect %s: %v", ErrUnavailable, o.Path, err)
	}
	req, err := encode(MsgTypeSubscribe, o.ProfileID, Subscribe{ProfileID: o.ProfileID})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("%w: write subscribe: %v", ErrUnavailable, err)
	}
	stream := &Stream{conn: conn, reader: bufio.NewReader(conn)}
	return stream, nil
}

func (o *Observer) connectTimeout() time.Duration {
	if o.ConnectTimeout <= 0 {
		return 2 * time.Second
	}
	return o.ConnectTimeout
}

// Stream is a live status subscription. It yields full replacement snapshots.
type Stream struct {
	conn   net.Conn
	reader *bufio.Reader
}

// Next returns the next status snapshot, blocking until one arrives. It returns
// ErrUnavailable on EOF/protocol error.
func (s *Stream) Next() (*StatusSnapshot, error) {
	return s.readFrame()
}

func (s *Stream) readFrame() (*StatusSnapshot, error) {
	line, err := s.reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("%w: read: %v", ErrUnavailable, err)
	}
	msg, err := parseMessage(line)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	switch msg.Type {
	case MsgTypeStatus:
		var snap StatusSnapshot
		if err := json.Unmarshal(msg.Payload, &snap); err != nil {
			return nil, fmt.Errorf("%w: malformed status: %v", ErrUnavailable, err)
		}
		return &snap, nil
	case MsgTypeError:
		var e ErrorMessage
		_ = json.Unmarshal(msg.Payload, &e)
		return nil, fmt.Errorf("%w: server error %s: %s", ErrUnavailable, e.Code, e.Message)
	case MsgTypePong:
		// Not expected in a status stream; continue.
		return s.readFrame()
	default:
		return nil, fmt.Errorf("%w: unexpected message type %s", ErrUnavailable, msg.Type)
	}
}

// Close ends the subscription.
func (s *Stream) Close() error {
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

// SendInvalidate sends a best-effort wake/invalidate message for a profile
// (§5.4). Failure is ignored; SQLite remains authoritative.
func SendInvalidate(path, profileID string) {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	msg, _ := encode(MsgTypeInvalidate, profileID, Invalidate{ProfileID: profileID})
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(msg)
}

// SendPing probes whether a worker is listening at the socket path.
func SendPing(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	msg, _ := encode(MsgTypePing, "", nil)
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write(msg); err != nil {
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return false
	}
	m, err := parseMessage(line)
	return err == nil && m.Type == MsgTypePong
}

// ResolveObserver builds an Observer for a profile using the persisted socket
// setting via the shared resolver. dbSettingReader reads a setting value ("" if
// absent); the worker socket path key lives in state.SettingWorkerSocketPath.
func ResolveObserver(dbSettingReader func(string) string, profileID string) *Observer {
	return &Observer{Path: ResolveSocketPath(dbSettingReader("worker_socket_path")), ProfileID: profileID}
}
