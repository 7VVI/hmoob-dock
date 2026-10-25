package agentlink

import (
	"context"
	"errors"
	"testing"
	"time"

	"hmoob-dock/internal/protocol"
)

// TestTheInventoryOfANodeGrowsOld reports that an inventory that is too
// old is refused.
//
// A deployment check must not work with the state of a node that nobody
// observes any more, so the control端 answers an error instead of the old
// inventory.
func TestTheInventoryOfANodeGrowsOld(t *testing.T) {
	hub := NewHub(nil)
	node := newConnection(hub, "node-1", nil, protocol.NodeIdentity{EngineID: "engine-a"})

	now := time.Now()
	node.inventory = &protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-a"},
		Containers:  []protocol.Container{{ID: "c1", Name: "web", State: "running"}},
		CollectedAt: now.Add(-2 * InventoryTimeout),
	}
	node.inventoryAt = now.Add(-2 * InventoryTimeout)

	if _, err := node.currentInventory(now); !errors.Is(err, ErrStaleInventory) {
		t.Fatalf("read the old inventory: %v, want %v", err, ErrStaleInventory)
	}

	// A fresh inventory is answered.
	node.inventoryAt = now
	if _, err := node.currentInventory(now); err != nil {
		t.Fatalf("read the fresh inventory: %v", err)
	}
}

// TestANodeWithoutAnInventoryHasNone reports that a node that sent no
// inventory has none.
func TestANodeWithoutAnInventoryHasNone(t *testing.T) {
	hub := NewHub(nil)
	node := newConnection(hub, "node-1", nil, protocol.NodeIdentity{EngineID: "engine-a"})

	if _, err := node.currentInventory(time.Now()); !errors.Is(err, ErrNoInventory) {
		t.Fatalf("read the inventory: %v, want %v", err, ErrNoInventory)
	}
}

// TestTheHeartbeatWindowDecidesAboutALostNode reports that a node whose
// agent stopped sending heartbeats counts as lost, even while its
// connection is still open.
func TestTheHeartbeatWindowDecidesAboutALostNode(t *testing.T) {
	hub := NewHub(nil)
	node := newConnection(hub, "node-1", nil, protocol.NodeIdentity{EngineID: "engine-a"})

	now := time.Now()
	node.lastSeen = now.Add(-2 * HeartbeatTimeout)
	if node.status(now).Connected {
		t.Fatal("the node counts as connected although its agent stopped sending heartbeats")
	}

	node.lastSeen = now
	if !node.status(now).Connected {
		t.Fatal("the node does not count as connected although its agent is alive")
	}
}

// TestTheHubRefusesAnUnknownMode reports that the hub only knows the modes
// of the platform.
func TestTheHubRefusesAnUnknownMode(t *testing.T) {
	hub := NewHub(nil)

	if _, err := hub.ReadInventory(context.Background(), "kubernetes", "node-1"); !errors.Is(err, ErrUnknownMode) {
		t.Fatalf("read the inventory: %v, want %v", err, ErrUnknownMode)
	}
}
