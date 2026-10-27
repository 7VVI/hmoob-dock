package alerts_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/store"
)

// testClock is a clock the test drives.
type testClock struct {
	now time.Time
}

// read returns the current time of the clock.
func (c *testClock) read() time.Time { return c.now }

// advance moves the clock forward.
func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// newOutbox opens a database with an outbox of the notifications.
func newOutbox(t *testing.T) (*alerts.Outbox, *sql.DB, *testClock) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	clock := &testClock{now: time.Unix(1800000000, 0).UTC()}
	outbox := alerts.NewOutbox(db)
	outbox.UseClock(clock.read)
	return outbox, db, clock
}

// insertNotification writes one notification of the outbox.
func insertNotification(t *testing.T, db *sql.DB, id string, nextAttemptAt time.Time) {
	t.Helper()

	if _, err := db.Exec(`
		INSERT INTO notification_outbox
		    (id, event_id, transition_seq, state, occurred_at, summary, environment, object, next_attempt_at)
		VALUES (?, ?, 1, 'active', ?, 'summary', 'environment', 'object', ?)`,
		id, "event-"+id, time.Unix(1800000000, 0).UnixMilli(), nextAttemptAt.UnixMilli()); err != nil {
		t.Fatalf("write the notification: %v", err)
	}
}

// TestPendingNotificationsReturnsTheOldestFirst proves that the sender
// reads the notifications in the order of their next attempt.
//
// The order matters: a notification that waits the longest is sent
// first, so a burst of notifications does not starve the old ones.
func TestPendingNotificationsReturnsTheOldestFirst(t *testing.T) {
	outbox, db, _ := newOutbox(t)
	ctx := context.Background()

	base := time.Unix(1800000000, 0).UTC()
	insertNotification(t, db, "c", base.Add(-1*time.Minute))
	insertNotification(t, db, "a", base.Add(-3*time.Minute))
	insertNotification(t, db, "b", base.Add(-2*time.Minute))

	pending, err := outbox.PendingNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("read the notifications: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("notifications = %d, want 3", len(pending))
	}
	for index, want := range []string{"a", "b", "c"} {
		if pending[index].ID != want {
			t.Fatalf("the notification %d = %q, want %q", index, pending[index].ID, want)
		}
	}
}

// TestPendingNotificationsIgnoresTheDeliveredOnes proves that a
// notification that reached its target does not wait any more.
func TestPendingNotificationsIgnoresTheDeliveredOnes(t *testing.T) {
	outbox, db, _ := newOutbox(t)
	ctx := context.Background()

	base := time.Unix(1800000000, 0).UTC()
	insertNotification(t, db, "a", base)
	if _, err := db.Exec(`UPDATE notification_outbox SET delivery_state = 'delivered', delivered_at = ? WHERE id = 'a'`,
		base.UnixMilli()); err != nil {
		t.Fatalf("deliver the notification: %v", err)
	}

	pending, err := outbox.PendingNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("read the notifications: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("notifications = %d, want none", len(pending))
	}
}

// TestTheClaimCountsTheAttemptAndHoldsTheLease proves that the claim of a
// notification is durable.
//
// The claim counts the attempt and it holds the notification until the
// lease ends, so a worker that died does not block it forever and no
// other worker sends it at the same time.
func TestTheClaimCountsTheAttemptAndHoldsTheLease(t *testing.T) {
	outbox, db, clock := newOutbox(t)
	ctx := context.Background()

	base := time.Unix(1800000000, 0).UTC()
	insertNotification(t, db, "a", base)

	claimed, attempt, err := outbox.ClaimNotification(ctx, "a", base.Add(time.Minute))
	if err != nil {
		t.Fatalf("claim the notification: %v", err)
	}
	if !claimed {
		t.Fatal("the notification was not claimed")
	}
	if attempt != 1 {
		t.Fatalf("the attempt = %d, want 1", attempt)
	}

	// The notification is held by the lease, so it does not wait for a
	// delivery while the lease lasts.
	pending, err := outbox.PendingNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("read the notifications: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("notifications = %d, want none", len(pending))
	}

	// The lease expired, so the notification waits for its delivery again.
	clock.advance(2 * time.Minute)
	claimed, attempt, err = outbox.ClaimNotification(ctx, "a", clock.read().Add(time.Minute))
	if err != nil {
		t.Fatalf("claim the notification: %v", err)
	}
	if !claimed {
		t.Fatal("the notification was not claimed after the lease")
	}
	if attempt != 2 {
		t.Fatalf("the attempt = %d, want 2", attempt)
	}
}

// TestTheClaimFailsOnAMissingNotification proves that a notification that
// does not exist cannot be claimed.
func TestTheClaimFailsOnAMissingNotification(t *testing.T) {
	outbox, _, _ := newOutbox(t)
	base := time.Unix(1800000000, 0).UTC()

	claimed, _, err := outbox.ClaimNotification(context.Background(), "missing", base.Add(time.Minute))
	if err != nil {
		t.Fatalf("claim the notification: %v", err)
	}
	if claimed {
		t.Fatal("a notification that does not exist was claimed")
	}
}

