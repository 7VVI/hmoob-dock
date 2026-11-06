// Package agentlink connects the control端 with its agents.
//
// The package owns the access of the nodes: the one time enrollment token
// that an administrator hands out, the credential that a node uses
// afterwards and the live connection of the agent. Every credential is
// stored as an irreversible digest, so a stolen database hands out neither
// the enrollment token nor the credential of a node.
package agentlink

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/store"
)

// EnrollmentTTL is the time an enrollment token stays valid. The token is
// consumed by the first enrollment that uses it.
const EnrollmentTTL = 15 * time.Minute

// auditLimit is the number of audit events the control端 keeps. It matches
// the limit of the authentication, so the whole audit log has one capacity.
const auditLimit = 10000

// auditWarningInterval is the time between two reports of a problem of the
// audit log. It keeps a control端 under a flood of refused enrollments from
// writing one log line per attempt.
const auditWarningInterval = time.Minute

// The actions of the audit events of the agent access.
const (
	// AuditActionEnroll marks the enrollment of a node.
	AuditActionEnroll = "agent_enroll"
	// AuditActionRevoke marks the revocation of the credential of a node.
	AuditActionRevoke = "agent_revoke"
)

// The results of the audit events.
const (
	// AuditResultSuccess marks an action that succeeded.
	AuditResultSuccess = "success"
	// AuditResultFailure marks an action that failed.
	AuditResultFailure = "failure"
)

// The errors of the package. The HTTP layer turns them into status codes
// and messages.
var (
	// ErrInvalidToken reports an unknown, expired or consumed enrollment
	// token.
	ErrInvalidToken = errors.New("agentlink: the enrollment token is invalid")
	// ErrInvalidCredential reports an unknown credential.
	ErrInvalidCredential = errors.New("agentlink: the credential is invalid")
	// ErrNodeRevoked reports the credential of a revoked node.
	ErrNodeRevoked = errors.New("agentlink: the credential of the node is revoked")
	// ErrEngineIDConflict reports an engine ID that is enrolled already.
	// The control端 does not replace a node on its own, the administrator
	// has to handle the conflict.
	ErrEngineIDConflict = errors.New("agentlink: a node with this engine ID exists already")
	// ErrUnknownNode reports a node that is not enrolled.
	ErrUnknownNode = errors.New("agentlink: the node is unknown")
	// ErrInvalidIdentity reports an identity that cannot name a node.
	ErrInvalidIdentity = errors.New("agentlink: the identity of the node is incomplete")
	// errAuditFull reports that the audit table reached its capacity, so
	// the event was not stored.
	errAuditFull = errors.New("agentlink: the audit table is full")
)

// Credential is the credential of a node.
type Credential struct {
	// NodeID identifies the node in the control端.
	NodeID string
	// Token is the plain credential. It only lives in the request that
	// carries it and in the file of the agent.
	Token string
}

// Node is a node that is enrolled at the control端.
type Node struct {
	// ID identifies the node in the control端.
	ID string
	// EngineID is the ID of the Docker engine of the node. It anchors the
	// identity of the node.
	EngineID string
	// SwarmID is the ID of the Swarm cluster of the node.
	SwarmID string
	// SwarmNodeID is the ID of the node inside its Swarm cluster.
	SwarmNodeID string
	// Role is the role of the node inside its Swarm cluster.
	Role string
	// Hostname is the name of the host of the node.
	Hostname string
	// DockerVersion is the version of the Docker engine, as the node
	// reported it with its last inventory.
	DockerVersion string
	// OSType is the type of the operating system of the engine, for
	// example linux.
	OSType string
	// OSArch is the architecture of the engine host, for example x86_64.
	OSArch string
	// AgentVersion is the version of the agent, as the hello of the last
	// connection reported it.
	AgentVersion string
	// CreatedAt is the time the node was enrolled.
	CreatedAt time.Time
	// RevokedAt is the time the credential of the node was revoked. It is
	// nil while the node is allowed to connect.
	RevokedAt *time.Time
}

