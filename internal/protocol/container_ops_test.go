package protocol

import (
	"errors"
	"strings"
	"testing"
)

// TestTheContainerOperationTypesAreRegistered proves that the control端
// accepts the messages of a container operation.
func TestTheContainerOperationTypesAreRegistered(t *testing.T) {
	registered := RegisteredTypes()
	found := map[string]bool{}
	for _, messageType := range registered {
		found[messageType] = true
	}
	for _, messageType := range []string{TypeContainerCommand, TypeContainerResult} {
		if !found[messageType] {
			t.Fatalf("the message type %s is not registered", messageType)
		}
		if !IsRegisteredType(messageType) {
			t.Fatalf("the message type %s is not accepted", messageType)
		}
	}
}

// TestTheLimitsOfTheContainerMessages proves that the messages of a
// container operation carry the limit of a control frame.
func TestTheLimitsOfTheContainerMessages(t *testing.T) {
	for _, messageType := range []string{TypeContainerCommand, TypeContainerResult} {
		if limit := PayloadLimit(messageType); limit != MaxControlPayloadSize {
			t.Fatalf("the limit of a %s = %d bytes, want %d", messageType, limit, MaxControlPayloadSize)
		}
	}
}

// TestDecodeContainerCommandRejectsBrokenCommands proves that a command
// without a request, with an unknown action or without a container is
// refused, so the agent never guesses what it was asked to do.
func TestDecodeContainerCommandRejectsBrokenCommands(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"no request", `{"action":"stop","container_id":"abcdef123456"}`},
		{"unknown action", `{"request_id":"r1","action":"exec","container_id":"abcdef123456"}`},
		{"empty action", `{"request_id":"r1","action":"","container_id":"abcdef123456"}`},
		{"no container", `{"request_id":"r1","action":"stop"}`},
		{"short container", `{"request_id":"r1","action":"stop","container_id":"abc"}`},
		{"non hexadecimal container", `{"request_id":"r1","action":"stop","container_id":"zzzzzz123456"}`},
		{"broken json", `{`},
	}
	for _, test := range cases {
		if _, err := DecodeContainerCommand([]byte(test.payload)); !errors.Is(err, ErrPayloadInvalid) {
			t.Fatalf("%s: the command was not refused: %v", test.name, err)
		}
	}
}

// TestDecodeContainerCommandAcceptsAWellFormedCommand proves that a command
// of every known action is decoded and that a short and a long container ID
// are both valid.
func TestDecodeContainerCommandAcceptsAWellFormedCommand(t *testing.T) {
	longID := strings.Repeat("a", 64)
	cases := []string{"abcdef123456", longID}
	for _, action := range ContainerActions() {
		for _, containerID := range cases {
			command, err := DecodeContainerCommand([]byte(
				`{"request_id":"r1","action":"` + action + `","container_id":"` + containerID + `"}`))
			if err != nil {
				t.Fatalf("the %s command of %s was refused: %v", action, containerID, err)
			}
			if command.RequestID != "r1" || command.Action != action || command.ContainerID != containerID {
				t.Fatalf("the command was decoded wrong: %+v", command)
			}
		}
	}
}

// TestDecodeContainerResultRequiresARequest proves that a result without
// the ID of its request is refused.
func TestDecodeContainerResultRequiresARequest(t *testing.T) {
	if _, err := DecodeContainerResult([]byte(`{"ok":true}`)); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("the result without a request was not refused: %v", err)
	}
	result, err := DecodeContainerResult([]byte(`{"request_id":"r1","ok":false,"error_code":"container_not_found","message":"gone"}`))
	if err != nil {
		t.Fatalf("the result was refused: %v", err)
	}
	if result.RequestID != "r1" || result.OK || result.ErrorCode != ResultCodeNotFound {
		t.Fatalf("the result was decoded wrong: %+v", result)
	}
}
