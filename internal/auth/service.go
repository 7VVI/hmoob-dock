// Package auth manages the single administrator of the control端: the one
// time bootstrap credential, the password, the sessions and the CSRF tokens
// that guard the modifying requests.
//
// Every credential is stored as an irreversible digest. The password becomes
// an Argon2id hash, the bootstrap credential and the session token become
// SHA-256 digests. The plain values only live in the request that carries
// them.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"hmoob-dock/internal/store"
)

const (
	// BootstrapTTL is the time the bootstrap credential stays valid.
	BootstrapTTL = 15 * time.Minute
	// SessionTTL is the time a session stays valid.
	SessionTTL = 8 * time.Hour
	// administratorID is the only identifier the single administrator has.
	administratorID = 1
)

// The errors of the package. The HTTP layer turns them into status codes
// and messages, and it keeps the messages of the credential failures alike
// so that they do not reveal which of them occurred.
var (
	// ErrNotInitialized reports that no administrator exists yet.
	ErrNotInitialized = errors.New("auth: the administrator is not initialized")
	// ErrAlreadyInitialized reports that the administrator exists already.
	ErrAlreadyInitialized = errors.New("auth: the administrator is initialized already")
	// ErrInvalidBootstrap reports an unknown, expired or consumed bootstrap
	// credential.
	ErrInvalidBootstrap = errors.New("auth: the bootstrap credential is invalid")
	// ErrInvalidPassword reports a wrong password.
	ErrInvalidPassword = errors.New("auth: the password is incorrect")
	// ErrInvalidSession reports an unknown, expired or revoked session.
	ErrInvalidSession = errors.New("auth: the session is invalid")
	// ErrPasswordPolicy reports a password outside of the allowed length.
	ErrPasswordPolicy = errors.New("auth: the password does not meet the policy")
)

// csrfContext separates the CSRF token of a session from other uses of the
// session token.
const csrfContext = "hmoob-dock/csrf/v1"

// Session is an authenticated session of the administrator.
type Session struct {
	// ID identifies the session in the database.
	ID string
	// Token is the plain session token. It only exists in the request that
	// carries it and in the cookie of that request.
	Token string
	// CSRF is the token that the browser has to send with a modifying
	// request.
	CSRF string
	// ExpiresAt is the time the session stops being valid.
	ExpiresAt time.Time
}

// Service manages the administrator, the bootstrap credential and the
// sessions of the control端.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

// New builds the service on top of the given database. The clock provides
// every timestamp and expiry; a nil clock falls back to the system clock.
func New(db *sql.DB, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{db: db, now: now}
}

// Initialized reports whether the administrator exists already.
func (s *Service) Initialized(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM administrator`).Scan(&count); err != nil {
		return false, fmt.Errorf("auth: count the administrators: %w", err)
	}
	return count > 0, nil
}

// SeedBootstrap stores the one time bootstrap credential. The credential is
// only stored as a digest and stays valid until the given time.
//
// A credential that is on record keeps the window it was given: a restart of
// the control端 must not extend the lifetime of a credential that was
// handed out already. A new credential is only stored while the table holds
// no record at all, so a deployment has at most one bootstrap credential.
//
// Seeding a credential that was consumed already does nothing, so a restart
// of the control端 cannot make a used credential valid again.
func (s *Service) SeedBootstrap(ctx context.Context, token string, expires time.Time) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("auth: the bootstrap credential is empty")
	}
	digest := digestOf(token)

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM bootstrap_tokens`).Scan(&count); err != nil {
		return fmt.Errorf("auth: count the bootstrap credentials: %w", err)
	}
	if count > 0 {
		return nil
	}

	id, err := randomID()
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO bootstrap_tokens (id, token_hash, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		id, digest, store.TimeToMillis(s.now()), store.TimeToMillis(expires)); err != nil {
		return fmt.Errorf("auth: store the bootstrap credential: %w", err)
	}
	return nil
}