// TestTheDeliveryLeavesTheQueue proves that a delivered notification does
// not wait any more and that a failed one waits for its next attempt.
func TestTheDeliveryLeavesTheQueue(t *testing.T) {
	outbox, db, _ := newOutbox(t)
	ctx := context.Background()

	base := time.Unix(1800000000, 0).UTC()
	insertNotification(t, db, "a", base)
	insertNotification(t, db, "b", base)

	if _, _, err := outbox.ClaimNotification(ctx, "a", base.Add(time.Minute)); err != nil {
		t.Fatalf("claim the notification: %v", err)
	}
	if err := outbox.CompleteNotification(ctx, "a", alerts.Delivery{Delivered: true}); err != nil {
		t.Fatalf("record the delivery: %v", err)
	}
	if _, _, err := outbox.ClaimNotification(ctx, "b", base.Add(time.Minute)); err != nil {
		t.Fatalf("claim the notification: %v", err)
	}
	if err := outbox.CompleteNotification(ctx, "b", alerts.Delivery{
		Failure:       "the target answered with the status 500",
		NextAttemptAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatalf("record the failure: %v", err)
	}

	var state string
	if err := db.QueryRow(`SELECT delivery_state FROM notification_outbox WHERE id = 'a'`).Scan(&state); err != nil {
		t.Fatalf("read the notification: %v", err)
	}
	if state != "delivered" {
		t.Fatalf("the delivery state = %q, want delivered", state)
	}

	pending, err := outbox.PendingNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("read the notifications: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("notifications = %d, want none", len(pending))
	}

	// The failed notification waits for its next attempt.
	if _, err := db.Exec(`UPDATE notification_outbox SET next_attempt_at = ? WHERE id = 'b'`, base.UnixMilli()); err != nil {
		t.Fatalf("move the notification: %v", err)
	}
	pending, err = outbox.PendingNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("read the notifications: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "b" {
		t.Fatalf("notifications = %v, want the failed one", pending)
	}
}

// TestPendingNotificationsIgnoresTheExhaustedOnes proves that a
// notification that used all of its tries does not wait in the queue.
//
// Such a notification cannot be claimed again, so a queue that returns it
// fills the batch with notifications that no worker can deliver: the
// notifications that follow them wait forever.
func TestPendingNotificationsIgnoresTheExhaustedOnes(t *testing.T) {
	outbox, db, _ := newOutbox(t)
	ctx := context.Background()

	base := time.Unix(1800000000, 0).UTC()
	insertNotification(t, db, "a", base)
	if _, err := db.Exec(`UPDATE notification_outbox SET attempts = ? WHERE id = 'a'`,
		alerts.MaxNotificationAttempts); err != nil {
		t.Fatalf("exhaust the notification: %v", err)
	}

	pending, err := outbox.PendingNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("read the notifications: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("notifications = %d, want none", len(pending))
	}
}

// TestTheSweepEndsTheExhaustedNotifications proves that the notifications
// that used all of their tries leave the queue.
//
// A worker that died after its last claim leaves a notification behind
// that no worker can claim again. The sweep ends it, so it does not
// starve the notifications that follow it. The sweep is idempotent: a
// notification that already left the queue is not touched again.
func TestTheSweepEndsTheExhaustedNotifications(t *testing.T) {
	outbox, db, _ := newOutbox(t)
	ctx := context.Background()

	base := time.Unix(1800000000, 0).UTC()
	insertNotification(t, db, "a", base)
	insertNotification(t, db, "b", base)
	insertNotification(t, db, "c", base)
	if _, err := db.Exec(`UPDATE notification_outbox SET attempts = ?, lease_until = 0 WHERE id = 'a'`,
		alerts.MaxNotificationAttempts); err != nil {
		t.Fatalf("exhaust the notification: %v", err)
	}
	// The notification of a worker that is still at work keeps its lease,
	// so the sweep does not end it.
	if _, err := db.Exec(`UPDATE notification_outbox SET attempts = ?, lease_until = ? WHERE id = 'b'`,
		alerts.MaxNotificationAttempts, base.Add(time.Minute).UnixMilli()); err != nil {
		t.Fatalf("exhaust the notification: %v", err)
	}

	ended, err := outbox.FailTheExhaustedNotifications(ctx)
	if err != nil {
		t.Fatalf("sweep the notifications: %v", err)
	}
	if ended != 1 {
		t.Fatalf("the ended notifications = %d, want 1", ended)
	}

	state, lastError := readDeliveryState(t, db, "a")
	if state != "failed" {
		t.Fatalf("the delivery state = %q, want failed", state)
	}
	if lastError == "" {
		t.Fatal("the ended notification carries no reason")
	}

	// The sweep is idempotent: the notification that already left the
	// queue is not ended a second time.
	ended, err = outbox.FailTheExhaustedNotifications(ctx)
	if err != nil {
		t.Fatalf("sweep the notifications: %v", err)
	}
	if ended != 0 {
		t.Fatalf("the ended notifications = %d, want none", ended)
	}
	state, _ = readDeliveryState(t, db, "a")
	if state != "failed" {
		t.Fatalf("the delivery state = %q, want failed", state)
	}

	// The notification that a live worker holds waits for its result.
	state, _ = readDeliveryState(t, db, "b")
	if state != "pending" {
		t.Fatalf("the delivery state = %q, want pending", state)
	}
	// The notification that still has tries waits for its delivery.
	state, _ = readDeliveryState(t, db, "c")
	if state != "pending" {
		t.Fatalf("the delivery state = %q, want pending", state)
	}
}

// readDeliveryState reads the state of one notification of the outbox.
func readDeliveryState(t *testing.T, db *sql.DB, id string) (string, string) {
	t.Helper()

	var state, lastError string
	if err := db.QueryRow(
		`SELECT delivery_state, last_error FROM notification_outbox WHERE id = ?`, id).
		Scan(&state, &lastError); err != nil {
		t.Fatalf("read the notification: %v", err)
	}
	return state, lastError
}

// TestTheUnfinishedNotificationsAreCounted proves that the queue of the
// notifications is counted, so the bound of the queue can be kept.
func TestTheUnfinishedNotificationsAreCounted(t *testing.T) {
	outbox, db, _ := newOutbox(t)
	ctx := context.Background()

	base := time.Unix(1800000000, 0).UTC()
	insertNotification(t, db, "a", base)
	insertNotification(t, db, "b", base)
	if _, err := db.Exec(`UPDATE notification_outbox SET delivery_state = 'delivered' WHERE id = 'b'`); err != nil {
		t.Fatalf("deliver the notification: %v", err)
	}

	count, err := outbox.UnfinishedNotifications(ctx)
	if err != nil {
		t.Fatalf("count the notifications: %v", err)
	}
	if count != 1 {
		t.Fatalf("the unfinished notifications = %d, want 1", count)
	}
}

// TestTheNotificationIsNotQueuedWhenTheQueueIsFull proves that the queue
// of the notifications does not grow without a bound.
//
// The event stays in the platform, but the notification is marked as one
// that was not queued, so the administrator sees that the target was
// missed.
func TestTheNotificationIsNotQueuedWhenTheQueueIsFull(t *testing.T) {
	outbox, db, _ := newOutbox(t)
	ctx := context.Background()

	base := time.Unix(1800000000, 0).UTC()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("start the transaction: %v", err)
	}
	statement, err := tx.Prepare(`
		INSERT INTO notification_outbox
		    (id, event_id, transition_seq, state, occurred_at, summary, environment, object, next_attempt_at)
		VALUES (?, ?, 1, 'active', ?, 'summary', 'environment', 'object', ?)`)
	if err != nil {
		t.Fatalf("prepare the statement: %v", err)
	}
	for index := 0; index < alerts.MaxPendingNotifications; index++ {
		id := fmt.Sprintf("n%d", index)
		if _, err := statement.Exec(id, "event-"+id, base.UnixMilli(), base.UnixMilli()); err != nil {
			t.Fatalf("write the notification: %v", err)
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatalf("close the statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the transaction: %v", err)
	}

	engine := alerts.New(db)
	if err := engine.PutRule(ctx, alerts.Rule{
		ID:        "r1",
		Kind:      alerts.KindResource,
		Metric:    "cpu_percent",
		Threshold: 80,
		Enabled:   true,
	}); err != nil {
		t.Fatalf("write the rule: %v", err)
	}
	event, err := engine.Observe(ctx, "r1", alerts.Observation{
		ObjectID: "c1",
		At:       base,
		Value:    90,
		Valid:    true,
	})
	if err != nil {
		t.Fatalf("open the alert: %v", err)
	}
	if event == nil {
		t.Fatal("the alert did not open")
	}

	count, err := outbox.UnfinishedNotifications(ctx)
	if err != nil {
		t.Fatalf("count the notifications: %v", err)
	}
	if count != alerts.MaxPendingNotifications {
		t.Fatalf("the unfinished notifications = %d, want %d", count, alerts.MaxPendingNotifications)
	}

	var state, lastError string
	if err := db.QueryRow(`SELECT delivery_state, last_error FROM notification_outbox WHERE event_id = ?`,
		event.ID).Scan(&state, &lastError); err != nil {
		t.Fatalf("read the notification: %v", err)
	}
	if state != "skipped" {
		t.Fatalf("the delivery state = %q, want skipped", state)
	}
	if lastError == "" {
		t.Fatal("the notification that was not queued carries no reason")
	}
}
