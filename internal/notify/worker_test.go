package notify_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/notify"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// fakeClock is a clock the test drives.
type fakeClock struct {
	now time.Time
}

// read returns the current time of the clock.
func (c *fakeClock) read() time.Time { return c.now }

// advance moves the clock forward.
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// newWorker opens a worker on a fresh database with one target.
func newWorker(t *testing.T, transport *recordingTransport) (*notify.Worker, *sql.DB, *fakeClock) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	box, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	targets := notify.NewStore(db, box)
	if err := targets.Save(context.Background(), notify.Target{
		ID:      notify.DefaultTargetID,
		URL:     "https://hooks.example.com/notify",
		Enabled: true,
	}); err != nil {
		t.Fatalf("save the target: %v", err)
	}

	clock := &fakeClock{now: time.Unix(1800000000, 0).UTC()}
	worker := notify.NewWorker(db, box, clock.read)
	if transport != nil {
		worker = worker.UseClient(&http.Client{Transport: transport})
	}
	return worker, db, clock
}

// testKey returns the master key of the tests.
func testKey() []byte {
	key := make([]byte, secret.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// recordAlert opens an alert, so the outbox holds one notification.
func recordAlert(t *testing.T, db *sql.DB, at time.Time) {
	t.Helper()

	engine := alerts.New(db)
	if err := engine.PutRule(context.Background(), alerts.Rule{
		ID:        "r1",
		Kind:      alerts.KindResource,
		Metric:    "cpu_percent",
		Threshold: 80,
		Enabled:   true,
	}); err != nil {
		t.Fatalf("write the rule: %v", err)
	}
	event, err := engine.Observe(context.Background(), "r1", alerts.Observation{
		ObjectID: "c1",
		At:       at,
		Value:    90,
		Valid:    true,
	})
	if err != nil {
		t.Fatalf("open the alert: %v", err)
	}
	if event == nil {
		t.Fatal("the alert did not open")
	}
}

// outboxState is the state of one notification of the outbox.
type outboxState struct {
	attempts      int
	deliveryState string
	nextAttemptAt int64
	deliveredAt   sql.NullInt64
	lastError     string
}

// readOutboxState reads the state of the notification of the outbox.
//
// The test database holds one notification, so the first row is the one
// of the test.
func readOutboxState(t *testing.T, db *sql.DB) outboxState {
	t.Helper()

	var state outboxState
	err := db.QueryRow(`
		SELECT attempts, delivery_state, next_attempt_at, delivered_at, last_error
		  FROM notification_outbox ORDER BY id LIMIT 1`).
		Scan(&state.attempts, &state.deliveryState, &state.nextAttemptAt, &state.deliveredAt, &state.lastError)
	if err != nil {
		t.Fatalf("read the notification: %v", err)
	}
	return state
}

// TestTheWorkerSendsTheWaitingNotification proves that the worker sends
// the notification that waits for its delivery and records the result.
func TestTheWorkerSendsTheWaitingNotification(t *testing.T) {
	transport := &recordingTransport{}
	worker, db, _ := newWorker(t, transport)

	at := time.Unix(1800000000, 0).UTC()
	recordAlert(t, db, at)

	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the worker: %v", err)
	}

	requests := transport.received()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	state := readOutboxState(t, db)
	if state.deliveryState != "delivered" {
		t.Fatalf("the delivery state = %q, want delivered", state.deliveryState)
	}
	if !state.deliveredAt.Valid {
		t.Fatal("the delivery was not recorded")
	}
	if state.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", state.attempts)
	}
}

// TestTheWorkerWaitsForTheNextAttempt proves that a notification that
// waits for its retry is not sent before its time.
func TestTheWorkerWaitsForTheNextAttempt(t *testing.T) {
	transport := &recordingTransport{}
	worker, db, _ := newWorker(t, transport)

	at := time.Unix(1800000000, 0).UTC()
	recordAlert(t, db, at)
	if _, err := db.Exec(`UPDATE notification_outbox SET next_attempt_at = ?`, at.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("move the notification: %v", err)
	}

	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the worker: %v", err)
	}
	if requests := transport.received(); len(requests) != 0 {
		t.Fatalf("requests = %d, want none", len(requests))
	}
}

