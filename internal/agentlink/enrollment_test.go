package agentlink_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/store"
)

// openEnrollment builds an enrollment service on top of a real database
// file, because the service reads and writes in transactions.
func openEnrollment(t *testing.T, now func() time.Time) (*agentlink.Enrollment, *sql.DB) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "nodes.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return agentlink.NewEnrollment(db, now), db
}

// fixedClock returns a clock and the function that moves it.
func fixedClock() (func() time.Time, func(time.Duration)) {
	current := time.Unix(1800000000, 0).UTC()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(d)
	}
	return clock, advance
}

func TestEnrollmentTokenCannotRegisterTwoNodes(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "nodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := agentlink.NewEnrollment(db, time.Now)
	ctx := context.Background()
	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"})
	if err != nil || first.NodeID == "" {
		t.Fatalf("first enrollment: %v", err)
	}
	if _, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-b"}); err == nil {
		t.Fatal("token replay accepted")
	}
}

func TestEnrollmentTokenExpires(t *testing.T) {
	clock, advance := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}

	advance(agentlink.EnrollmentTTL + time.Second)

	if _, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"}); !errors.Is(err, agentlink.ErrInvalidToken) {
		t.Fatalf("redeem an expired token: %v, want %v", err, agentlink.ErrInvalidToken)
	}
}

func TestEnrollmentTokenIsUsableBeforeItsExpiry(t *testing.T) {
	clock, advance := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	advance(agentlink.EnrollmentTTL - time.Second)

	credential, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"})
	if err != nil {
		t.Fatalf("redeem a valid token: %v", err)
	}
	if credential.NodeID == "" || credential.Token == "" {
		t.Fatalf("credential = %+v, want a node ID and a token", credential)
	}
}

func TestEnrollmentTokenIsRedeemedOnlyOnceUnderConcurrency(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}

	const attempts = 8
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		accepted   int
		unexpected []error
	)
	start := make(chan struct{})
	for index := 0; index < attempts; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, agentlink.ErrInvalidToken):
			default:
				unexpected = append(unexpected, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if accepted != 1 {
		t.Fatalf("accepted enrollments = %d, want 1", accepted)
	}
	if len(unexpected) > 0 {
		t.Fatalf("unexpected errors: %v", unexpected)
	}
}

