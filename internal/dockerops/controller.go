package dockerops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"

	"hmoob-dock/internal/protocol"
)

// The grace periods and the timeouts of a container operation.
const (
	// containerStopGraceSeconds is the number of the seconds Docker
	// waits for a container to end by itself before it kills it. It
	// matches the default of the engine, written out so the platform
	// does not depend on it silently.
	containerStopGraceSeconds = 10
	// operationTimeout bounds one operation of the Docker API. A stop
	// with its grace period fits into the timeout with room to spare.
	operationTimeout = 25 * time.Second
)

// ContainerController operates on single containers of a node.
//
// The operations are the fixed verbs of the platform: they take the ID of
// a container and nothing else. They never carry a shell, a path or a flag
// of their own, and they never remove the volumes of a container.
type ContainerController interface {
	// StartContainer starts a stopped container.
	StartContainer(ctx context.Context, containerID string) error
	// StopContainer stops a container with the grace period of the
	// platform.
	StopContainer(ctx context.Context, containerID string) error
	// RestartContainer restarts a container.
	RestartContainer(ctx context.Context, containerID string) error
	// RemoveContainer removes a stopped container. The volumes and the
	// images of the container are kept.
	RemoveContainer(ctx context.Context, containerID string) error
	// InspectContainer reads the state of a container after an
	// operation.
	InspectContainer(ctx context.Context, containerID string) (*protocol.Container, error)
}

// compile time proof that the local engine implements the controller.
var _ ContainerController = (*Local)(nil)

// StartContainer starts a stopped container.
func (l *Local) StartContainer(ctx context.Context, containerID string) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := l.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return operationError("start", containerID, err)
	}
	return nil
}

// StopContainer stops a container with the grace period of the platform.
func (l *Local) StopContainer(ctx context.Context, containerID string) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := l.client.ContainerStop(ctx, containerID, container.StopOptions{Timeout: intPtr(containerStopGraceSeconds)}); err != nil {
		return operationError("stop", containerID, err)
	}
	return nil
}

// RestartContainer restarts a container.
func (l *Local) RestartContainer(ctx context.Context, containerID string) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := l.client.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: intPtr(containerStopGraceSeconds)}); err != nil {
		return operationError("restart", containerID, err)
	}
	return nil
}

// RemoveContainer removes a container.
//
// The volumes and the images of the container are kept: the removal of a
// container is not the cleanup of a project, and the data of a site never
// belongs to the platform.
func (l *Local) RemoveContainer(ctx context.Context, containerID string) error {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := l.client.ContainerRemove(ctx, containerID, container.RemoveOptions{}); err != nil {
		return operationError("remove", containerID, err)
	}
	return nil
}

// InspectContainer reads the state of a container and projects it into the
// shape of the inventory, so a result carries the state the page shows.
func (l *Local) InspectContainer(ctx context.Context, containerID string) (*protocol.Container, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	details, err := l.client.ContainerInspect(ctx, containerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ErrContainerNotFound
		}
		return nil, fmt.Errorf("dockerops: inspect the container %s: %w", containerID, err)
	}
	summary := container.Summary{
		ID:    details.ID,
		Names: []string{details.Name},
		Image: details.Config.Image,
		State: details.State.Status,
	}
	protocolContainer := containerOf(summary, details)
	return &protocolContainer, nil
}

// intPtr returns a pointer to the given number of the seconds, so a call of
// the SDK carries the grace period explicitly.
func intPtr(seconds int) *int {
	return &seconds
}

// operationError translates the errors of the Docker API into the errors
// of the platform: a container that is gone is reported as gone, and every
// other refusal keeps its reason.
func operationError(action, containerID string, err error) error {
	if errdefs.IsNotFound(err) {
		return ErrContainerNotFound
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("dockerops: %s of the container %s reached the timeout: %w", action, containerID, err)
	}
	return fmt.Errorf("dockerops: %s of the container %s: %w", action, containerID, err)
}
