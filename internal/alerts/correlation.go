package alerts

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"hmoob-dock/internal/protocol"
)

// The labels Docker writes on the container of a swarm task.
const (
	labelServiceID = "com.docker.swarm.service.id"
	labelSlot      = "com.docker.swarm.task.slot"
	labelNodeID    = "com.docker.swarm.node.id"
)

// randomID makes an identifier for an event or a notification.
func randomID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// The identifier of an event is not a secret: the time keeps the
		// values apart when the system source is not available.
		return fmt.Sprintf("alert-%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}

// ObjectID names the object a container belongs to.
//
// A container of a swarm keeps its identity across a replacement, so a
// rolling update does not open and close an event for every replica: a
// replicated service is identified by its slot, a global service by its
// node, because both survive a new task of the same place. A standalone
// container is identified by itself, so a new container is a new object.
func ObjectID(nodeID string, container protocol.Container) string {
	serviceID := container.Labels[labelServiceID]
	if serviceID == "" {
		return "container:" + container.ID
	}
	if slot, ok := slotOf(container); ok {
		return fmt.Sprintf("service:%s@slot:%d", serviceID, slot)
	}
	taskNode := container.Labels[labelNodeID]
	if taskNode == "" {
		taskNode = nodeID
	}
	return fmt.Sprintf("service:%s@node:%s", serviceID, taskNode)
}

// slotOf reads the slot of a replicated task. A global task has no slot, so
// its absence tells the two modes apart.
func slotOf(container protocol.Container) (int, bool) {
	raw, ok := container.Labels[labelSlot]
	if !ok || raw == "" {
		return 0, false
	}
	slot, err := strconv.Atoi(raw)
	if err != nil || slot <= 0 {
		return 0, false
	}
	return slot, true
}

// ExitObservation builds the sample of a container that stopped.
//
// expected reports whether a known action explains the end: a one shot task
// that succeeded, or a container that the platform replaced while the new
// one took over.
func ExitObservation(nodeID string, container protocol.Container, at time.Time, expected bool) Observation {
	value := 1.0
	if expected {
		value = 0
	}
	return Observation{
		ObjectID:       ObjectID(nodeID, container),
		At:             at.UTC(),
		Value:          value,
		Valid:          true,
		ExpectedAction: expected,
	}
}

// ExpectedExit reports whether a stop of a container has an explanation the
// platform knows.
//
// Only an exit code of zero counts. A non zero exit code is always an
// anomaly: a replacement whose new container fails must still reach the
// administrator, and it must not be suppressed by the fact that the old
// container was replaced on purpose.
func ExpectedExit(container protocol.Container, replaced bool, oneShot bool) bool {
	if container.ExitCode == nil || *container.ExitCode != 0 {
		return false
	}
	return replaced || oneShot
}
