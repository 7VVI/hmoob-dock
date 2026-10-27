package dockerops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"hmoob-dock/internal/protocol"
)

// Sample reads one sample of a container.
//
// The sample is a single request to the engine and not a stream: the agent
// reads the counters every interval and derives the rates itself, so no
// long lived connection of the engine is held between two samples.
//
// The limit of the memory comes from the explicit configuration of the
// container. The engine reports the memory of the host as the limit of a
// container without one, so the field of the stats is not used: an
// unlimited container keeps the limit zero and is never shown as one with a
// limit of zero bytes.
func (l *Local) Sample(ctx context.Context, containerID string) (protocol.RawSample, error) {
	id := strings.TrimSpace(containerID)
	if id == "" {
		return protocol.RawSample{}, fmt.Errorf("dockerops: sample a container without an ID")
	}

	reader, err := l.client.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return protocol.RawSample{}, fmt.Errorf("dockerops: read the stats of the container %s: %w", id, err)
	}
	var stats container.StatsResponse
	decodeErr := json.NewDecoder(reader.Body).Decode(&stats)
	closeErr := reader.Body.Close()
	if decodeErr != nil {
		return protocol.RawSample{}, fmt.Errorf("dockerops: decode the stats of the container %s: %w", id, decodeErr)
	}
	if closeErr != nil {
		return protocol.RawSample{}, fmt.Errorf("dockerops: close the stats of the container %s: %w", id, closeErr)
	}

	// The sample belongs to the moment the counters were read.
	collectedAt := time.Now().UTC()

	details, err := l.client.ContainerInspect(ctx, id)
	if err != nil {
		return protocol.RawSample{}, fmt.Errorf("dockerops: inspect the container %s: %w", id, err)
	}
	var limit uint64
	if details.HostConfig != nil && details.HostConfig.Memory > 0 {
		limit = uint64(details.HostConfig.Memory)
	}

	return sampleOf(id, stats, limit, collectedAt), nil
}

// Events streams the events of the containers of the local engine.
//
// The events are read from the engine itself and not from the inventory of
// the agent: a container that starts and ends between two inventories is
// still an event. A stream that ends is reported, so the agent opens a new
// one; the events of the gap are not invented. The inventory of the node
// stays the source of the current state after such a gap.
func (l *Local) Events(ctx context.Context, emit func(protocol.ContainerEvent) error) error {
	if emit == nil {
		return errors.New("dockerops: read the events without a reader")
	}

	options := events.ListOptions{
		Filters: filters.NewArgs(filters.Arg("type", "container")),
	}
	// The stream of the engine ends with this call, also when the reader of
	// the events returns an error. A stream that would outlive the call
	// would hold the response of the engine open.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	messages, failures := l.client.Events(streamCtx, options)
	for {
		select {
		case <-ctx.Done():
			// The reader ended the subscription. That is not a failure of
			// the engine.
			return nil
		case message, open := <-messages:
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("dockerops: the event stream of the engine ended")
			}
			event := containerEventOf(message)
			if event.ContainerID == "" {
				// The event belongs to no container. The filter of the
				// engine should keep it away already.
				continue
			}
			if err := emit(event); err != nil {
				return err
			}
		case err, open := <-failures:
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("dockerops: the event stream of the engine ended")
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("dockerops: read the events of the engine: %w", err)
			}
		}
	}
}

// containerEventOf projects an event of the Docker SDK into an event of the
// protocol.
//
// The exit code is only part of the attributes of an event of a container
// that ended. The slot of a Swarm task is not an attribute of the container;
// it is the second to last part of the name of the task.
func containerEventOf(message events.Message) protocol.ContainerEvent {
	attributes := message.Actor.Attributes
	event := protocol.ContainerEvent{
		ContainerID: message.Actor.ID,
		ServiceID:   attributes["com.docker.swarm.service.id"],
		Slot:        attributes["com.docker.swarm.task.slot"],
		NodeID:      attributes["com.docker.swarm.node.id"],
		Action:      string(message.Action),
	}
	if event.ContainerID == "" {
		// The ID of the actor is the current field; the ID of the message
		// is the deprecated one.
		event.ContainerID = message.ID
	}
	if event.Slot == "" {
		// The name of a task is <service>.<slot>.<task>, so the slot is
		// the second to last part.
		name := attributes["com.docker.swarm.task.name"]
		parts := strings.Split(name, ".")
		if len(parts) >= 3 {
			event.Slot = parts[len(parts)-2]
		}
	}
	if raw, present := attributes["exitCode"]; present {
		if code, err := strconv.Atoi(raw); err == nil {
			event.ExitCode = &code
		}
	}
	switch {
	case message.TimeNano > 0:
		event.OccurredAt = time.Unix(0, message.TimeNano).UTC()
	case message.Time > 0:
		event.OccurredAt = time.Unix(message.Time, 0).UTC()
	}
	return event
}

// sampleOf projects the stats of the Docker SDK into a raw sample.
//
// The projection only keeps the counters of the platform: the rates are
// calculated from two samples, never from one. The block device counters
// are summed over their devices and told apart by the operation.
func sampleOf(containerID string, stats container.StatsResponse, memoryLimit uint64, collectedAt time.Time) protocol.RawSample {
	var netRX, netTX uint64
	for _, network := range stats.Networks {
		netRX += network.RxBytes
		netTX += network.TxBytes
	}

	var ioRead, ioWrite uint64
	for _, entry := range stats.BlkioStats.IoServiceBytesRecursive {
		switch {
		case strings.EqualFold(entry.Op, "read"):
			ioRead += entry.Value
		case strings.EqualFold(entry.Op, "write"):
			ioWrite += entry.Value
		}
	}

	return protocol.RawSample{
		ContainerID:   containerID,
		CollectedAt:   collectedAt,
		CPUUsageNS:    stats.CPUStats.CPUUsage.TotalUsage,
		SystemUsageNS: stats.CPUStats.SystemUsage,
		OnlineCPUs:    stats.CPUStats.OnlineCPUs,
		MemoryUsage:   stats.MemoryStats.Usage,
		MemoryLimit:   memoryLimit,
		NetRX:         netRX,
		NetTX:         netTX,
		IORead:        ioRead,
		IOWrite:       ioWrite,
	}
}
