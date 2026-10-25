package auth_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/store"
)

// clock is the time source of the service under test.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Unix(1800000000, 0)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

// newService opens a database in a temporary directory and returns the
// service on top of it.
func newService(t *testing.T) (*auth.Service, *sql.DB, *clock) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tick := newClock()
	return auth.New(db, tick.Now), db, tick
}

// newServiceWithAuditLimit opens a database in a temporary directory and
// returns a service that keeps at most limit audit events.
func newServiceWithAuditLimit(t *testing.T, limit int) (*auth.Service, *sql.DB, *clock) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tick := newClock()
	return auth.NewWithAuditLimit(db, tick.Now, limit), db, tick
}

// captureLog redirects the log of the standard logger into a buffer for the
// duration of the test, so the reports of the control端 do not pollute the
// output of the test run.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	buffer := &bytes.Buffer{}
	writer := log.Writer()
	log.SetOutput(buffer)
	t.Cleanup(func() { log.SetOutput(writer) })
	return buffer
}

// initializeAdmin seeds a bootstrap credential and initializes the
// administrator with it.
func initializeAdmin(t *testing.T, service *auth.Service, tick *clock) {
	t.Helper()

	ctx := context.Background()
	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	if err := service.Initialize(ctx, "deployment-secret", "a-strong-test-password"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func TestInitializeConsumesBootstrapOnce(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	now := time.Unix(1800000000, 0)
	svc := auth.New(db, func() time.Time { return now })
	ctx := context.Background()
	if err := svc.SeedBootstrap(ctx, "deployment-secret", now.Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	if err := svc.Initialize(ctx, "wrong-secret", "a-strong-test-password"); err == nil {
		t.Fatal("unguarded initialization")
	}
	if err := svc.Initialize(ctx, "deployment-secret", "a-strong-test-password"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := svc.Initialize(ctx, "deployment-secret", "replacement-password"); err == nil {
		t.Fatal("bootstrap reused")
	}
}

func TestInitializeRejectsAnExpiredBootstrapToken(t *testing.T) {
	service, _, tick := newService(t)
	ctx := context.Background()

	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	tick.Advance(16 * time.Minute)

	if err := service.Initialize(ctx, "deployment-secret", "a-strong-test-password"); !errors.Is(err, auth.ErrInvalidBootstrap) {
		t.Fatalf("Initialize error = %v, want %v", err, auth.ErrInvalidBootstrap)
	}
}

func TestInitializeKeepsTheBootstrapTokenWhenThePasswordIsRejected(t *testing.T) {
	service, _, tick := newService(t)
	ctx := context.Background()

	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	if err := service.Initialize(ctx, "deployment-secret", "too-short"); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("Initialize error = %v, want %v", err, auth.ErrPasswordPolicy)
	}
	if err := service.Initialize(ctx, "deployment-secret", "a-strong-test-password"); err != nil {
		t.Fatalf("Initialize after a rejected password: %v", err)
	}
}

func TestInitializeRefusesASecondAdministrator(t *testing.T) {
	service, _, tick := newService(t)
	ctx := context.Background()

	initializeAdmin(t, service, tick)

	// A fresh bootstrap credential must not open a second administrator.
	if err := service.SeedBootstrap(ctx, "another-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	if err := service.Initialize(ctx, "another-secret", "another-strong-password"); !errors.Is(err, auth.ErrAlreadyInitialized) {
		t.Fatalf("Initialize error = %v, want %v", err, auth.ErrAlreadyInitialized)
	}
}

func TestInitializeAcceptsOnlyOneConcurrentAdministrator(t *testing.T) {
	service, db, tick := newService(t)
	ctx := context.Background()

	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan error, 8)
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- service.Initialize(ctx, "deployment-secret", "a-strong-test-password")
		}()
	}
	wg.Wait()
	close(results)

	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful initializations = %d, want 1", succeeded)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM administrator`).Scan(&count); err != nil {
		t.Fatalf("count the administrators: %v", err)
	}
	if count != 1 {
		t.Fatalf("administrators = %d, want 1", count)
	}
}

func TestTheSchemaAllowsOnlyOneAdministrator(t *testing.T) {
	service, db, tick := newService(t)
	initializeAdmin(t, service, tick)

	if _, err := db.Exec(
		`INSERT INTO administrator (id, password_hash, created_at, updated_at) VALUES (2, 'hash', 0, 0)`,
	); err == nil {
		t.Fatal("the schema accepted a second administrator")
	}
}

