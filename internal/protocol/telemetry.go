package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The message types of the telemetry and the on demand logs.
const (
	// TypeMetricsSample carries one raw sample of a container.
	TypeMetricsSample = "metrics.sample"
	// TypeContainerEvent carries one event of a container of a node.
	TypeContainerEvent = "container.event"
	// TypeLogsSubscribe asks an agent to stream the logs of a container.
	TypeLogsSubscribe = "logs.subscribe"
	// TypeLogsUnsubscribe ends a subscription of the logs of a container.
	TypeLogsUnsubscribe = "logs.unsubscribe"
	// TypeLogsFrame carries a piece of the logs of a container.
	TypeLogsFrame = "logs.frame"
)

// The limits of the logs of a container.
//
// The number of the lines belongs to the subscription, the size of a frame
// bounds one message and the queue bounds the memory of a subscription of a
// node that reads slower than Docker writes.
const (
	// DefaultLogTail is the number of the lines a subscription reads when
	// it names no number.
	DefaultLogTail = 200
	// MaxLogTail is the largest number of the lines a subscription reads.
	MaxLogTail = 2000
	// MaxLogFrameTextSize is the largest text one frame carries.
	MaxLogFrameTextSize = 32 * 1024
	// MaxLogPayloadSize is the largest payload of a log frame. The JSON
	// escapes the control characters of the text, so the payload may be
	// several times the size of the text.
	MaxLogPayloadSize = 256 * 1024
)

// RawSample is the state of one container at one point in time, as Docker
// reports it.
//
// The counters are cumulative: a rate is the difference between two samples
// of the same container. The metrics package calculates the rates and marks
// a gap when the counters do not fit together.
type RawSample struct {
	// ContainerID is the container the sample belongs to.
	ContainerID string `json:"container_id"`
	// CollectedAt is the time the sample was read.
	CollectedAt time.Time `json:"collected_at"`
	// CPUUsageNS is the cumulative CPU time of the container in
	// nanoseconds.
	CPUUsageNS uint64 `json:"cpu_usage_ns"`
	// SystemUsageNS is the cumulative CPU time of the host in
	// nanoseconds.
	SystemUsageNS uint64 `json:"system_usage_ns"`
	// OnlineCPUs is the number of the CPUs of the host.
	OnlineCPUs uint32 `json:"online_cpus"`
	// MemoryUsage is the current memory usage in bytes.
	MemoryUsage uint64 `json:"memory_usage"`
	// MemoryLimit is the explicit limit of the container in bytes. Zero
	// means that the container has no limit; it never means a limit of
	// zero bytes.
	MemoryLimit uint64 `json:"memory_limit"`
	// NetRX is the cumulative number of the received bytes.
	NetRX uint64 `json:"net_rx"`
	// NetTX is the cumulative number of the transmitted bytes.
	NetTX uint64 `json:"net_tx"`
	// IORead is the cumulative number of the bytes read from the block
	// devices.
	IORead uint64 `json:"io_read"`
	// IOWrite is the cumulative number of the bytes written to the block
	// devices.
	IOWrite uint64 `json:"io_write"`
}

// ContainerEvent is one event of a container of a node.
//
// The source of an event is the authenticated connection of its node. The
// NodeID of the event names the Swarm node Docker reported, not the node
// that sent the event.
type ContainerEvent struct {
	// ContainerID is the container the event belongs to.
	ContainerID string `json:"container_id"`
	// ServiceID is the Swarm service of the container, when it has one.
	ServiceID string `json:"service_id,omitempty"`
	// Slot is the slot of the Swarm task of the container, when it has
	// one.
	Slot string `json:"slot,omitempty"`
	// NodeID is the Swarm node Docker reported for the container.
	NodeID string `json:"node_id,omitempty"`
	// Action is the action of the event, for example die, start or stop.
	Action string `json:"action"`
	// ExitCode is the exit code of a container that ended.
	ExitCode *int `json:"exit_code,omitempty"`
	// OccurredAt is the time Docker reported for the event.
	OccurredAt time.Time `json:"occurred_at"`
}

// LogRequest asks an agent to stream the logs of a container.
type LogRequest struct {
	// SubscriptionID identifies the subscription.
	SubscriptionID string `json:"subscription_id"`
	// ContainerID is the container whose logs are read. An unsubscribe
	// leaves it empty.
	ContainerID string `json:"container_id,omitempty"`
	// Tail is the number of the lines the subscription starts with.
	Tail int `json:"tail"`
	// Follow reports whether the subscription follows the stream.
	Follow bool `json:"follow"`
}

// LogFrame is a piece of the logs of a container.
type LogFrame struct {
	// SubscriptionID identifies the subscription the frame belongs to.
	SubscriptionID string `json:"subscription_id"`
	// Offset is the position of the text in the output of the
	// subscription.
	Offset uint64 `json:"offset"`
	// Text is the piece of the output.
	Text string `json:"text"`
	// Truncated reports that a piece before this frame was dropped.
	Truncated bool `json:"truncated,omitempty"`
	// ErrorCode names the reason the stream ended without its logs.
	ErrorCode string `json:"error_code,omitempty"`
}

// NormalizeLogTail returns the number of the lines a subscription reads.
// A missing or negative number becomes the default, a larger number is
// capped, so the agent never reads more than the limit of the platform.
func NormalizeLogTail(tail int) int {
	if tail <= 0 {
		return DefaultLogTail
	}
	if tail > MaxLogTail {
		return MaxLogTail
	}
	return tail
}

// DecodeLogRequest reads the request of a subscription of the logs.
func DecodeLogRequest(payload []byte) (LogRequest, error) {
	var request LogRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return LogRequest{}, fmt.Errorf("%w: decode the log request: %v", ErrPayloadInvalid, err)
	}
	if strings.TrimSpace(request.SubscriptionID) == "" {
		return LogRequest{}, fmt.Errorf("%w: the log request names no subscription", ErrPayloadInvalid)
	}
	return request, nil
}

// DecodeLogFrame reads a frame of the logs.
func DecodeLogFrame(payload []byte) (LogFrame, error) {
	var frame LogFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return LogFrame{}, fmt.Errorf("%w: decode the log frame: %v", ErrPayloadInvalid, err)
	}
	if strings.TrimSpace(frame.SubscriptionID) == "" {
		return LogFrame{}, fmt.Errorf("%w: the log frame names no subscription", ErrPayloadInvalid)
	}
	if len(frame.Text) > MaxLogFrameTextSize {
		return LogFrame{}, fmt.Errorf("%w: the log frame holds %d bytes of text, the limit is %d bytes",
			ErrPayloadInvalid, len(frame.Text), MaxLogFrameTextSize)
	}
	return frame, nil
}