// Initialize creates the administrator with the given password. The
// bootstrap credential is consumed by the call and cannot be used again.
func (s *Service) Initialize(ctx context.Context, token, password string) error {
	if err := ValidatePassword(password); err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return ErrInvalidBootstrap
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: start the initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	credential, known, err := s.findBootstrap(ctx, tx, token)
	if err != nil {
		return err
	}
	initialized, err := s.initializedIn(ctx, tx)
	if err != nil {
		return err
	}
	if credential == "" {
		if known {
			// The credential was consumed or it expired.
			return ErrInvalidBootstrap
		}
		// A credential that the deployment never issued cannot open a
		// second administrator.
		if initialized {
			return ErrAlreadyInitialized
		}
		return ErrInvalidBootstrap
	}
	if initialized {
		return ErrAlreadyInitialized
	}

	// The password is only hashed after the credential was accepted, so a
	// request with a wrong credential cannot make the control端 spend its
	// time on a hash.
	encoded, err := EncodePassword(password)
	if err != nil {
		return err
	}

	now := store.TimeToMillis(s.now())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO administrator (id, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		administratorID, encoded, now, now); err != nil {
		// A concurrent initialization may have won the race.
		if initialized, checkErr := s.initializedIn(ctx, tx); checkErr == nil && initialized {
			return ErrAlreadyInitialized
		}
		return fmt.Errorf("auth: store the administrator: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE bootstrap_tokens SET consumed_at = ? WHERE id = ?`, now, credential); err != nil {
		return fmt.Errorf("auth: consume the bootstrap credential: %w", err)
	}
	if err := s.recordAudit(ctx, tx, AuditActionInitialize, AuditResultSuccess); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: commit the initialization: %w", err)
	}
	return nil
}

// Login checks the password of the administrator and starts a session.
//
// Every attempt is recorded in the audit events: a failed one without the
// reason of the failure, so the record tells that an attempt happened
// without telling a credential to the reader of the table.
func (s *Service) Login(ctx context.Context, password string) (Session, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM administrator WHERE id = ?`, administratorID).Scan(&encoded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if auditErr := s.recordAudit(ctx, s.db, AuditActionLogin, AuditResultFailure); auditErr != nil {
			return Session{}, auditErr
		}
		return Session{}, ErrNotInitialized
	case err != nil:
		return Session{}, fmt.Errorf("auth: read the administrator: %w", err)
	}
	if err := VerifyPassword(encoded, password); err != nil {
		if auditErr := s.recordAudit(ctx, s.db, AuditActionLogin, AuditResultFailure); auditErr != nil {
			return Session{}, auditErr
		}
		return Session{}, err
	}
	return s.startSession(ctx)
}

// Authenticate returns the session that the given token identifies. An
// unknown, expired or revoked token is reported as ErrInvalidSession.
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrInvalidSession
	}

	var id, csrfDigest string
	var expiresAt int64
	var revokedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, csrf_hash, expires_at, revoked_at FROM sessions WHERE token_hash = ?`,
		digestOf(token)).Scan(&id, &csrfDigest, &expiresAt, &revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Session{}, ErrInvalidSession
	case err != nil:
		return Session{}, fmt.Errorf("auth: read the session: %w", err)
	}
	if revokedAt.Valid {
		return Session{}, ErrInvalidSession
	}
	expires := store.TimeFromMillis(expiresAt)
	if !expires.After(s.now()) {
		return Session{}, ErrInvalidSession
	}

	csrf := deriveCSRF(token)
	if subtle.ConstantTimeCompare([]byte(digestOf(csrf)), []byte(csrfDigest)) != 1 {
		return Session{}, ErrInvalidSession
	}
	return Session{ID: id, Token: token, CSRF: csrf, ExpiresAt: expires}, nil
}