func TestInitializeChecksThePasswordLength(t *testing.T) {
	service, _, tick := newService(t)
	ctx := context.Background()

	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	if err := service.Initialize(ctx, "deployment-secret", "short-passw"); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("Initialize error = %v, want %v", err, auth.ErrPasswordPolicy)
	}
	if err := service.Initialize(ctx, "deployment-secret", strings.Repeat("a", 129)); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("Initialize error = %v, want %v", err, auth.ErrPasswordPolicy)
	}
	if err := service.Initialize(ctx, "deployment-secret", strings.Repeat("a", 128)); err != nil {
		t.Fatalf("Initialize with a valid password: %v", err)
	}
}

func TestLoginRejectsAWrongPassword(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	if _, err := service.Login(context.Background(), "a-wrong-test-password"); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("Login error = %v, want %v", err, auth.ErrInvalidPassword)
	}
}

func TestLoginRequiresAnInitializedAdministrator(t *testing.T) {
	service, _, _ := newService(t)

	if _, err := service.Login(context.Background(), "a-strong-test-password"); !errors.Is(err, auth.ErrNotInitialized) {
		t.Fatalf("Login error = %v, want %v", err, auth.ErrNotInitialized)
	}
}

func TestLoginStartsASessionThatAuthenticates(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	session, err := service.Login(context.Background(), "a-strong-test-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if session.ID == "" || session.Token == "" || session.CSRF == "" {
		t.Fatalf("session = %+v, want an identifier, a token and a CSRF token", session)
	}
	if !session.ExpiresAt.After(tick.Now()) {
		t.Fatalf("session expiry = %v, want a time after %v", session.ExpiresAt, tick.Now())
	}

	authenticated, err := service.Authenticate(context.Background(), session.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authenticated.ID != session.ID || authenticated.CSRF != session.CSRF {
		t.Fatalf("authenticated session = %+v, want the session of the login", authenticated)
	}
}

func TestAuthenticateRejectsAnUnknownToken(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	if _, err := service.Authenticate(context.Background(), "not-a-session-token"); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("Authenticate error = %v, want %v", err, auth.ErrInvalidSession)
	}
}

