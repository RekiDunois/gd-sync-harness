package live

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the worker status socket protocol version (§5). Client and
// worker must agree on this version or the observer falls back to SQLite.
const ProtocolVersion = 1

// Message kinds.
const (
	MsgTypeSubscribe   = "subscribe"
	MsgTypeStatus      = "status"
	MsgTypeError       = "error"
	MsgTypeInvalidate  = "invalidate"
	MsgTypeRequestSync = "request_sync"
	MsgTypePing        = "ping"
	MsgTypePong        = "pong"
)

// Activity kinds carried by a status snapshot (§5.3).
const (
	ActivityFullReconcile = "full_reconcile"
	ActivityFastUpsert    = "fast_upsert"
	ActivityPrune         = "prune"
	ActivityDerived       = "derived_sync"
)

// Message is the envelope for every NDJSON frame on the socket. Unknown fields
// are ignored for forward compatibility; unknown types produce an error
// response rather than crashing the worker (§5).
type Message struct {
	ProtocolVersion int             `json:"protocol_version"`
	Type            string          `json:"type"`
	ProfileID       string          `json:"profile_id,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
}

// Subscribe is a client request for the full status stream of one profile.
type Subscribe struct {
	ProfileID string `json:"profile_id"`
}

// Invalidate is a best-effort worker wake/invalidation request sent by CLI
// mutation commands after their durable SQLite commit (§5.4).
type Invalidate struct {
	ProfileID string `json:"profile_id"`
}

// RequestSync asks the worker to persist a durable reconciliation/sync request
// and wake the relevant profile (§12). The worker performs the durable
// submission itself so intent authority stays with SQLite.
type RequestSync struct {
	ProfileID      string `json:"profile_id"`
	Scheduled      bool   `json:"scheduled"`
	AllowDeletes   int    `json:"allow_deletes,omitempty"`
	BypassDebounce bool   `json:"bypass_debounce,omitempty"`
	RequestedGen   int64  `json:"requested_generation,omitempty"`
	WorkerResponse bool   `json:"worker_response,omitempty"`
}

// encode marshals a message as one NDJSON line.
func encode(t string, profileID string, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	msg := Message{ProtocolVersion: ProtocolVersion, Type: t, ProfileID: profileID, Payload: raw}
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// encodeError builds a stable error response (§5.5). Protocol mismatches and
// unknown profiles are represented as error messages so clients fall back to
// SQLite without failing the whole command.
func encodeError(profileID string, code string, detail string) ([]byte, error) {
	return encode(MsgTypeError, profileID, ErrorMessage{Code: code, Message: detail})
}

// ErrorMessage is the stable error body.
type ErrorMessage struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Status codes for error responses.
const (
	ErrCodeUnknownProfile   = "unknown_profile"
	ErrCodeProtocolMismatch = "protocol_mismatch"
	ErrCodeUnknownType      = "unknown_type"
	ErrCodeBadRequest       = "bad_request"
	ErrCodeUnavailable      = "unavailable"
)

// statusPayload builds a full status message.
func statusPayload(s StatusSnapshot) ([]byte, error) {
	return encode(MsgTypeStatus, s.ProfileID, s)
}

// parseMessage decodes one envelope, validating the protocol version.
func parseMessage(line []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	if m.ProtocolVersion != ProtocolVersion {
		return nil, fmt.Errorf("protocol mismatch: got version %d, want %d", m.ProtocolVersion, ProtocolVersion)
	}
	return &m, nil
}
