package dockerops

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/errdefs"

	"hmoob-dock/internal/protocol"
)

// multiplexFrame builds one frame of the stdout/stderr multiplex of Docker.
func multiplexFrame(stream byte, text string) []byte {
	frame := make([]byte, logHeaderSize+len(text))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:logHeaderSize], uint32(len(text)))
	copy(frame[logHeaderSize:], text)
	return frame
}

// TestTheMultiplexIsDecodedInOrder proves that stdout and stderr are read
// as text in the order Docker sent them.
func TestTheMultiplexIsDecodedInOrder(t *testing.T) {
	data := append(multiplexFrame(1, "hello\n"), multiplexFrame(2, "error\n")...)

	var chunks []string
	if err := readLogStream(bytes.NewReader(data), func(p []byte) error {
		chunks = append(chunks, string(p))
		return nil
	}); err != nil {
		t.Fatalf("read the multiplex: %v", err)
	}
	if len(chunks) != 2 || chunks[0] != "hello\n" || chunks[1] != "error\n" {
		t.Fatalf("chunks = %q, want the two frames in order", chunks)
	}
}

// TestALongFrameIsSplitBelowTheFrameLimit proves that the payload of one
// Docker frame is handed over in pieces of at most 32 KiB.
func TestALongFrameIsSplitBelowTheFrameLimit(t *testing.T) {
	text := bytes.Repeat([]byte("x"), protocol.MaxLogFrameTextSize+1024)
	data := multiplexFrame(1, string(text))

	var chunks [][]byte
	if err := readLogStream(bytes.NewReader(data), func(p []byte) error {
		chunks = append(chunks, append([]byte(nil), p...))
		return nil
	}); err != nil {
		t.Fatalf("read the multiplex: %v", err)
	}
	total := 0
	for _, chunk := range chunks {
		if len(chunk) > protocol.MaxLogFrameTextSize {
			t.Fatalf("a chunk holds %d bytes, want at most %d", len(chunk), protocol.MaxLogFrameTextSize)
		}
		total += len(chunk)
	}
	if total != len(text) {
		t.Fatalf("the chunks hold %d bytes, want %d", total, len(text))
	}
}

// TestATruncatedMultiplexIsAnError proves that a stream that ends in the
// middle of a frame is reported and not treated as a complete line.
func TestATruncatedMultiplexIsAnError(t *testing.T) {
	frame := multiplexFrame(1, "hello")
	err := readLogStream(bytes.NewReader(frame[:len(frame)-2]), func([]byte) error { return nil })
	if err == nil {
		t.Fatal("a truncated multiplex was accepted")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("the error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
}

// TestATtyStreamIsNotDecodedAsMultiplex proves that the raw stream of a TTY
// container is not mistaken for frames: bytes that look like a header stay
// part of the text.
func TestATtyStreamIsNotDecodedAsMultiplex(t *testing.T) {
	raw := []byte("\x01\x00\x00\x00\x00\x00\x00\x05hello")
	var text []byte
	if err := readRawLogStream(bytes.NewReader(raw), func(p []byte) error {
		text = append(text, p...)
		return nil
	}); err != nil {
		t.Fatalf("read the raw stream: %v", err)
	}
	if !bytes.Equal(text, raw) {
		t.Fatalf("the text = %q, want the raw bytes %q", text, raw)
	}
}

// TestTheSampleUsesTheFieldsOfTheEngine proves the projection of the stats:
// the CPU counters, the current memory usage, the sums of the network
// interfaces and the read and write blocks of the block device.
func TestTheSampleUsesTheFieldsOfTheEngine(t *testing.T) {
	collectedAt := time.Unix(100, 0).UTC()
	stats := container.StatsResponse{
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 1234},
			SystemUsage: 5678,
			OnlineCPUs:  4,
		},
		MemoryStats: container.MemoryStats{Usage: 700, Limit: 999},
		Networks: map[string]container.NetworkStats{
			"eth0": {RxBytes: 100, TxBytes: 200},
			"eth1": {RxBytes: 300, TxBytes: 400},
		},
		BlkioStats: container.BlkioStats{IoServiceBytesRecursive: []container.BlkioStatEntry{
			{Op: "Read", Value: 1000},
			{Op: "read", Value: 500},
			{Op: "Write", Value: 2000},
			{Op: "write", Value: 500},
			{Op: "Sync", Value: 9999},
		}},
	}

	sample := sampleOf("c1", stats, 512, collectedAt)
	if sample.ContainerID != "c1" || !sample.CollectedAt.Equal(collectedAt) {
		t.Fatalf("the sample = %+v, want the container and the time of the test", sample)
	}
	if sample.CPUUsageNS != 1234 || sample.SystemUsageNS != 5678 || sample.OnlineCPUs != 4 {
		t.Fatalf("the CPU fields = %+v, want the counters of the engine", sample)
	}
	if sample.MemoryUsage != 700 || sample.MemoryLimit != 512 {
		t.Fatalf("the memory = %d/%d, want the usage and the explicit limit", sample.MemoryUsage, sample.MemoryLimit)
	}
	if unlimited := sampleOf("c1", stats, 0, collectedAt); unlimited.MemoryLimit != 0 {
		t.Fatalf("the limit of an unlimited container = %d, want 0", unlimited.MemoryLimit)
	}
	if sample.NetRX != 400 || sample.NetTX != 600 {
		t.Fatalf("the network = %d/%d, want the sum of the interfaces", sample.NetRX, sample.NetTX)
	}
	if sample.IORead != 1500 || sample.IOWrite != 2500 {
		t.Fatalf("the IO = %d/%d, want the read and write blocks", sample.IORead, sample.IOWrite)
	}
}