func TestAuthenticateRejectsAnExpiredSession(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	session, err := service.Login(context.Background(), "a-strong-test-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	tick.Advance(auth.SessionTTL + time.Minute)

	if _, err := service.Authenticate(context.Background(), session.Token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("Authenticate error = %v, want %v", err, auth.ErrInvalidSession)
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	session, err := service.Login(context.Background(), "a-strong-test-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := service.Logout(context.Background(), session.Token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := service.Authenticate(context.Background(), session.Token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("Authenticate error = %v, want %v", err, auth.ErrInvalidSession)
	}
	if err := service.Logout(context.Background(), session.Token); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("second Logout error = %v, want %v", err, auth.ErrInvalidSession)
	}
}

func TestChangePasswordRevokesEverySession(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	first, err := service.Login(context.Background(), "a-strong-test-password")
	if err != nil {
		t.Fatalf("first Login: %v", err)
	}
	second, err := service.Login(context.Background(), "a-strong-test-password")
	if err != nil {
		t.Fatalf("second Login: %v", err)
	}

	if err := service.ChangePassword(context.Background(), first.Token, "a-strong-test-password", "a-new-strong-password"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	for name, token := range map[string]string{"first": first.Token, "second": second.Token} {
		if _, err := service.Authenticate(context.Background(), token); !errors.Is(err, auth.ErrInvalidSession) {
			t.Fatalf("Authenticate of the %s session error = %v, want %v", name, err, auth.ErrInvalidSession)
		}
	}
	if _, err := service.Login(context.Background(), "a-strong-test-password"); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("Login with the old password error = %v, want %v", err, auth.ErrInvalidPassword)
	}
	if _, err := service.Login(context.Background(), "a-new-strong-password"); err != nil {
		t.Fatalf("Login with the new password: %v", err)
	}
}

func TestChangePasswordRejectsAWrongCurrentPassword(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	session, err := service.Login(context.Background(), "a-strong-test-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := service.ChangePassword(context.Background(), session.Token, "a-wrong-test-password", "a-new-strong-password"); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("ChangePassword error = %v, want %v", err, auth.ErrInvalidPassword)
	}
	if _, err := service.Authenticate(context.Background(), session.Token); err != nil {
		t.Fatalf("Authenticate after a rejected password change: %v", err)
	}
}

func TestChangePasswordRejectsAWeakNewPassword(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	session, err := service.Login(context.Background(), "a-strong-test-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := service.ChangePassword(context.Background(), session.Token, "a-strong-test-password", "short-passw"); !errors.Is(err, auth.ErrPasswordPolicy) {
		t.Fatalf("ChangePassword error = %v, want %v", err, auth.ErrPasswordPolicy)
	}
}

func TestChangePasswordRequiresASession(t *testing.T) {
	service, _, tick := newService(t)
	initializeAdmin(t, service, tick)

	if err := service.ChangePassword(context.Background(), "not-a-session-token", "a-strong-test-password", "a-new-strong-password"); !errors.Is(err, auth.ErrInvalidSession) {
		t.Fatalf("ChangePassword error = %v, want %v", err, auth.ErrInvalidSession)
	}
}

func TestSeedBootstrapDoesNotReviveAConsumedCredential(t *testing.T) {
	service, db, tick := newService(t)
	ctx := context.Background()

	initializeAdmin(t, service, tick)

	// Seeding the consumed credential again must not make it usable.
	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	if err := service.Initialize(ctx, "deployment-secret", "another-strong-password"); !errors.Is(err, auth.ErrInvalidBootstrap) {
		t.Fatalf("Initialize error = %v, want %v", err, auth.ErrInvalidBootstrap)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM administrator`).Scan(&count); err != nil {
		t.Fatalf("count the administrators: %v", err)
	}
	if count != 1 {
		t.Fatalf("administrators = %d, want 1", count)
	}
	if _, err := service.Login(ctx, "a-strong-test-password"); err != nil {
		t.Fatalf("Login with the original password: %v", err)
	}
}

func TestSeedBootstrapRejectsAnEmptyCredential(t *testing.T) {
	service, _, _ := newService(t)

	if err := service.SeedBootstrap(context.Background(), "   ", time.Now().Add(15*time.Minute)); err == nil {
		t.Fatal("SeedBootstrap accepted an empty credential")
	}
}

func TestInitializedReportsTheStateOfTheAdministrator(t *testing.T) {
	service, _, tick := newService(t)
	ctx := context.Background()

	initialized, err := service.Initialized(ctx)
	if err != nil {
		t.Fatalf("Initialized: %v", err)
	}
	if initialized {
		t.Fatal("the administrator is initialized before the bootstrap credential was used")
	}

	initializeAdmin(t, service, tick)

	initialized, err = service.Initialized(ctx)
	if err != nil {
		t.Fatalf("Initialized: %v", err)
	}
	if !initialized {
		t.Fatal("the administrator is not initialized after the bootstrap credential was used")
	}
}

func TestSeedBootstrapKeepsTheWindowOfTheCredential(t *testing.T) {
	service, db, tick := newService(t)
	ctx := context.Background()

	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	var first int64
	if err := db.QueryRow(`SELECT expires_at FROM bootstrap_tokens`).Scan(&first); err != nil {
		t.Fatalf("read the expiry of the credential: %v", err)
	}

	// A restart of the control端 seeds the credential again, but the
	// window of the credential that is on record stays the same.
	tick.Advance(time.Minute)
	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("second SeedBootstrap: %v", err)
	}

	var second int64
	if err := db.QueryRow(`SELECT expires_at FROM bootstrap_tokens`).Scan(&second); err != nil {
		t.Fatalf("read the expiry of the credential: %v", err)
	}
	if second != first {
		t.Fatalf("the expiry of the credential = %d, want %d", second, first)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM bootstrap_tokens`).Scan(&count); err != nil {
		t.Fatalf("count the bootstrap credentials: %v", err)
	}
	if count != 1 {
		t.Fatalf("bootstrap credentials = %d, want 1", count)
	}
}

func TestTheAuthenticationWritesAuditEvents(t *testing.T) {
	service, db, tick := newService(t)
	ctx := context.Background()

	initializeAdmin(t, service, tick)

	if _, err := service.Login(ctx, "a-wrong-test-password"); !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("Login error = %v, want %v", err, auth.ErrInvalidPassword)
	}
	session, err := service.Login(ctx, "a-strong-test-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := service.ChangePassword(ctx, session.Token, "a-strong-test-password", "a-new-strong-password"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	type event struct {
		id         string
		occurredAt int64
		action     string
		targetID   string
		result     string
	}
	rows, err := db.Query(`SELECT id, occurred_at, action, target_id, result FROM audit_events`)
	if err != nil {
		t.Fatalf("read the audit events: %v", err)
	}
	var events []event
	for rows.Next() {
		var entry event
		if err := rows.Scan(&entry.id, &entry.occurredAt, &entry.action, &entry.targetID, &entry.result); err != nil {
			rows.Close()
			t.Fatalf("scan the audit event: %v", err)
		}
		events = append(events, entry)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("read the audit events: %v", err)
	}
	rows.Close()

	want := map[string]int{
		"initialize/success":      1,
		"login/failure":           1,
		"login/success":           1,
		"password_change/success": 1,
	}
	if len(events) != len(want) {
		t.Fatalf("audit events = %d, want %d", len(events), len(want))
	}
	for _, entry := range events {
		if entry.id == "" {
			t.Fatal("an audit event has no identifier")
		}
		if entry.occurredAt != store.TimeToMillis(tick.Now()) {
			t.Fatalf("the audit event happened at %d, want %d", entry.occurredAt, store.TimeToMillis(tick.Now()))
		}
		if entry.targetID != "admin" {
			t.Fatalf("the target of the audit event = %q, want admin", entry.targetID)
		}
		key := entry.action + "/" + entry.result
		if want[key] == 0 {
			t.Fatalf("the audit event %q is not expected", key)
		}
		want[key]--
	}
	for key, count := range want {
		if count != 0 {
			t.Fatalf("the audit event %q is missing", key)
		}
	}

	// The events hold no password, no token and no digest.
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("encode the audit events: %v", err)
	}
	for _, secret := range []string{
		"a-strong-test-password",
		"a-new-strong-password",
		"a-wrong-test-password",
		session.Token,
		session.CSRF,
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the audit events hold %q: %s", secret, encoded)
		}
	}
}

