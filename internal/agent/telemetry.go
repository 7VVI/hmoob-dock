package agent

import (
	"context"
	"log"
	"sync"
	"time"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/protocol"
)

// The limits of the telemetry of a node.
const (
	// maxConcurrentSamples is the number of the samples a round reads at
	// the same time. The limit keeps a node with many containers from
	// opening a request per container at once.
	maxConcurrentSamples = 8
)

// The intervals of the telemetry. They are variables so a test does not
// have to wait for the real interval.
var (
	// sampleInterval is the time between two rounds of the samples of the
	// containers of the node.
	sampleInterval = 10 * time.Second
	// sampleTimeout bounds one sample. A container whose sample does not
	// answer in time is skipped, so it does not hold the whole round.
	sampleTimeout = 5 * time.Second
	// eventRetryDelay is the time before a stream of the events of Docker
	// is opened again.
	eventRetryDelay = time.Second
)

// sampler reads the stats of one container.
type sampler interface {
	// Sample returns one sample of a container.
	Sample(ctx context.Context, containerID string) (protocol.RawSample, error)
}

// eventSource streams the events of the containers of the engine.
type eventSource interface {
	// Events hands the events of Docker to emit until the context ends or
	// the stream fails.
	Events(ctx context.Context, emit func(protocol.ContainerEvent) error) error
}

// The Docker engine of the node provides the telemetry of the agent.
var (
	_ sampler     = (*dockerops.Local)(nil)
	_ eventSource = (*dockerops.Local)(nil)
	_ logSource   = (*dockerops.Local)(nil)
)

// reportSamples reads the containers of the node every interval and sends
// one raw sample of every running container.
//
// The samples are read immediately and then in every interval. The agent
// keeps no history: a sample that cannot be sent is lost, and a control端
// that was away does not receive the samples of the gap. The inventory of
// the node remains the source of the current containers.
func reportSamples(ctx context.Context, engine source, sampling sampler, out sender) {
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()

	for {
		inventory, err := engine.Inventory(ctx)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				log.Printf("agent: read the inventory for the samples: %v", err)
			}
		case ctx.Err() != nil:
			return
		default:
			sampleRound(ctx, inventory.Containers, sampling, out, maxConcurrentSamples, sampleTimeout)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sampleRound reads the samples of the given containers with a bounded
// number of requests at the same time.
//
// A container that fails or runs out of time is reported and skipped; the
// other containers of the round are sampled anyway. The samples are handed
// to the sender as they arrive, so a slow container does not delay the
// samples of the others.
func sampleRound(ctx context.Context, containers []protocol.Container, sampling sampler, out sender, concurrency int, timeout time.Duration) {
	if sampling == nil || out == nil {
		return
	}
	if concurrency < 1 {
		concurrency = 1
	}

	jobs := make(chan string)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for index := 0; index < concurrency; index++ {
		go func() {
			defer workers.Done()
			for containerID := range jobs {
				sampleCtx, cancel := context.WithTimeout(ctx, timeout)
				sample, err := sampling.Sample(sampleCtx, containerID)
				cancel()
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("agent: sample the container %s: %v", containerID, err)
					}
					continue
				}
				if err := out.sendLog(protocol.TypeMetricsSample, sample); err != nil {
					// The sample is lost; the metrics of the control端
					// show a gap and never a zero rate.
					if ctx.Err() == nil {
						log.Printf("agent: send the sample of the container %s: %v", containerID, err)
					}
				}
			}
		}()
	}

	for _, container := range containers {
		if container.State != "running" {
			// The stats of a container that ended are not a running
			// observation.
			continue
		}
		select {
		case jobs <- container.ID:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return
		}
	}
	close(jobs)
	workers.Wait()
}

// reportEvents forwards the events of the containers of Docker to the
// control端.
//
// The events are a stream of the engine of its own. A stream that ends is
// opened again after a delay; the events of the gap are not invented. The
// inventory of the node tells the control端 the current state after such a
// gap.
func reportEvents(ctx context.Context, events eventSource, out sender) {
	for {
		err := events.Events(ctx, func(event protocol.ContainerEvent) error {
			// The node of the source is the authenticated connection of
			// the agent, not the NodeID the event carries. The control端
			// decides about the source with the connection.
			return out.sendControl(protocol.TypeContainerEvent, event)
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("agent: the event stream of Docker ended: %v", err)
		}

		timer := time.NewTimer(eventRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
