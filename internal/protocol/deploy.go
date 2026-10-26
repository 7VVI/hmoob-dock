package protocol

import (
	"encoding/json"
	"fmt"
	"strings"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/yamlspec"
)

// The message types of a deployment.
const (
	// TypeDeployCommand carries the instruction of the control端 to
	// deploy an application.
	TypeDeployCommand = "deploy.command"
	// TypeDeployReceipt carries the report of an agent about a task.
	TypeDeployReceipt = "deploy.receipt"
	// TypeDeployOutput carries a fragment of the output of a task.
	TypeDeployOutput = "deploy.output"
	// TypeDeployQuery asks an agent about a task in its ledger. The query
	// changes nothing.
	TypeDeployQuery = "deploy.query"
	// TypeDeployQueryResult answers a query about the ledger of an agent.
	TypeDeployQueryResult = "deploy.query_result"
	// TypeDeployResolve asks an agent to end a task whose outcome could
	// not be proven.
	//
	// The resolution is a frame of its own and not a command: a command
	// deploys an application, while a resolution ends a task that nobody
	// may prove to be finished. A frame of its own keeps the two apart, so
	// a resolution never reaches the executor of a deployment.
	TypeDeployResolve = "deploy.resolve"
)

// The limits of the configuration of a command.
//
// The limits are the limits of the configuration itself. A frame that fits
// into the limit of its kind may still carry a configuration that is too
// large, and the limit of the frame is not a way around the limit of the
// configuration.
const (
	// MaxCommandYAMLSize is the largest document a command may carry.
	MaxCommandYAMLSize = yamlspec.MaxDocumentSize
	// MaxCommandVariablesSize is the largest total size of the variables
	// a command may carry.
	MaxCommandVariablesSize = yamlspec.MaxVariablesSize
)

// DeployCommand is the instruction of the control端 to deploy an
// application.
type DeployCommand struct {
	// Command is the command the agent executes.
	Command model.Command `json:"command"`
}

// DeployReceipt is the report of an agent about a task.
//
// The receipt carries the sequence number of the task, so a receipt that
// arrives again is recognised as the same report and not as a new one.
type DeployReceipt struct {
	// Receipt is the receipt of the task.
	Receipt model.Receipt `json:"receipt"`
}

// DeployOutput is a fragment of the output of a task.
//
// The fragment names its offset, so a reader skips the fragments it has
// already seen, and it marks the truncation, so a reader learns that the
// output of the task is incomplete.
type DeployOutput struct {
	// TaskID is the task the fragment belongs to.
	TaskID string `json:"task_id"`
	// Offset is the position of the fragment in the output of the task.
	Offset int64 `json:"offset"`
	// Data is the fragment itself.
	Data []byte `json:"data"`
	// Truncated reports that the output of the task was truncated.
	Truncated bool `json:"truncated"`
}

// DeployQuery asks the ledger of an agent about a task.
//
// The query only reads the ledger. It never asks the agent to execute
// anything, so a control端 that lost the state of a task learns what
// happened without touching the target again.
type DeployQuery struct {
	// TaskID is the task the query asks about.
	TaskID string `json:"task_id"`
}

// DeployQueryResult is the answer of an agent about a task in its ledger.
type DeployQueryResult struct {
	// TaskID is the task the answer belongs to.
	TaskID string `json:"task_id"`
	// Found reports whether the ledger of the agent knows the task.
	Found bool `json:"found"`
	// Receipt is the last receipt of the task. It is empty when the
	// ledger does not know the task.
	Receipt model.Receipt `json:"receipt"`
}

// DeployResolve asks the ledger of an agent to end a task by hand.
//
// The resolution carries the sequence number the task holds, so a stale
// resolution cannot cover a newer state of the task, and it carries the
// reason of the administrator, so the node keeps the reason of the outcome
// it writes.
type DeployResolve struct {
	// TaskID is the task the resolution ends.
	TaskID string `json:"task_id"`
	// Digest identifies the configuration of the task.
	Digest string `json:"digest"`
	// ExpectedSeq is the sequence number the task holds.
	ExpectedSeq uint64 `json:"expected_seq"`
	// Reason is the reason of the administrator.
	Reason string `json:"reason"`
}