func TestTheStoredCredentialsAreIrreversible(t *testing.T) {
	service, db, tick := newService(t)
	initializeAdmin(t, service, tick)

	session, err := service.Login(context.Background(), "a-strong-test-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	var encoded string
	if err := db.QueryRow(`SELECT password_hash FROM administrator WHERE id = 1`).Scan(&encoded); err != nil {
		t.Fatalf("read the administrator: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("password hash = %q, want an argon2id hash", encoded)
	}
	if strings.Contains(encoded, "a-strong-test-password") {
		t.Fatal("the password hash contains the plain password")
	}

	var tokenHash, csrfHash string
	if err := db.QueryRow(`SELECT token_hash, csrf_hash FROM sessions WHERE id = ?`, session.ID).Scan(&tokenHash, &csrfHash); err != nil {
		t.Fatalf("read the session: %v", err)
	}
	if strings.Contains(tokenHash, session.Token) {
		t.Fatal("the session holds the plain session token")
	}
	if strings.Contains(csrfHash, session.CSRF) {
		t.Fatal("the session holds the plain CSRF token")
	}
	if tokenHash == session.Token || csrfHash == session.CSRF {
		t.Fatal("the session holds a credential instead of its digest")
	}
}

func TestSeedBootstrapStartsTheWindowAgainAfterTheCredentialExpired(t *testing.T) {
	service, db, tick := newService(t)
	ctx := context.Background()

	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}

	// The deployment did not finish its initialization in time, so the
	// credential on record expired.
	tick.Advance(16 * time.Minute)

	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("second SeedBootstrap: %v", err)
	}

	var expiresAt int64
	if err := db.QueryRow(`SELECT expires_at FROM bootstrap_tokens`).Scan(&expiresAt); err != nil {
		t.Fatalf("read the expiry of the credential: %v", err)
	}
	if want := store.TimeToMillis(tick.Now().Add(15 * time.Minute)); expiresAt != want {
		t.Fatalf("the expiry of the credential = %d, want %d", expiresAt, want)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM bootstrap_tokens`).Scan(&count); err != nil {
		t.Fatalf("count the bootstrap credentials: %v", err)
	}
	if count != 1 {
		t.Fatalf("bootstrap credentials = %d, want 1", count)
	}

	// The credential of the deployment opens the initialization again.
	if err := service.Initialize(ctx, "deployment-secret", "a-strong-test-password"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func TestSeedBootstrapReplacesAnExpiredCredentialWithANewOne(t *testing.T) {
	service, db, tick := newService(t)
	ctx := context.Background()

	if err := service.SeedBootstrap(ctx, "the-old-credential", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	tick.Advance(16 * time.Minute)

	// The deployment changed the content of its bootstrap file.
	if err := service.SeedBootstrap(ctx, "the-new-credential", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("second SeedBootstrap: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM bootstrap_tokens`).Scan(&count); err != nil {
		t.Fatalf("count the bootstrap credentials: %v", err)
	}
	if count != 1 {
		t.Fatalf("bootstrap credentials = %d, want 1", count)
	}

	// The old credential does not open the initialization any more.
	if err := service.Initialize(ctx, "the-old-credential", "another-strong-password"); !errors.Is(err, auth.ErrInvalidBootstrap) {
		t.Fatalf("Initialize error = %v, want %v", err, auth.ErrInvalidBootstrap)
	}

	// The new one does.
	if err := service.Initialize(ctx, "the-new-credential", "a-strong-test-password"); err != nil {
		t.Fatalf("Initialize with the new credential: %v", err)
	}
}