// TestAnEventCarriesTheSwarmIdentity proves the projection of a Docker
// event: the exit code of a container that ended and the identity of its
// Swarm task.
func TestAnEventCarriesTheSwarmIdentity(t *testing.T) {
	message := events.Message{
		Type:   "container",
		Action: "die",
		Actor: events.Actor{
			ID: "c1",
			Attributes: map[string]string{
				"exitCode":                    "3",
				"com.docker.swarm.service.id": "service-1",
				"com.docker.swarm.node.id":    "node-1",
				"com.docker.swarm.task.name":  "stack_api.2.abcdef",
			},
		},
		TimeNano: time.Unix(100, 0).UnixNano(),
	}

	event := containerEventOf(message)
	if event.ContainerID != "c1" || event.Action != "die" {
		t.Fatalf("the event = %+v, want the container and the action of the message", event)
	}
	if event.ServiceID != "service-1" || event.NodeID != "node-1" || event.Slot != "2" {
		t.Fatalf("the swarm identity = %+v, want the service, the node and the slot", event)
	}
	if event.ExitCode == nil || *event.ExitCode != 3 {
		t.Fatalf("the exit code = %v, want 3", event.ExitCode)
	}
	if !event.OccurredAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("the time = %s, want the time of the message", event.OccurredAt)
	}

	plain := containerEventOf(events.Message{
		Type:   "container",
		Action: "start",
		Actor:  events.Actor{ID: "c2"},
	})
	if plain.ExitCode != nil {
		t.Fatalf("the exit code of a started container = %v, want none", *plain.ExitCode)
	}
}

// TestAMissingContainerIsRecognised proves that the error of the engine is
// kept as a sentinel of the package, so the agent can name the reason.
func TestAMissingContainerIsRecognised(t *testing.T) {
	err := wrapLogError("inspect the container", errdefs.NotFound(errors.New("no such container")))
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("the error = %v, want %v", err, ErrContainerNotFound)
	}

	err = wrapLogError("read the logs", errdefs.NotImplemented(errors.New("the driver does not support reading")))
	if !errors.Is(err, ErrLogsUnavailable) {
		t.Fatalf("the error = %v, want %v", err, ErrLogsUnavailable)
	}
}
