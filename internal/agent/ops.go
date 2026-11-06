package agent

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/protocol"
)

// The bounds of the container operations of a node.
const (
	// maxConcurrentOps is the largest number of the container
	// operations a node runs at the same time. A single administrator
	// rarely needs more, and the bound keeps a flood of commands from
	// exhausting the engine.
	maxConcurrentOps = 2
	// operationContextTimeout bounds one operation. It is longer than
	// the timeout of the Docker API itself, so the engine reports its
	// own reason instead of the session ending first.
	operationContextTimeout = 30 * time.Second
)

// errOperationUnsupported reports an action the agent does not know. The
// protocol refuses such a command already; the check repeats here, so a
// caller of the manager cannot bypass the whitelist.
var errOperationUnsupported = errors.New("agent: the action of the container command is not supported")

// opsManager serves the container operation frames of the control端.
//
// An operation runs in its own goroutine: a stop may take the grace period
// of the engine, and the read loop of the connection must not wait for it,
// because the frames of a deployment arrive on the same connection. The
// manager bounds the operations that run at the same time.
type opsManager struct {
	// controller operates on the containers of the engine.
	controller dockerops.ContainerController
	// ctx is the context of the session: an operation that still runs
	// when the connection ends is canceled with it.
	ctx context.Context
	// slots bounds the operations that run at the same time.
	slots chan struct{}
	// refreshInventory asks the report of the inventory to leave at once.
	// It is set by the session that owns the manager; it may be nil in a
	// manager that a test built by hand.
	refreshInventory func()

	// mu guards the draining of the waiters below.
	mu sync.Mutex
	// closed reports that the manager no longer starts operations.
	closed bool
	// running counts the operations that wait for their execution.
	running sync.WaitGroup
}

// newOpsManager builds the manager of the container operations of a
// session. A nil controller means the engine of the node cannot operate on
// its containers, and every command is refused.
func newOpsManager(ctx context.Context, controller dockerops.ContainerController) *opsManager {
	return &opsManager{
		controller: controller,
		ctx:        ctx,
		slots:      make(chan struct{}, maxConcurrentOps),
	}
}

// handle serves a frame of the control端.
//
// The command is validated before it leaves the read loop, so a broken
// command is answered at once. A well formed command runs in its own
// goroutine and answers with exactly one result frame.
func (m *opsManager) handle(envelope protocol.Envelope, out sender) {
	if m == nil || m.controller == nil {
		return
	}
	command, err := protocol.DecodeContainerCommand(envelope.Payload)
	if err != nil {
		log.Printf("agent: the container command of the control端 is not valid: %v", err)
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = out.sendControl(protocol.TypeContainerResult, protocol.ContainerResult{
			RequestID: command.RequestID,
			ErrorCode: protocol.ResultCodeFailed,
			Message:   "节点连接正在关闭，操作没有执行",
		})
		return
	}
	m.running.Add(1)
	m.mu.Unlock()

	go m.run(command, out)
}

// run executes one operation and answers with its result.
//
// A slot is taken before the operation starts, so the operations of a node
// queue politely instead of competing for the engine.
func (m *opsManager) run(command protocol.ContainerCommand, out sender) {
	defer m.running.Done()

	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-m.ctx.Done():
		m.answer(command, protocol.ContainerResult{
			RequestID: command.RequestID,
			ErrorCode: protocol.ResultCodeFailed,
			Message:   "节点连接已结束，操作没有执行",
		}, out)
		return
	}

	result := m.execute(command)
	m.answer(command, result, out)
	// The state of the engine changed, so the report of the inventory
	// leaves at once and the pages see the result within moments.
	if m.refreshInventory != nil {
		m.refreshInventory()
	}
}

// execute runs the action of the command on the engine.
func (m *opsManager) execute(command protocol.ContainerCommand) protocol.ContainerResult {
	ctx, cancel := context.WithTimeout(m.ctx, operationContextTimeout)
	defer cancel()

	var err error
	switch command.Action {
	case protocol.ContainerActionStart:
		err = m.controller.StartContainer(ctx, command.ContainerID)
	case protocol.ContainerActionStop:
		err = m.controller.StopContainer(ctx, command.ContainerID)
	case protocol.ContainerActionRestart:
		err = m.controller.RestartContainer(ctx, command.ContainerID)
	case protocol.ContainerActionRemove:
		err = m.controller.RemoveContainer(ctx, command.ContainerID)
		if err == nil {
			// The container is gone; there is no state to report.
			return protocol.ContainerResult{RequestID: command.RequestID, OK: true}
		}
	default:
		return protocol.ContainerResult{
			RequestID: command.RequestID,
			ErrorCode: protocol.ResultCodeUnsupported,
			Message:   errOperationUnsupported.Error(),
		}
	}

	if err != nil {
		return failureOf(command.RequestID, err)
	}

	// The state of the container after the operation belongs to the
	// answer, so the page shows the result and not a guess. A state that
	// cannot be read does not fail the operation.
	state, stateErr := m.controller.InspectContainer(ctx, command.ContainerID)
	if stateErr != nil {
		return protocol.ContainerResult{RequestID: command.RequestID, OK: true}
	}
	return protocol.ContainerResult{RequestID: command.RequestID, OK: true, Container: state}
}

// answer hands the result of an operation to the control端.
func (m *opsManager) answer(command protocol.ContainerCommand, result protocol.ContainerResult, out sender) {
	if err := out.sendControl(protocol.TypeContainerResult, result); err != nil {
		log.Printf("agent: report the result of the container %s: %v", command.ContainerID, err)
	}
}

// failureOf turns the error of an operation into the result the control端
// shows: a container that is gone is reported as gone, everything else
// keeps the reason of the refusal.
func failureOf(requestID string, err error) protocol.ContainerResult {
	result := protocol.ContainerResult{RequestID: requestID}
	switch {
	case errors.Is(err, dockerops.ErrContainerNotFound):
		result.ErrorCode = protocol.ResultCodeNotFound
		result.Message = "容器不存在或已被删除"
	default:
		result.ErrorCode = protocol.ResultCodeFailed
		result.Message = err.Error()
	}
	return result
}

// Close stops the manager from starting new operations and waits for the
// operations that still run, so no frame is written to a closed
// connection.
func (m *opsManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.running.Wait()
}