func TestSeedBootstrapKeepsAConsumedCredential(t *testing.T) {
	service, db, tick := newService(t)
	ctx := context.Background()

	initializeAdmin(t, service, tick)

	var expiresAt, consumedAt int64
	if err := db.QueryRow(`SELECT expires_at, consumed_at FROM bootstrap_tokens`).Scan(&expiresAt, &consumedAt); err != nil {
		t.Fatalf("read the credential: %v", err)
	}

	// The credential was consumed, so it is not replaced.
	tick.Advance(time.Minute)
	if err := service.SeedBootstrap(ctx, "another-credential", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}

	var keptExpiresAt, keptConsumedAt int64
	if err := db.QueryRow(`SELECT expires_at, consumed_at FROM bootstrap_tokens`).Scan(&keptExpiresAt, &keptConsumedAt); err != nil {
		t.Fatalf("read the credential: %v", err)
	}
	if keptExpiresAt != expiresAt {
		t.Fatalf("the expiry of the credential = %d, want %d", keptExpiresAt, expiresAt)
	}
	if keptConsumedAt != consumedAt {
		t.Fatalf("the consumption of the credential = %d, want %d", keptConsumedAt, consumedAt)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM bootstrap_tokens`).Scan(&count); err != nil {
		t.Fatalf("count the bootstrap credentials: %v", err)
	}
	if count != 1 {
		t.Fatalf("bootstrap credentials = %d, want 1", count)
	}
}

func TestTheAuditTableIsBounded(t *testing.T) {
	service, db, _ := newServiceWithAuditLimit(t, 3)
	ctx := context.Background()
	buffer := captureLog(t)

	// Every attempt before the initialization writes an audit event.
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := service.Login(ctx, "a-strong-test-password"); !errors.Is(err, auth.ErrNotInitialized) {
			t.Fatalf("Login error = %v, want %v", err, auth.ErrNotInitialized)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatalf("count the audit events: %v", err)
	}
	if count != 3 {
		t.Fatalf("audit events = %d, want 3", count)
	}

	// The table is full now. The attempt is answered with the result of the
	// authentication and not with an error of the audit log.
	if _, err := service.Login(ctx, "a-strong-test-password"); !errors.Is(err, auth.ErrNotInitialized) {
		t.Fatalf("Login error = %v, want %v", err, auth.ErrNotInitialized)
	}
	if err := db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatalf("count the audit events: %v", err)
	}
	if count != 3 {
		t.Fatalf("audit events = %d, want 3", count)
	}

	// The problem is reported, so it does not pass unnoticed.
	if !strings.Contains(buffer.String(), "audit table is full") {
		t.Fatalf("the log = %q, want a report about the full audit table", buffer.String())
	}
}

func TestTheLoginWorksWhenTheAuditTableIsFull(t *testing.T) {
	service, db, tick := newServiceWithAuditLimit(t, 1)
	ctx := context.Background()
	captureLog(t)

	// The initialization writes the first audit event, so the table is full
	// from now on.
	initializeAdmin(t, service, tick)

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatalf("count the audit events: %v", err)
	}
	if count != 1 {
		t.Fatalf("audit events = %d, want 1", count)
	}

	// The login opens a session, although its audit event cannot be
	// stored.
	session, err := service.Login(ctx, "a-strong-test-password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if session.Token == "" {
		t.Fatal("the session has no token")
	}
	if _, err := service.Authenticate(ctx, session.Token); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

func TestTheInitializationWorksWhenTheAuditTableIsFull(t *testing.T) {
	service, db, tick := newServiceWithAuditLimit(t, 1)
	ctx := context.Background()
	captureLog(t)

	// The table of the audit events is full before the initialization.
	if _, err := db.Exec(
		`INSERT INTO audit_events (id, occurred_at, action, target_id, result)
		 VALUES ('fill', 0, 'fill', 'admin', 'success')`,
	); err != nil {
		t.Fatalf("fill the audit events: %v", err)
	}

	// The initialization still creates the administrator and the login
	// still opens a session.
	if err := service.SeedBootstrap(ctx, "deployment-secret", tick.Now().Add(15*time.Minute)); err != nil {
		t.Fatalf("SeedBootstrap: %v", err)
	}
	if err := service.Initialize(ctx, "deployment-secret", "a-strong-test-password"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := service.Login(ctx, "a-strong-test-password"); err != nil {
		t.Fatalf("Login: %v", err)
	}
}
