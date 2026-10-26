package protocol

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hmoob-dock/internal/model"
)

// TestTheFrameLimitIsEightMebibytes proves that the limit of a frame was
// raised to eight mebibytes, so a command with a document fits into one
// frame.
func TestTheFrameLimitIsEightMebibytes(t *testing.T) {
	if MaxMessageSize != 8*1024*1024 {
		t.Fatalf("the limit of a frame = %d bytes, want %d", MaxMessageSize, 8*1024*1024)
	}
}

// TestTheLimitsOfTheMessageTypes proves that every kind of message has its
// own limit and that a payload beyond it is refused.
func TestTheLimitsOfTheMessageTypes(t *testing.T) {
	cases := []struct {
		messageType string
		limit       int
	}{
		{TypeDeployCommand, MaxCommandPayloadSize},
		{TypeInventory, MaxInventoryPayloadSize},
		{TypeDeployReceipt, MaxControlPayloadSize},
		{TypeDeployQuery, MaxControlPayloadSize},
		{TypeDeployQueryResult, MaxControlPayloadSize},
		{TypeDeployOutput, MaxOutputPayloadSize},
	}

	for _, test := range cases {
		envelope := Envelope{
			Version: WireVersion,
			Type:    test.messageType,
			Payload: make([]byte, test.limit+1),
		}
		if err := envelope.Validate(); !errors.Is(err, ErrPayloadTooLarge) {
			t.Fatalf("the payload of a %s of %d bytes was not refused: %v",
				test.messageType, test.limit+1, err)
		}

		envelope.Payload = make([]byte, test.limit)
		if err := envelope.Validate(); err != nil {
			t.Fatalf("the payload of a %s of %d bytes was refused: %v",
				test.messageType, test.limit, err)
		}
	}
}

// TestTheDeploymentTypesAreRegistered proves that the control端 accepts the
// messages of a deployment.
func TestTheDeploymentTypesAreRegistered(t *testing.T) {
	types := []string{
		TypeDeployCommand,
		TypeDeployReceipt,
		TypeDeployOutput,
		TypeDeployQuery,
		TypeDeployQueryResult,
	}
	for _, messageType := range types {
		if !IsRegisteredType(messageType) {
			t.Fatalf("the type %s is not registered", messageType)
		}
	}
}

// TestTheBusinessLimitsHoldAfterTheDecode proves that a frame that fits into
// the frame limit may still carry a configuration that is too large.
func TestTheBusinessLimitsHoldAfterTheDecode(t *testing.T) {
	command := model.Command{
		Task: model.Task{ID: "task-1"},
		Config: model.Config{
			YAML: strings.Repeat("y", MaxCommandYAMLSize+1),
		},
	}
	payload, err := json.Marshal(DeployCommand{Command: command})
	if err != nil {
		t.Fatalf("encode the command: %v", err)
	}
	if _, err := DecodeDeployCommand(payload); err == nil {
		t.Fatal("the decode accepted a document that is too large")
	}

	command.Config.YAML = strings.Repeat("y", MaxCommandYAMLSize)
	command.Config.Variables = map[string]string{
		"PASSWORD": strings.Repeat("p", MaxCommandVariablesSize+1),
	}
	payload, err = json.Marshal(DeployCommand{Command: command})
	if err != nil {
		t.Fatalf("encode the command: %v", err)
	}
	if _, err := DecodeDeployCommand(payload); err == nil {
		t.Fatal("the decode accepted variables that are too large")
	}
}

// TestTheDecodeOfACommandKeepsTheCommand proves that a command survives the
// way around.
func TestTheDecodeOfACommandKeepsTheCommand(t *testing.T) {
	command := model.Command{
		Task:   model.Task{ID: "task-1", AppID: "app-1", VersionID: "v1", Digest: "digest-1"},
		App:    model.App{ID: "app-1", Name: "demo", Mode: "compose", TargetID: "node-1", Namespace: "demo"},
		Config: model.Config{YAML: "services:\n  api:\n    image: nginx\n", Variables: map[string]string{"TAG": "1"}},
	}
	payload, err := json.Marshal(DeployCommand{Command: command})
	if err != nil {
		t.Fatalf("encode the command: %v", err)
	}

	decoded, err := DecodeDeployCommand(payload)
	if err != nil {
		t.Fatalf("decode the command: %v", err)
	}
	if decoded.Task.ID != "task-1" || decoded.Task.Digest != "digest-1" {
		t.Fatalf("the decoded task = %+v, want the encoded one", decoded.Task)
	}
	if decoded.Config.YAML != command.Config.YAML {
		t.Fatalf("the decoded document = %q, want the encoded one", decoded.Config.YAML)
	}
	if decoded.Config.Variables["TAG"] != "1" {
		t.Fatalf("the decoded variables = %+v, want the encoded ones", decoded.Config.Variables)
	}
}

// TestTheDecodeOfAQueryRefusesAnEmptyTask proves that a query names a task.
func TestTheDecodeOfAQueryRefusesAnEmptyTask(t *testing.T) {
	payload, err := json.Marshal(DeployQuery{})
	if err != nil {
		t.Fatalf("encode the query: %v", err)
	}
	if _, err := DecodeDeployQuery(payload); err == nil {
		t.Fatal("the decode accepted a query that names no task")
	}
}
