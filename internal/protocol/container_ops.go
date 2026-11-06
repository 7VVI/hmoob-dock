package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The message types of the container operations.
//
// The control端 asks an agent to operate on one container of its node and
// the agent answers with one result. The RequestID of the envelope ties the
// result to the command; one command is answered by exactly one frame.
const (
	// TypeContainerCommand asks an agent to operate on one container.
	TypeContainerCommand = "container.command"
	// TypeContainerResult carries the outcome of one container command.
	TypeContainerResult = "container.result"
)

// The actions of a container command. The list is closed: an agent refuses
// an action it does not know instead of guessing what it might mean.
const (
	// ContainerActionStart starts a stopped container.
	ContainerActionStart = "start"
	// ContainerActionStop stops a running container with a grace period.
	ContainerActionStop = "stop"
	// ContainerActionRestart restarts a container.
	ContainerActionRestart = "restart"
	// ContainerActionRemove removes a container. The platform removes
	// neither the volumes of the container nor its images.
	ContainerActionRemove = "remove"
)

// ContainerActions returns the actions a container command may carry.
func ContainerActions() []string {
	return []string{ContainerActionStart, ContainerActionStop, ContainerActionRestart, ContainerActionRemove}
}

// IsContainerAction reports whether the given action is known.
func IsContainerAction(action string) bool {
	switch action {
	case ContainerActionStart, ContainerActionStop, ContainerActionRestart, ContainerActionRemove:
		return true
	}
	return false
}

// ContainerCommand asks an agent to operate on one container of its node.
//
// The source of the command is the authenticated connection of the
// control端. The command carries no shell, no flags and no path of its
// own: the action is one of the fixed words above and everything else is
// decided by the agent.
type ContainerCommand struct {
	// RequestID ties the command to its result. The control端 generates
	// it and waits for the result that carries the same ID.
	RequestID string `json:"request_id"`
	// Action is one of the fixed actions of the protocol.
	Action string `json:"action"`
	// ContainerID is the container the action applies to.
	ContainerID string `json:"container_id"`
}

// ContainerResult reports the outcome of one container command.
type ContainerResult struct {
	// RequestID is the ID of the command the result answers.
	RequestID string `json:"request_id"`
	// OK reports whether the operation succeeded.
	OK bool `json:"ok"`
	// ErrorCode names the reason of a failed operation, for example
	// container_not_found. It is empty on success.
	ErrorCode string `json:"error_code,omitempty"`
	// Message is a short, human readable reason of a failed operation.
	Message string `json:"message,omitempty"`
	// Container is the state of the container after the operation, as
	// far as the agent could read it. It is nil when the container is
	// gone or the state could not be read.
	Container *Container `json:"container,omitempty"`
}

// The error codes of a container result.
const (
	// ResultCodeNotFound reports a container the agent cannot find.
	ResultCodeNotFound = "container_not_found"
	// ResultCodeFailed reports an operation Docker refused or that
	// failed for another reason.
	ResultCodeFailed = "operation_failed"
	// ResultCodeUnsupported reports an action the agent does not know.
	ResultCodeUnsupported = "action_unsupported"
)

// DecodeContainerCommand reads the command of a container operation.
func DecodeContainerCommand(payload []byte) (ContainerCommand, error) {
	var command ContainerCommand
	if err := json.Unmarshal(payload, &command); err != nil {
		return ContainerCommand{}, fmt.Errorf("%w: decode the container command: %v", ErrPayloadInvalid, err)
	}
	if strings.TrimSpace(command.RequestID) == "" {
		return ContainerCommand{}, fmt.Errorf("%w: the container command names no request", ErrPayloadInvalid)
	}
	if !IsContainerAction(command.Action) {
		return ContainerCommand{}, fmt.Errorf("%w: the container command names an unknown action %q", ErrPayloadInvalid, command.Action)
	}
	if !ValidContainerID(command.ContainerID) {
		return ContainerCommand{}, fmt.Errorf("%w: the container command names no valid container", ErrPayloadInvalid)
	}
	return command, nil
}

// DecodeContainerResult reads the result of a container operation.
func DecodeContainerResult(payload []byte) (ContainerResult, error) {
	var result ContainerResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return ContainerResult{}, fmt.Errorf("%w: decode the container result: %v", ErrPayloadInvalid, err)
	}
	if strings.TrimSpace(result.RequestID) == "" {
		return ContainerResult{}, fmt.Errorf("%w: the container result names no request", ErrPayloadInvalid)
	}
	return result, nil
}

// ValidContainerID reports whether the given string can be a Docker
// container ID: hexadecimal, between the short and the long form.
func ValidContainerID(containerID string) bool {
	if len(containerID) < 12 || len(containerID) > 64 {
		return false
	}
	for _, ch := range containerID {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
			return false
		}
	}
	return true
}
