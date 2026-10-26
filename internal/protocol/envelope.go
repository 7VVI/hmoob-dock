// Package protocol defines the wire format of the connection between the
// control端 and its agents.
//
// The connection is a sequence of JSON frames. Every frame carries the
// version of the protocol, a type, a sequence number and an optional
// payload. The control端 accepts only the registered message types, so an
// agent of a later stage cannot make the control端 of an earlier stage
// believe that it understands a message it does not know.
//
// Every kind of message has its own limit. A command carries a document of
// an application, an inventory carries a batch of containers, a receipt
// carries an outcome and a fragment of the output carries a piece of a log.
// The limit of the frame is the outer bound; the limit of the kind is the
// inner one, and it is checked after the frame was decoded.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// WireVersion is the version of the protocol the control端 speaks.
const WireVersion = 1

// The limits of the frames of the protocol.
const (
	// MaxMessageSize is the largest frame that travels over a connection.
	//
	// The limit was raised to eight mebibytes, because a command carries a
	// document of an application and the JSON of the frame escapes the
	// characters of the document. The limit of the frame bounds the bytes
	// on the wire; the limit of a kind bounds the payload after it was
	// decoded.
	MaxMessageSize = 8 * 1024 * 1024
	// MaxPayloadSize is the largest payload a message may carry when its
	// kind has no limit of its own.
	MaxPayloadSize = 1024 * 1024
	// MaxCommandPayloadSize is the largest payload of a deploy.command.
	//
	// The limit is measured on the decoded payload: the JSON of the frame
	// escapes the characters of the document, and a document within the
	// limit of the configuration may need more bytes on the wire than it
	// has. The limit of the frame still bounds the bytes on the wire.
	MaxCommandPayloadSize = 8 * 1024 * 1024
	// MaxInventoryPayloadSize is the largest payload of an inventory
	// batch. An inventory that does not fit into one payload is sent in
	// several batches.
	MaxInventoryPayloadSize = 1024 * 1024
	// MaxControlPayloadSize is the largest payload of a receipt, of a
	// query and of the answer of a query.
	MaxControlPayloadSize = 128 * 1024
	// MaxOutputPayloadSize is the largest payload of a fragment of the
	// output of a task.
	MaxOutputPayloadSize = 32 * 1024
)

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
	// ErrPayloadTooLarge reports a frame whose payload exceeds the limit
	// of its kind.
	ErrPayloadTooLarge = errors.New("protocol: the message payload is too large")
	// ErrPayloadInvalid reports a payload that does not hold the message
	// it claims to hold.
	ErrPayloadInvalid = errors.New("protocol: the message payload is invalid")
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
	TypeHello:             true,
	TypeHeartbeat:         true,
	TypeInventory:         true,
	TypeDeployCommand:     true,
	TypeDeployReceipt:     true,
	TypeDeployOutput:      true,
	TypeDeployQuery:       true,
	TypeDeployQueryResult: true,
}

// IsRegisteredType reports whether the control端 accepts the given message
// type.
func IsRegisteredType(messageType string) bool {
	return registeredTypes[messageType]
}

// RegisteredTypes returns the accepted message types.
func RegisteredTypes() []string {
	return []string{
		TypeHello,
		TypeHeartbeat,
		TypeInventory,
		TypeDeployCommand,
		TypeDeployReceipt,
		TypeDeployOutput,
		TypeDeployQuery,
		TypeDeployQueryResult,
	}
}

// PayloadLimit returns the largest payload the given message type may
// carry.
//
// The limit of a kind is smaller than the limit of a frame, so a frame that
// fits may still carry too much of its own kind.
func PayloadLimit(messageType string) int {
	switch messageType {
	case TypeDeployCommand:
		return MaxCommandPayloadSize
	case TypeInventory:
		return MaxInventoryPayloadSize
	case TypeDeployReceipt, TypeDeployQuery, TypeDeployQueryResult:
		return MaxControlPayloadSize
	case TypeDeployOutput:
		return MaxOutputPayloadSize
	default:
		return MaxPayloadSize
	}
}

// Validate checks a frame that arrives from an agent. A frame of an
// unknown version, of an unknown type or with an oversized payload is
// refused, so the connection is closed instead of acting on a message the
// control端 does not understand.
func (e Envelope) Validate() error {
	if e.Version != WireVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, e.Version)
	}
	if !IsRegisteredType(e.Type) {
		return fmt.Errorf("%w: %s", ErrUnsupportedType, e.Type)
	}
	limit := PayloadLimit(e.Type)
	if len(e.Payload) > limit {
		return fmt.Errorf("%w: %d bytes, the limit of a %s is %d bytes",
			ErrPayloadTooLarge, len(e.Payload), e.Type, limit)
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