// TestTheWorkerRetriesWithTheScheduledDelays proves that a failed
// notification waits for the scheduled delays and that it leaves the
// queue after the last attempt.
//
// The delays are 30 seconds, 2 minutes and 10 minutes, so the fourth
// failure is the end of the notification.
func TestTheWorkerRetriesWithTheScheduledDelays(t *testing.T) {
	transport := &recordingTransport{status: http.StatusInternalServerError}
	worker, db, clock := newWorker(t, transport)

	at := time.Unix(1800000000, 0).UTC()
	recordAlert(t, db, at)

	for attempt := 1; attempt <= notify.MaxAttempts; attempt++ {
		err := worker.RunOnce(context.Background())
		if err == nil {
			t.Fatalf("the attempt %d did not report the failure", attempt)
		}
		state := readOutboxState(t, db)
		if state.attempts != attempt {
			t.Fatalf("attempts = %d, want %d", state.attempts, attempt)
		}
		if state.lastError == "" {
			t.Fatalf("the attempt %d did not record the failure", attempt)
		}
		if attempt == notify.MaxAttempts {
			if state.deliveryState != "failed" {
				t.Fatalf("the delivery state = %q, want failed", state.deliveryState)
			}
			break
		}
		if state.deliveryState != "pending" {
			t.Fatalf("the delivery state = %q, want pending", state.deliveryState)
		}
		want := clock.read().Add(notify.RetryDelay(attempt)).UnixMilli()
		if state.nextAttemptAt != want {
			t.Fatalf("the next attempt = %d, want %d", state.nextAttemptAt, want)
		}
		// The clock moves to the time of the next attempt.
		clock.advance(notify.RetryDelay(attempt))
	}

	// The notification left the queue, so no further attempt is made.
	clock.advance(time.Hour)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the worker: %v", err)
	}
	state := readOutboxState(t, db)
	if state.attempts != notify.MaxAttempts {
		t.Fatalf("attempts = %d, want %d", state.attempts, notify.MaxAttempts)
	}
}

// TestTheWorkerRedeliversAfterAnExpiredLease proves that a notification
// whose worker died is sent again.
//
// The result of a delivery that never reached the database is unknown, so
// the notification is sent again with the same identifier and the
// receiver can deduplicate it.
func TestTheWorkerRedeliversAfterAnExpiredLease(t *testing.T) {
	transport := &recordingTransport{}
	worker, db, clock := newWorker(t, transport)

	at := time.Unix(1800000000, 0).UTC()
	recordAlert(t, db, at)

	outbox := alerts.NewOutbox(db)
	outbox.UseClock(clock.read)
	pending, err := outbox.PendingNotifications(context.Background(), 10)
	if err != nil {
		t.Fatalf("read the notifications: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("notifications = %d, want 1", len(pending))
	}
	claimed, attempt, err := outbox.ClaimNotification(context.Background(), pending[0].ID, clock.read().Add(notify.LeaseDuration))
	if err != nil {
		t.Fatalf("claim the notification: %v", err)
	}
	if !claimed || attempt != 1 {
		t.Fatalf("the claim = %v, the attempt = %d, want true and 1", claimed, attempt)
	}

	// The worker died before it recorded the result. The lease expires, so
	// the notification waits for its delivery again.
	clock.advance(notify.LeaseDuration + time.Second)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the worker: %v", err)
	}

	requests := transport.received()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if !strings.Contains(requests[0].Body, pending[0].ID) {
		t.Fatalf("the notification %s was not sent again", pending[0].ID)
	}
	state := readOutboxState(t, db)
	if state.deliveryState != "delivered" {
		t.Fatalf("the delivery state = %q, want delivered", state.deliveryState)
	}
	if state.attempts != 2 {
		t.Fatalf("attempts = %d, want 2", state.attempts)
	}
}

