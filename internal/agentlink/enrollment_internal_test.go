package agentlink

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/store"
)

// TestTheAuditProblemIsReportedOnce reports that a flood of refused
// enrollments does not write one log line per attempt.
//
// The report of a problem of the audit log is aggregated, so the log of the
// control端 stays readable while it is under a flood.
func TestTheAuditProblemIsReportedOnce(t *testing.T) {
	buffer := &bytes.Buffer{}
	writer := log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() { log.SetOutput(writer) })

	db, err := store.Open(filepath.Join(t.TempDir(), "nodes.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	svc := NewEnrollment(db, time.Now)
	ctx := context.Background()

	// The table of the audit events is full, so every further event is
	// refused.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result)
		 SELECT 'fill-' || value, 0, 'fill', 'admin', 'success'
		 FROM (WITH RECURSIVE seq(value) AS (SELECT 1 UNION ALL SELECT value + 1 FROM seq WHERE value < ?)
		       SELECT value FROM seq)`,
		auditLimit); err != nil {
		t.Fatalf("fill the audit events: %v", err)
	}

	for attempt := 0; attempt < 3; attempt++ {
		if _, err := svc.Redeem(ctx, "not-a-token", protocol.NodeIdentity{EngineID: "engine-a"}); err == nil {
			t.Fatal("the enrollment of an unknown token was accepted")
		}
	}

	reports := strings.Count(buffer.String(), "audit table is full")
	if reports != 1 {
		t.Fatalf("reports about the full audit table = %d, want 1; the log = %q", reports, buffer.String())
	}
}