// DecodeDeployCommand reads the command of a frame.
//
// The identity of the command is checked before its limits: a command that
// names no task, no application or no configuration cannot be kept in the
// ledger of an agent, and a command that names another application silently
// would break the exclusion of the applications.
//
// The limits of the configuration hold after the frame was decoded: a frame
// that fits into the limit of its kind may still carry a document or
// variables that are too large.
func DecodeDeployCommand(payload []byte) (model.Command, error) {
	var message DeployCommand
	if err := json.Unmarshal(payload, &message); err != nil {
		return model.Command{}, fmt.Errorf("%w: decode the command: %v", ErrPayloadInvalid, err)
	}
	if err := checkCommandIdentity(message.Command); err != nil {
		return model.Command{}, err
	}
	if err := checkCommandLimits(message.Command); err != nil {
		return model.Command{}, err
	}
	return message.Command, nil
}

// DecodeDeployQuery reads the query of a frame.
func DecodeDeployQuery(payload []byte) (DeployQuery, error) {
	var query DeployQuery
	if err := json.Unmarshal(payload, &query); err != nil {
		return DeployQuery{}, fmt.Errorf("%w: decode the query: %v", ErrPayloadInvalid, err)
	}
	if strings.TrimSpace(query.TaskID) == "" {
		return DeployQuery{}, fmt.Errorf("%w: the query names no task", ErrPayloadInvalid)
	}
	return query, nil
}

// DecodeDeployResolve reads the resolution of a frame.
//
// The identity of the resolution is checked before it is used: a resolution
// that names no task or no configuration cannot be compared with the task of
// the ledger, and a resolution without a reason would end a task without
// telling why.
func DecodeDeployResolve(payload []byte) (DeployResolve, error) {
	var message DeployResolve
	if err := json.Unmarshal(payload, &message); err != nil {
		return DeployResolve{}, fmt.Errorf("%w: decode the resolution: %v", ErrPayloadInvalid, err)
	}
	if strings.TrimSpace(message.TaskID) == "" {
		return DeployResolve{}, fmt.Errorf("%w: the resolution names no task", ErrPayloadInvalid)
	}
	if strings.TrimSpace(message.Digest) == "" {
		return DeployResolve{}, fmt.Errorf("%w: the resolution names no configuration", ErrPayloadInvalid)
	}
	if strings.TrimSpace(message.Reason) == "" {
		return DeployResolve{}, fmt.Errorf("%w: the resolution names no reason", ErrPayloadInvalid)
	}
	return message, nil
}

// checkCommandIdentity refuses a command that does not name its task, its
// application and its configuration.
//
// The identity of a command is what the ledger of the agent compares, so a
// command without it cannot be kept there and cannot be refused there
// either. A command that names another application of a known task would be
// executed for the wrong application.
func checkCommandIdentity(command model.Command) error {
	if strings.TrimSpace(command.Task.ID) == "" {
		return fmt.Errorf("%w: the command names no task", ErrPayloadInvalid)
	}
	if strings.TrimSpace(command.Task.AppID) == "" {
		return fmt.Errorf("%w: the command names no application", ErrPayloadInvalid)
	}
	if strings.TrimSpace(command.Task.Digest) == "" {
		return fmt.Errorf("%w: the command names no configuration", ErrPayloadInvalid)
	}
	return nil
}

// checkCommandLimits refuses a command whose configuration is too large.
func checkCommandLimits(command model.Command) error {
	if len(command.Config.YAML) > MaxCommandYAMLSize {
		return fmt.Errorf("%w: the document of the command holds %d bytes, the limit is %d bytes",
			ErrPayloadInvalid, len(command.Config.YAML), MaxCommandYAMLSize)
	}
	size := 0
	for name, value := range command.Config.Variables {
		size += len(name) + len(value)
	}
	if size > MaxCommandVariablesSize {
		return fmt.Errorf("%w: the variables of the command hold %d bytes, the limit is %d bytes",
			ErrPayloadInvalid, size, MaxCommandVariablesSize)
	}
	return nil
}
