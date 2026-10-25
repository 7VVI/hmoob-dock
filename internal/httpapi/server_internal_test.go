package httpapi

import (
	"bytes"
	"path/filepath"
	"testing"

	"hmoob-dock/internal/config"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// TestReadPoolObservesTheRowsOfTheInjectedWritePool proves that the read
// pool of the server and the injected write pool address the same database:
// a row written through the write pool is visible through the read pool.
func TestReadPoolObservesTheRowsOfTheInjectedWritePool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")

	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result) VALUES ('evt-1', 1700000000000, 'login', 'admin', 'success')`,
	); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}

	box, err := secret.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}

	handler, err := NewServer(db, box, config.Control{DBPath: path})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	server, ok := handler.(*Server)
	if !ok {
		t.Fatal("NewServer did not return a *Server")
	}
	t.Cleanup(func() { server.Close() })

	var count int
	if err := server.reads.QueryRow(
		`SELECT count(*) FROM audit_events WHERE id = 'evt-1'`,
	).Scan(&count); err != nil {
		t.Fatalf("read through the read pool: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit events = %d, want 1", count)
	}
}