// Enrollment manages the enrollment tokens and the credentials of the
// nodes.
type Enrollment struct {
	db  *sql.DB
	now func() time.Time
	// revokeMu guards the callbacks that close the connections of the
	// revoked nodes.
	revokeMu sync.RWMutex
	// revokeHooks are called when the credential of a node is revoked.
	revokeHooks []func(nodeID string)
	// auditMu guards the state of the reports about the audit log.
	auditMu sync.Mutex
	// auditFullReported remembers that a full audit table was reported, so
	// the report does not repeat itself with every attempt.
	auditFullReported bool
	// auditLastReport is the moment of the last report of a problem of the
	// audit log.
	auditLastReport time.Time
}

// New builds the enrollment service on top of the given database. The clock
// provides every timestamp and expiry; a nil clock falls back to the system
// clock.
func NewEnrollment(db *sql.DB, now func() time.Time) *Enrollment {
	if now == nil {
		now = time.Now
	}
	return &Enrollment{db: db, now: now}
}

// OnRevoke registers a callback that is called when the credential of a node
// is revoked. The hub uses it to close the connection of the node, so a
// revoked agent cannot keep talking to the control端.
func (e *Enrollment) OnRevoke(fn func(nodeID string)) {
	if fn == nil {
		return
	}
	e.revokeMu.Lock()
	defer e.revokeMu.Unlock()
	e.revokeHooks = append(e.revokeHooks, fn)
}

// Issue hands out a one time enrollment token. The plain token only exists
// in the answer of the call, the database keeps its digest.
func (e *Enrollment) Issue(ctx context.Context) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}

	now := e.now()
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO agent_tokens (id, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		id, digestOf(token), store.TimeToMillis(now), store.TimeToMillis(now.Add(EnrollmentTTL))); err != nil {
		return "", fmt.Errorf("agentlink: store the enrollment token: %w", err)
	}
	return token, nil
}