// ChangePassword replaces the password of the administrator and revokes
// every session in the same transaction, so no session survives the change.
func (s *Service) ChangePassword(ctx context.Context, token, current, next string) error {
	if err := ValidatePassword(next); err != nil {
		return err
	}
	if _, err := s.Authenticate(ctx, token); err != nil {
		return err
	}

	var encoded string
	err := s.db.QueryRowContext(ctx,
		`SELECT password_hash FROM administrator WHERE id = ?`, administratorID).Scan(&encoded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotInitialized
	case err != nil:
		return fmt.Errorf("auth: read the administrator: %w", err)
	}
	if err := VerifyPassword(encoded, current); err != nil {
		return err
	}
	replacement, err := EncodePassword(next)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("auth: start the password change: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := store.TimeToMillis(s.now())
	if _, err := tx.ExecContext(ctx,
		`UPDATE administrator SET password_hash = ?, updated_at = ? WHERE id = ?`,
		replacement, now, administratorID); err != nil {
		return fmt.Errorf("auth: store the password: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE revoked_at IS NULL`, now); err != nil {
		return fmt.Errorf("auth: revoke the sessions: %w", err)
	}
	if err := s.recordAudit(ctx, tx, AuditActionPasswordChange, AuditResultSuccess); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("auth: commit the password change: %w", err)
	}
	return nil
}

// Logout revokes the session that the given token identifies.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return ErrInvalidSession
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`,
		store.TimeToMillis(s.now()), digestOf(token))
	if err != nil {
		return fmt.Errorf("auth: revoke the session: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: revoke the session: %w", err)
	}
	if affected == 0 {
		return ErrInvalidSession
	}
	return nil
}

// startSession creates a session with a fresh random token. The session and
// the audit event of the login are written in one transaction, so a
// successful login is never recorded without a session.
func (s *Service) startSession(ctx context.Context) (Session, error) {
	token, err := randomToken()
	if err != nil {
		return Session{}, err
	}
	id, err := randomID()
	if err != nil {
		return Session{}, err
	}
	csrf := deriveCSRF(token)
	now := s.now()
	expires := now.Add(SessionTTL)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("auth: start the session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (id, token_hash, csrf_hash, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		id, digestOf(token), digestOf(csrf),
		store.TimeToMillis(now), store.TimeToMillis(expires)); err != nil {
		return Session{}, fmt.Errorf("auth: store the session: %w", err)
	}
	if err := s.recordAudit(ctx, tx, AuditActionLogin, AuditResultSuccess); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("auth: commit the session: %w", err)
	}
	return Session{ID: id, Token: token, CSRF: csrf, ExpiresAt: expires}, nil
}

// findBootstrap returns the identifier of the bootstrap credential that
// matches the given token and is still usable. The digests are compared in
// constant time. The second return value reports whether a credential with
// this digest is on record at all, so a consumed or expired credential can
// be told apart from an unknown one.
func (s *Service) findBootstrap(ctx context.Context, tx *sql.Tx, token string) (string, bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, token_hash, expires_at, consumed_at FROM bootstrap_tokens`)
	if err != nil {
		return "", false, fmt.Errorf("auth: read the bootstrap credentials: %w", err)
	}
	defer rows.Close()

	digest := digestOf(token)
	now := s.now()
	matched := ""
	known := false
	for rows.Next() {
		var id, stored string
		var expiresAt int64
		var consumedAt sql.NullInt64
		if err := rows.Scan(&id, &stored, &expiresAt, &consumedAt); err != nil {
			return "", false, fmt.Errorf("auth: scan the bootstrap credential: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(digest), []byte(stored)) != 1 {
			continue
		}
		known = true
		if consumedAt.Valid {
			continue
		}
		if !store.TimeFromMillis(expiresAt).After(now) {
			continue
		}
		matched = id
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("auth: read the bootstrap credentials: %w", err)
	}
	return matched, known, nil
}

// initializedIn reports whether the administrator exists, using the given
// transaction.
func (s *Service) initializedIn(ctx context.Context, tx *sql.Tx) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM administrator`).Scan(&count); err != nil {
		return false, fmt.Errorf("auth: count the administrators: %w", err)
	}
	return count > 0, nil
}

// deriveCSRF derives the CSRF token of a session from its session token.
// The derivation is deterministic, so the token can be shown to the browser
// again, while the database only holds the digest of the result.
func deriveCSRF(token string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(csrfContext))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// digestOf returns the hex encoded SHA-256 digest of the given value.
func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// randomToken returns a random token that identifies a session.
func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("auth: generate a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// randomID returns a random identifier.
func randomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("auth: generate an identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