// TestTheWorkerSendsNothingWithoutATarget proves that a control端 without
// a target sends nothing and fails nothing.
func TestTheWorkerSendsNothingWithoutATarget(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	box, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	transport := &recordingTransport{}
	worker := notify.NewWorker(db, box, time.Now).UseClient(&http.Client{Transport: transport})

	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the worker: %v", err)
	}
	if requests := transport.received(); len(requests) != 0 {
		t.Fatalf("requests = %d, want none", len(requests))
	}
}

// TestTheWorkerSendsNothingToADisabledTarget proves that a target that
// the administrator disabled is not used.
func TestTheWorkerSendsNothingToADisabledTarget(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	box, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	targets := notify.NewStore(db, box)
	if err := targets.Save(context.Background(), notify.Target{
		ID:      notify.DefaultTargetID,
		URL:     "https://hooks.example.com/notify",
		Enabled: false,
	}); err != nil {
		t.Fatalf("save the target: %v", err)
	}
	transport := &recordingTransport{}
	worker := notify.NewWorker(db, box, time.Now).UseClient(&http.Client{Transport: transport})

	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the worker: %v", err)
	}
	if requests := transport.received(); len(requests) != 0 {
		t.Fatalf("requests = %d, want none", len(requests))
	}
}

// TestTheStoreKeepsTheAddressSecret proves that the address of a target is
// stored encrypted.
//
// The address of a service of a chat holds a token, so the database holds
// no readable address.
func TestTheStoreKeepsTheAddressSecret(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	box, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	targets := notify.NewStore(db, box)

	const address = "https://hooks.example.com/services/T00/B00/secret"
	if err := targets.Save(context.Background(), notify.Target{
		ID:      notify.DefaultTargetID,
		URL:     address,
		Enabled: true,
	}); err != nil {
		t.Fatalf("save the target: %v", err)
	}

	var ciphertext []byte
	if err := db.QueryRow(`SELECT url_ciphertext FROM webhook_targets WHERE id = ?`,
		notify.DefaultTargetID).Scan(&ciphertext); err != nil {
		t.Fatalf("read the target: %v", err)
	}
	if len(ciphertext) == 0 {
		t.Fatal("the target holds no address")
	}
	for _, secretValue := range []string{"hooks.example.com", "T00", "secret"} {
		if strings.Contains(string(ciphertext), secretValue) {
			t.Fatalf("the stored address carries %q", secretValue)
		}
	}

	target, found, err := targets.Target(context.Background(), notify.DefaultTargetID)
	if err != nil {
		t.Fatalf("read the target: %v", err)
	}
	if !found {
		t.Fatal("the target was not found")
	}
	if target.URL != address {
		t.Fatalf("the address = %q, want %q", target.URL, address)
	}
	if target.MaskedURL != "https://hooks.example.com/***" {
		t.Fatalf("the mask = %q", target.MaskedURL)
	}
}

// TestTheStoreReportsAMissingTarget proves that a target that does not
// exist is reported instead of guessed.
func TestTheStoreReportsAMissingTarget(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	box, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	targets := notify.NewStore(db, box)

	_, found, err := targets.Target(context.Background(), notify.DefaultTargetID)
	if err != nil {
		t.Fatalf("read the target: %v", err)
	}
	if found {
		t.Fatal("a target that does not exist was found")
	}
}

// TestTheWorkerReportsAnUnreachableTarget proves that the worker does not
// swallow the failure of a target it cannot use.
func TestTheWorkerReportsAnUnreachableTarget(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	box, err := secret.New(testKey())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	targets := notify.NewStore(db, box)
	if err := targets.Save(context.Background(), notify.Target{
		ID:      notify.DefaultTargetID,
		URL:     "ftp://hooks.example.com/notify",
		Enabled: true,
	}); err != nil {
		t.Fatalf("save the target: %v", err)
	}
	worker := notify.NewWorker(db, box, time.Now)

	err = worker.RunOnce(context.Background())
	if err == nil {
		t.Fatal("the unusable target was not reported")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("the error is not about the target: %v", err)
	}
}
