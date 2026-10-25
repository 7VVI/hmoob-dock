package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

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

// AuditMaxEvents is the number of audit events the control端 keeps.
//
// The table of the audit events has no expiry and no archive, so it has to
// be bounded: without a limit an anonymous client could fill the database of
// the control端 with events. Once the table holds this many events, the
// further events are refused and the problem is reported in the log.
const AuditMaxEvents = 10000

// auditWarningInterval is the time between two reports of a problem of the
// audit log. It keeps a control端 under a flood of requests from writing one
// log line per request.
const auditWarningInterval = time.Minute

// ErrAuditFull reports that the audit table reached its capacity, so the
// event was not stored.
var ErrAuditFull = errors.New("auth: the audit table is full")

// The target of the audit events of the authentication. The control端 has
// exactly one administrator, so every event of the authentication names the
// same target.
const auditTarget = "admin"

// sqlExecutor is the part of the database that runs a statement.
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// storeAudit writes an audit event of the authentication.
//
// A problem of the audit log does not fail the request that caused it: the
// result of the authentication must not depend on the audit log, because a
// full audit table would otherwise keep the administrator out of the
// control端 and would let the attempts bypass the budget of their source.
// The problem is reported in the log, so it does not pass unnoticed.
func (s *Service) storeAudit(ctx context.Context, executor sqlExecutor, action, result string) {
	if err := s.recordAudit(ctx, executor, action, result); err != nil {
		s.reportAuditProblem(err)
	}
}

// recordAudit stores an audit event of the authentication.
//
// The event holds no password, no token and no digest: only the action,
// its target and its result. The identifier of the event is random, so
// two events of the same action never collide.
//
// The table of the audit events is bounded. The event is only stored while
// the table holds fewer events than its capacity; a full table is reported
// as ErrAuditFull, so the caller can decide what to do about it.
func (s *Service) recordAudit(ctx context.Context, executor sqlExecutor, action, result string) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	stored, err := executor.ExecContext(ctx,
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result)
		 SELECT ?, ?, ?, ?, ? WHERE (SELECT count(*) FROM audit_events) < ?`,
		id, store.TimeToMillis(s.now()), action, auditTarget, result, s.auditLimit)
	if err != nil {
		return fmt.Errorf("auth: store the audit event: %w", err)
	}
	affected, err := stored.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: store the audit event: %w", err)
	}
	if affected == 0 {
		// The table reached its capacity, so the event was not stored.
		return ErrAuditFull
	}
	return nil
}

// reportAuditProblem reports a problem of the audit log.
//
// The report of a full table happens once, the report of the other problems
// is throttled, so a flood of requests does not fill the log of the
// control端 either. The report holds neither a credential nor a digest.
func (s *Service) reportAuditProblem(err error) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	if errors.Is(err, ErrAuditFull) {
		if s.auditFullReported {
			return
		}
		s.auditFullReported = true
		log.Printf(
			"control: the audit table is full (%d events); the further audit events are not stored",
			s.auditLimit,
		)
		return
	}

	now := s.now()
	if !s.auditLastReport.IsZero() && now.Sub(s.auditLastReport) < auditWarningInterval {
		return
	}
	s.auditLastReport = now
	log.Printf("control: the audit event could not be stored: %v", err)
}
