// Package protocol defines the wire format of the connection between the
// control端 and its agents.
//
// The connection is a sequence of JSON frames. Every frame carries the
// version of the protocol, a type, a sequence number and an optional
// payload. The control端 accepts only the registered message types, so an
// agent of a later stage cannot make the control端 of an earlier stage
// believe that it understands a message it does not know.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// WireVersion is the version of the protocol the control端 speaks.
const WireVersion = 1

// MaxPayloadSize is the largest payload a single message may carry. An
// inventory that does not fit into one payload is sent in several batches,
// so a payload that exceeds the limit is refused instead of being cut.
const MaxPayloadSize = 1024 * 1024

// MaxMessageSize is the largest frame the control端 reads from a connection.
// It leaves room for the envelope around a payload of the maximum size.
const MaxMessageSize = MaxPayloadSize + 64*1024

// The message types of the protocol. A later stage extends the registry
// explicitly, so every stage accepts exactly the messages it understands.
const (
	// TypeHello opens the conversation of a connection. It names the
	// identity of the agent.
	TypeHello = "hello"
	// TypeHeartbeat reports that the agent is alive.
	TypeHeartbeat = "heartbeat"
	// TypeInventory carries one batch of an inventory snapshot.
	TypeInventory = "inventory"
)

// The errors of the protocol.
var (
	// ErrUnsupportedVersion reports a frame of an unknown version.
	ErrUnsupportedVersion = errors.New("protocol: unsupported message version")
	// ErrUnsupportedType reports a frame of a type that is not registered.
	ErrUnsupportedType = errors.New("protocol: unsupported message type")
	// ErrPayloadTooLarge reports a frame whose payload exceeds the limit.
	ErrPayloadTooLarge = errors.New("protocol: the message payload is too large")
)

// Envelope is the frame of the protocol.
type Envelope struct {
	// Version is the version of the protocol the frame belongs to.
	Version int `json:"version"`
	// Type names the message of the frame.
	Type string `json:"type"`
	// RequestID ties a frame to the request that caused it.
	RequestID string `json:"request_id,omitempty"`
	// Seq counts the frames of a connection. It grows by one per frame.
	Seq uint64 `json:"seq"`
	// Payload holds the message of the frame.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// registeredTypes holds the message types the control端 accepts. A later
// stage adds its types here.
var registeredTypes = map[string]bool{
	TypeHello:     true,
	TypeHeartbeat: true,
	TypeInventory: true,
}

// IsRegisteredType reports whether the control端 accepts the given message
// type.
func IsRegisteredType(messageType string) bool {
	return registeredTypes[messageType]
}

// RegisteredTypes returns the accepted message types.
func RegisteredTypes() []string {
	return []string{TypeHello, TypeHeartbeat, TypeInventory}
}

// Validate checks a frame that arrives from an agent. A frame of an
// unknown version, of an unknown type or with an oversized payload is
// refused, so the connection is closed instead of acting on a message the
// control端 does not understand.
func (e Envelope) Validate() error {
	if e.Version != WireVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, e.Version)
	}
	if len(e.Payload) > MaxPayloadSize {
		return fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, len(e.Payload))
	}
	if !IsRegisteredType(e.Type) {
		return fmt.Errorf("%w: %s", ErrUnsupportedType, e.Type)
	}
	return nil
}

// Encode turns an envelope into the bytes of a frame.
func Encode(envelope Envelope) ([]byte, error) {
	data, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("protocol: encode the envelope: %w", err)
	}
	return data, nil
}

// Decode reads an envelope from the bytes of a frame.
func Decode(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("protocol: decode the envelope: %w", err)
	}
	return envelope, nil
}
