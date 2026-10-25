package auth

import (
	"context"
	"database/sql"
	"fmt"

	"hmoob-dock/internal/store"
)

// The actions of the audit events of the authentication.
const (
	// AuditActionLogin marks a login of the administrator.
	AuditActionLogin = "login"
	// AuditActionInitialize marks the creation of the administrator.
	AuditActionInitialize = "initialize"
	// AuditActionPasswordChange marks a change of the password.
	AuditActionPasswordChange = "password_change"
)

// The results of the audit events.
const (
	// AuditResultSuccess marks an action that succeeded.
	AuditResultSuccess = "success"
	// AuditResultFailure marks an action that failed.
	AuditResultFailure = "failure"
)

// auditTarget is the target of the audit events of the authentication. The
// control端 has exactly one administrator, so every event of the
// authentication names the same target.
const auditTarget = "admin"

// sqlExecutor is the part of the database that runs a statement.
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// recordAudit stores an audit event of the authentication.
//
// The event holds no password, no token and no digest: only the action,
// its target and its result. The identifier of the event is random, so
// two events of the same action never collide.
func (s *Service) recordAudit(ctx context.Context, executor sqlExecutor, action, result string) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	if _, err := executor.ExecContext(ctx,
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result) VALUES (?, ?, ?, ?, ?)`,
		id, store.TimeToMillis(s.now()), action, auditTarget, result); err != nil {
		return fmt.Errorf("auth: store the audit event: %w", err)
	}
	return nil
}