func TestEnrollmentKeepsTheEngineIDOfAnExistingNode(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	first, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	enrolled, err := svc.Redeem(ctx, first, protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-1"})
	if err != nil {
		t.Fatalf("first enrollment: %v", err)
	}

	second, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	replacement, err := svc.Redeem(ctx, second, protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-2"})
	if !errors.Is(err, agentlink.ErrEngineIDConflict) {
		t.Fatalf("redeem a known engine ID: %v, want %v", err, agentlink.ErrEngineIDConflict)
	}
	if replacement.NodeID != "" {
		t.Fatalf("the conflicting enrollment returned the node %q", replacement.NodeID)
	}

	// The node of the first enrollment keeps its identity.
	nodes, err := svc.ListNodes(ctx)
	if err != nil {
		t.Fatalf("list the nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if nodes[0].ID != enrolled.NodeID {
		t.Fatalf("node ID = %q, want %q", nodes[0].ID, enrolled.NodeID)
	}
	if nodes[0].Hostname != "worker-1" {
		t.Fatalf("hostname = %q, want worker-1", nodes[0].Hostname)
	}
}

func TestEnrollmentRefusesASecondEnrollmentOfAnActiveNode(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	first, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	if _, err := svc.Redeem(ctx, first, protocol.NodeIdentity{EngineID: "engine-a"}); err != nil {
		t.Fatalf("the first enrollment: %v", err)
	}

	// The node is active, so the control端 does not replace it on its own.
	second, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	if _, err := svc.Redeem(ctx, second, protocol.NodeIdentity{EngineID: "engine-a"}); !errors.Is(err, agentlink.ErrEngineIDConflict) {
		t.Fatalf("redeem a known engine ID: %v, want %v", err, agentlink.ErrEngineIDConflict)
	}
}

func TestEnrollmentRegistersARevokedNodeAgain(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	first, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	enrolled, err := svc.Redeem(ctx, first, protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-1"})
	if err != nil {
		t.Fatalf("the first enrollment: %v", err)
	}
	if err := svc.Revoke(ctx, enrolled.NodeID); err != nil {
		t.Fatalf("revoke the node: %v", err)
	}

	// The administrator hands out a new token for the machine, so the node
	// may enter again.
	second, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	again, err := svc.Redeem(ctx, second, protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-1"})
	if err != nil {
		t.Fatalf("redeem the token of the revoked node: %v", err)
	}
	if again.Token == enrolled.Token {
		t.Fatal("the node received the credential of the revoked enrollment")
	}
	if again.NodeID != enrolled.NodeID {
		t.Fatalf("node ID = %q, want %q", again.NodeID, enrolled.NodeID)
	}

	// The new credential works and the old one does not.
	authenticated, err := svc.Authenticate(ctx, again.Token)
	if err != nil {
		t.Fatalf("authenticate the new credential: %v", err)
	}
	if authenticated.NodeID != again.NodeID {
		t.Fatalf("node ID = %q, want %q", authenticated.NodeID, again.NodeID)
	}
	if _, err := svc.Authenticate(ctx, enrolled.Token); !errors.Is(err, agentlink.ErrInvalidCredential) {
		t.Fatalf("authenticate the credential of the revoked node: %v, want %v", err, agentlink.ErrInvalidCredential)
	}

	// The node is not revoked any more.
	record, err := svc.NodeRecord(ctx, again.NodeID)
	if err != nil {
		t.Fatalf("read the node: %v", err)
	}
	if record.RevokedAt != nil {
		t.Fatalf("the node is revoked at %v, want no revocation", record.RevokedAt)
	}
	if record.EngineID != "engine-a" {
		t.Fatalf("engine ID = %q, want engine-a", record.EngineID)
	}
}

func TestEnrollmentUpdatesTheIdentityOfARevokedNode(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	first, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	enrolled, err := svc.Redeem(ctx, first, protocol.NodeIdentity{
		EngineID: "engine-a",
		Role:     protocol.RoleStandalone,
	})
	if err != nil {
		t.Fatalf("the first enrollment: %v", err)
	}

	// The node joins a Swarm cluster, so its role changes. The
	// administrator revokes the node and registers it again, so the record
	// holds the new role of the node.
	if err := svc.Revoke(ctx, enrolled.NodeID); err != nil {
		t.Fatalf("revoke the node: %v", err)
	}
	second, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	if _, err := svc.Redeem(ctx, second, protocol.NodeIdentity{
		EngineID: "engine-a",
		SwarmID:  "cluster-1",
		Role:     protocol.RoleManager,
	}); err != nil {
		t.Fatalf("redeem the token of the revoked node: %v", err)
	}

	record, err := svc.NodeRecord(ctx, enrolled.NodeID)
	if err != nil {
		t.Fatalf("read the node: %v", err)
	}
	if record.Role != protocol.RoleManager {
		t.Fatalf("role = %q, want %q", record.Role, protocol.RoleManager)
	}
	if record.SwarmID != "cluster-1" {
		t.Fatalf("cluster = %q, want cluster-1", record.SwarmID)
	}
}

func TestEnrollmentRejectsAnEmptyEngineID(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	if _, err := svc.Redeem(ctx, token, protocol.NodeIdentity{}); err == nil {
		t.Fatal("an enrollment without an engine ID was accepted")
	}
}

func TestEnrollmentStoresOnlyTheDigestOfTheToken(t *testing.T) {
	clock, _ := fixedClock()
	svc, db := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}

	var stored string
	if err := db.QueryRowContext(ctx, `SELECT token_hash FROM agent_tokens`).Scan(&stored); err != nil {
		t.Fatalf("read the token record: %v", err)
	}
	if stored == token {
		t.Fatal("the database holds the plain token")
	}
	digest := sha256.Sum256([]byte(token))
	if stored != hex.EncodeToString(digest[:]) {
		t.Fatalf("stored digest = %q, want the SHA-256 digest of the token", stored)
	}
}

func TestEnrollmentStoresOnlyTheDigestOfTheCredential(t *testing.T) {
	clock, _ := fixedClock()
	svc, db := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	credential, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"})
	if err != nil {
		t.Fatalf("redeem the token: %v", err)
	}

	var stored string
	if err := db.QueryRowContext(ctx, `SELECT credential_hash FROM nodes WHERE id = ?`, credential.NodeID).Scan(&stored); err != nil {
		t.Fatalf("read the node record: %v", err)
	}
	if stored == credential.Token {
		t.Fatal("the database holds the plain credential")
	}
	digest := sha256.Sum256([]byte(credential.Token))
	if stored != hex.EncodeToString(digest[:]) {
		t.Fatalf("stored digest = %q, want the SHA-256 digest of the credential", stored)
	}
}

func TestEnrollmentAuthenticatesTheIssuedCredential(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	credential, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"})
	if err != nil {
		t.Fatalf("redeem the token: %v", err)
	}

	authenticated, err := svc.Authenticate(ctx, credential.Token)
	if err != nil {
		t.Fatalf("authenticate the credential: %v", err)
	}
	if authenticated.NodeID != credential.NodeID {
		t.Fatalf("node ID = %q, want %q", authenticated.NodeID, credential.NodeID)
	}

	if _, err := svc.Authenticate(ctx, "not-the-credential"); !errors.Is(err, agentlink.ErrInvalidCredential) {
		t.Fatalf("authenticate an unknown credential: %v, want %v", err, agentlink.ErrInvalidCredential)
	}
}

func TestEnrollmentRevokeBlocksTheAuthentication(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	credential, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"})
	if err != nil {
		t.Fatalf("redeem the token: %v", err)
	}

	if err := svc.Revoke(ctx, credential.NodeID); err != nil {
		t.Fatalf("revoke the node: %v", err)
	}

	if _, err := svc.Authenticate(ctx, credential.Token); !errors.Is(err, agentlink.ErrNodeRevoked) {
		t.Fatalf("authenticate a revoked credential: %v, want %v", err, agentlink.ErrNodeRevoked)
	}
}

func TestEnrollmentRevokeReportsAnUnknownNode(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)

	if err := svc.Revoke(context.Background(), "node-that-does-not-exist"); !errors.Is(err, agentlink.ErrUnknownNode) {
		t.Fatalf("revoke an unknown node: %v, want %v", err, agentlink.ErrUnknownNode)
	}
}

func TestEnrollmentRecordsAnAuditEventOfTheAccess(t *testing.T) {
	clock, _ := fixedClock()
	svc, db := openEnrollment(t, clock)
	ctx := context.Background()

	token, err := svc.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the token: %v", err)
	}
	credential, err := svc.Redeem(ctx, token, protocol.NodeIdentity{EngineID: "engine-a"})
	if err != nil {
		t.Fatalf("redeem the token: %v", err)
	}
	if err := svc.Revoke(ctx, credential.NodeID); err != nil {
		t.Fatalf("revoke the node: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		`SELECT action, target_id, result FROM audit_events WHERE action LIKE 'agent_%' ORDER BY occurred_at`)
	if err != nil {
		t.Fatalf("read the audit events: %v", err)
	}
	defer rows.Close()

	type event struct{ action, target, result string }
	var events []event
	for rows.Next() {
		var record event
		if err := rows.Scan(&record.action, &record.target, &record.result); err != nil {
			t.Fatalf("scan the audit event: %v", err)
		}
		events = append(events, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the audit events: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("audit events = %v, want an event of the enrollment and one of the revocation", events)
	}
	if events[0].action != "agent_enroll" || events[0].result != "success" {
		t.Fatalf("first audit event = %+v, want a successful enrollment", events[0])
	}
	if events[0].target != credential.NodeID {
		t.Fatalf("the enrollment event names %q, want %q", events[0].target, credential.NodeID)
	}
	if events[1].action != "agent_revoke" || events[1].result != "success" {
		t.Fatalf("second audit event = %+v, want a successful revocation", events[1])
	}
}

func TestEnrollmentIssuesADifferentTokenEveryTime(t *testing.T) {
	clock, _ := fixedClock()
	svc, _ := openEnrollment(t, clock)
	ctx := context.Background()

	seen := make(map[string]bool)
	for index := 0; index < 8; index++ {
		token, err := svc.Issue(ctx)
		if err != nil {
			t.Fatalf("issue the token: %v", err)
		}
		if seen[token] {
			t.Fatal("the service issued the same token twice")
		}
		seen[token] = true
	}
}