// Redeem consumes an enrollment token and enrolls the node that owns the
// given identity.
//
// The token and the node are created in one transaction, so a token is
// never consumed without a node and a node never exists without a consumed
// token. The engine ID of the identity anchors the node: an engine ID that
// is enrolled already is a conflict and does not replace the node that
// exists already.
//
// A node that was revoked is the exception: the administrator hands out a
// new enrollment token for it, so the machine may enter again. The token is
// the proof of that decision, so the redemption is not possible without it.
func (e *Enrollment) Redeem(ctx context.Context, token string, identity protocol.NodeIdentity) (Credential, error) {
	if strings.TrimSpace(token) == "" {
		return Credential{}, ErrInvalidToken
	}
	if strings.TrimSpace(identity.EngineID) == "" {
		return Credential{}, ErrInvalidIdentity
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, fmt.Errorf("agentlink: start the enrollment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record, err := e.findToken(ctx, tx, token)
	if err != nil {
		return Credential{}, err
	}
	if record == "" {
		if err := e.failEnrollment(ctx, tx, identity.EngineID); err != nil {
			return Credential{}, err
		}
		return Credential{}, ErrInvalidToken
	}

	existing, err := e.findNodeByEngine(ctx, tx, identity.EngineID)
	if err != nil {
		return Credential{}, err
	}
	if existing != nil && existing.RevokedAt == nil {
		// The node is active, so the control端 does not replace it on its
		// own: the administrator has to handle the conflict.
		if err := e.failEnrollment(ctx, tx, identity.EngineID); err != nil {
			return Credential{}, err
		}
		return Credential{}, ErrEngineIDConflict
	}

	credential, err := randomToken()
	if err != nil {
		return Credential{}, err
	}
	now := e.now()
	nodeID, err := e.storeNode(ctx, tx, existing, identity, credential, now)
	if err != nil {
		return Credential{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE agent_tokens SET consumed_at = ? WHERE id = ?`,
		store.TimeToMillis(now), record); err != nil {
		return Credential{}, fmt.Errorf("agentlink: consume the enrollment token: %w", err)
	}
	e.recordAudit(ctx, tx, AuditActionEnroll, AuditResultSuccess, nodeID)
	if err := tx.Commit(); err != nil {
		return Credential{}, fmt.Errorf("agentlink: commit the enrollment: %w", err)
	}
	return Credential{NodeID: nodeID, Token: credential}, nil
}

// storeNode writes the node of an enrollment and returns its identifier.
//
// A node that does not exist yet is created. A node that was revoked keeps
// its identifier and gets the new credential: the row of the node stays,
// only the revocation and the credential change.
func (e *Enrollment) storeNode(
	ctx context.Context,
	tx *sql.Tx,
	existing *Node,
	identity protocol.NodeIdentity,
	credential string,
	now time.Time,
) (string, error) {
	if existing == nil {
		nodeID, err := randomID()
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (id, engine_id, swarm_id, swarm_node_id, role, hostname, credential_hash, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			nodeID, identity.EngineID, identity.SwarmID, identity.SwarmNodeID, identity.Role, identity.Hostname,
			digestOf(credential), store.TimeToMillis(now)); err != nil {
			return "", fmt.Errorf("agentlink: store the node: %w", err)
		}
		return nodeID, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE nodes SET swarm_id = ?, swarm_node_id = ?, role = ?, hostname = ?, credential_hash = ?, revoked_at = NULL
		 WHERE id = ?`,
		identity.SwarmID, identity.SwarmNodeID, identity.Role, identity.Hostname,
		digestOf(credential), existing.ID); err != nil {
		return "", fmt.Errorf("agentlink: store the node: %w", err)
	}
	return existing.ID, nil
}

// findNodeByEngine returns the enrolled node of the given engine ID. It
// returns nil when no node uses the engine.
func (e *Enrollment) findNodeByEngine(ctx context.Context, tx *sql.Tx, engineID string) (*Node, error) {
	var node Node
	var createdAt int64
	var revokedAt sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT id, engine_id, swarm_id, swarm_node_id, role, hostname, created_at, revoked_at
		 FROM nodes WHERE engine_id = ?`, engineID).
		Scan(&node.ID, &node.EngineID, &node.SwarmID, &node.SwarmNodeID, &node.Role, &node.Hostname,
			&createdAt, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("agentlink: read the node: %w", err)
	}
	node.CreatedAt = store.TimeFromMillis(createdAt)
	if revokedAt.Valid {
		revoked := store.TimeFromMillis(revokedAt.Int64)
		node.RevokedAt = &revoked
	}
	return &node, nil
}

// Revoke withdraws the credential of a node. The node cannot authenticate
// again and its connection is closed. The containers that run on the node
// are not touched.
func (e *Enrollment) Revoke(ctx context.Context, nodeID string) error {
	if strings.TrimSpace(nodeID) == "" {
		return ErrUnknownNode
	}

	now := e.now()
	result, err := e.db.ExecContext(ctx,
		`UPDATE nodes SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		store.TimeToMillis(now), nodeID)
	if err != nil {
		return fmt.Errorf("agentlink: revoke the node: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("agentlink: revoke the node: %w", err)
	}
	if affected == 0 {
		// The node is unknown or revoked already. An unknown node is an
		// error, a revoked one is not: the request of the administrator
		// reached its goal already.
		var known int
		if err := e.db.QueryRowContext(ctx,
			`SELECT count(*) FROM nodes WHERE id = ?`, nodeID).Scan(&known); err != nil {
			return fmt.Errorf("agentlink: read the node: %w", err)
		}
		if known == 0 {
			return ErrUnknownNode
		}
		return nil
	}

	e.recordAudit(ctx, e.db, AuditActionRevoke, AuditResultSuccess, nodeID)
	e.closeConnections(nodeID)
	return nil
}

// Authenticate returns the credential that the given token identifies. An
// unknown credential is reported as ErrInvalidCredential, the credential of
// a revoked node as ErrNodeRevoked.
func (e *Enrollment) Authenticate(ctx context.Context, token string) (Credential, error) {
	if strings.TrimSpace(token) == "" {
		return Credential{}, ErrInvalidCredential
	}

	digest := digestOf(token)
	var id, stored string
	var revokedAt sql.NullInt64
	err := e.db.QueryRowContext(ctx,
		`SELECT id, credential_hash, revoked_at FROM nodes WHERE credential_hash = ?`, digest).
		Scan(&id, &stored, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Credential{}, ErrInvalidCredential
	case err != nil:
		return Credential{}, fmt.Errorf("agentlink: read the node: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(digest)) != 1 {
		return Credential{}, ErrInvalidCredential
	}
	if revokedAt.Valid {
		return Credential{}, ErrNodeRevoked
	}
	return Credential{NodeID: id, Token: token}, nil
}

// nodeColumns are the columns every reader of a node selects.
const nodeColumns = `id, engine_id, swarm_id, swarm_node_id, role, hostname,
	docker_version, os_type, os_arch, agent_version, created_at, revoked_at`

// scanNode reads one row of the nodes table into the given node.
func scanNode(scanner interface{ Scan(...any) error }) (Node, error) {
	var node Node
	var createdAt int64
	var revokedAt sql.NullInt64
	if err := scanner.Scan(&node.ID, &node.EngineID, &node.SwarmID, &node.SwarmNodeID, &node.Role, &node.Hostname,
		&node.DockerVersion, &node.OSType, &node.OSArch, &node.AgentVersion,
		&createdAt, &revokedAt); err != nil {
		return Node{}, err
	}
	node.CreatedAt = store.TimeFromMillis(createdAt)
	if revokedAt.Valid {
		revoked := store.TimeFromMillis(revokedAt.Int64)
		node.RevokedAt = &revoked
	}
	return node, nil
}

// ListNodes returns the enrolled nodes.
func (e *Enrollment) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := e.db.QueryContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("agentlink: read the nodes: %w", err)
	}
	defer rows.Close()

	var nodes []Node
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("agentlink: scan the node: %w", err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentlink: read the nodes: %w", err)
	}
	return nodes, nil
}

// NodeRecord returns the record of a node.
func (e *Enrollment) NodeRecord(ctx context.Context, nodeID string) (Node, error) {
	node, err := scanNode(e.db.QueryRowContext(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, nodeID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Node{}, ErrUnknownNode
	case err != nil:
		return Node{}, fmt.Errorf("agentlink: read the node: %w", err)
	}
	return node, nil
}

// UpdateNodeEnvironment stores the environment a node reported with its
// last inventory.
//
// The write keeps only the facts of the report. It runs for every
// inventory that arrives, so it is deliberately cheap: a single row is
// updated only when the report differs from the stored one, and a node
// that is away keeps the facts of its last report.
func (e *Enrollment) UpdateNodeEnvironment(ctx context.Context, nodeID string, identity protocol.NodeIdentity, agentVersion string) error {
	if strings.TrimSpace(nodeID) == "" {
		return ErrUnknownNode
	}
	result, err := e.db.ExecContext(ctx,
		`UPDATE nodes SET docker_version = ?, os_type = ?, os_arch = ?, agent_version = ?
		 WHERE id = ?`,
		identity.ServerVersion, identity.OSType, identity.Architecture, agentVersion, nodeID)
	if err != nil {
		return fmt.Errorf("agentlink: store the environment of the node: %w", err)
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return ErrUnknownNode
	}
	return nil
}

// findToken returns the identifier of the enrollment token that matches the
// given token and is still usable. The digest is compared in constant time.
func (e *Enrollment) findToken(ctx context.Context, tx *sql.Tx, token string) (string, error) {
	digest := digestOf(token)
	var id, stored string
	var expiresAt int64
	var consumedAt sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT id, token_hash, expires_at, consumed_at FROM agent_tokens WHERE token_hash = ?`, digest).
		Scan(&id, &stored, &expiresAt, &consumedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("agentlink: read the enrollment tokens: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(digest)) != 1 {
		return "", nil
	}
	if consumedAt.Valid {
		return "", nil
	}
	if !store.TimeFromMillis(expiresAt).After(e.now()) {
		return "", nil
	}
	return id, nil
}

// failEnrollment rolls the enrollment back and records the failed attempt.
//
// The rollback happens before the audit event is written, because the write
// pool of the control端 holds a single connection: a write on the pool
// would wait for the transaction that is still open.
func (e *Enrollment) failEnrollment(ctx context.Context, tx *sql.Tx, target string) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("agentlink: roll back the enrollment: %w", err)
	}
	e.recordAudit(ctx, e.db, AuditActionEnroll, AuditResultFailure, target)
	return nil
}

// closeConnections tells the registered parts that the credential of the
// node is gone, so the connection of the node is closed.
func (e *Enrollment) closeConnections(nodeID string) {
	e.revokeMu.RLock()
	hooks := make([]func(string), len(e.revokeHooks))
	copy(hooks, e.revokeHooks)
	e.revokeMu.RUnlock()

	for _, hook := range hooks {
		hook(nodeID)
	}
}

// sqlExecutor is the part of the database that runs a statement.
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// recordAudit writes an audit event of the agent access.
//
// A problem of the audit log does not fail the operation that caused it:
// the access of a node must not depend on the audit log, because a full
// audit table would otherwise keep the nodes out of the control端. The
// problem is reported in the log, so it does not pass unnoticed.
//
// The event holds no token and no digest: only the action, its target and
// its result.
func (e *Enrollment) recordAudit(ctx context.Context, executor sqlExecutor, action, result, target string) {
	if err := e.insertAudit(ctx, executor, action, result, target); err != nil {
		e.reportAuditProblem(err)
	}
}

// reportAuditProblem reports a problem of the audit log.
//
// The report of a full table happens once, the report of the other problems
// is throttled, so a flood of refused enrollments does not fill the log of
// the control端 with one line per attempt. The report holds neither a
// credential nor a digest.
func (e *Enrollment) reportAuditProblem(err error) {
	e.auditMu.Lock()
	defer e.auditMu.Unlock()

	if errors.Is(err, errAuditFull) {
		if e.auditFullReported {
			return
		}
		e.auditFullReported = true
		log.Printf(
			"control: the audit table is full (%d events); the further audit events are not stored",
			auditLimit,
		)
		return
	}

	now := e.now()
	if !e.auditLastReport.IsZero() && now.Sub(e.auditLastReport) < auditWarningInterval {
		return
	}
	e.auditLastReport = now
	log.Printf("control: the audit event could not be stored: %v", err)
}

// insertAudit stores an audit event of the agent access.
func (e *Enrollment) insertAudit(ctx context.Context, executor sqlExecutor, action, result, target string) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	stored, err := executor.ExecContext(ctx,
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result)
		 SELECT ?, ?, ?, ?, ? WHERE (SELECT count(*) FROM audit_events) < ?`,
		id, store.TimeToMillis(e.now()), action, target, result, auditLimit)
	if err != nil {
		return fmt.Errorf("agentlink: store the audit event: %w", err)
	}
	affected, err := stored.RowsAffected()
	if err != nil {
		return fmt.Errorf("agentlink: store the audit event: %w", err)
	}
	if affected == 0 {
		// The table reached its capacity, so the event was not stored.
		return errAuditFull
	}
	return nil
}

// digestOf returns the hex encoded SHA-256 digest of the given value.
func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// randomToken returns a random token that identifies a credential.
func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("agentlink: generate a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// randomID returns a random identifier.
func randomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("agentlink: generate an identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
