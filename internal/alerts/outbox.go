package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"hmoob-dock/internal/store"
)

// MaxPendingNotifications bounds the queue of the notifications.
//
// The queue may not grow without a bound: a target that is down would
// leave a notification behind for every transition of every event. When
// the queue is full, the event stays in the platform and the notification
// is marked as one that was not queued, so the queue never grows further.
const MaxPendingNotifications = 10000

// MaxNotificationAttempts is the number of the tries of one notification.
//
// The first try and three retries follow it, so a notification that the
// target never accepts leaves the queue instead of waiting forever.
const MaxNotificationAttempts = 4

// The states of the delivery of a notification.
const (
	// DeliveryPending reports a notification that waits for its delivery.
	DeliveryPending = "pending"
	// DeliveryDelivered reports a notification that reached its target.
	DeliveryDelivered = "delivered"
	// DeliveryFailed reports a notification that gave up after its last try.
	DeliveryFailed = "failed"
	// DeliverySkipped reports a notification that was never queued.
	DeliverySkipped = "skipped"
)

// defaultNotificationBatch is the number of the notifications one read of
// the queue returns.
const defaultNotificationBatch = 100

// exhaustedFailure is the reason of a notification that used all of its
// tries.
//
// The reason carries no address and no secret: it ends up in the database
// and in the log of the control端.
const exhaustedFailure = "the notification used all of its tries"

// Delivery is the result of one try to deliver a notification.
type Delivery struct {
	// Delivered reports whether the target accepted the notification.
	Delivered bool
	// Failure describes why the try failed. It is empty after a
	// successful delivery.
	Failure string
	// NextAttemptAt is the time of the next try. It is empty when the
	// notification leaves the queue.
	NextAttemptAt time.Time
	// Terminal reports whether the notification leaves the queue after
	// this try.
	Terminal bool
}

// Outbox is the queue of the notifications that wait for their delivery.
//
// The sender of the notifications uses the outbox to claim the
// notifications and to record their result. It never evaluates an
// observation, so it never touches the continuity of the rules.
type Outbox struct {
	// db is the write pool of the control端.
	db *sql.DB
	// now reads the clock; the tests replace it.
	now func() time.Time
}

// NewOutbox opens the outbox of the notifications on the database.
//
// The outbox is not the engine of the alerts: it neither opens an event
// nor clears the continuity of the rules, so a sender may use it while
// the engine is running.
func NewOutbox(db *sql.DB) *Outbox {
	return &Outbox{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// UseClock replaces the clock of the outbox. The tests use it to drive
// the delivery without waiting.
func (o *Outbox) UseClock(now func() time.Time) {
	if now == nil {
		return
	}
	o.now = now
}

// PendingNotifications returns the notifications that wait for their
// delivery, the oldest first.
//
// A notification that waits for its retry is returned as soon as its next
// try is due, and a notification whose lease expired is returned again,
// so a worker that died does not strand its notification. The delivery
// of a notification is made at least once: a worker that died after it
// sent the notification and before it recorded the result sends it again,
// and the receiver deduplicates it by the identifier of the notification.
//
// A notification that used all of its tries is not returned: it cannot be
// claimed again, so a queue that returns it fills the batch with
// notifications that no worker can deliver and starves the notifications
// that follow them.
func (o *Outbox) PendingNotifications(ctx context.Context, limit int) ([]Notification, error) {
	if limit <= 0 {
		limit = defaultNotificationBatch
	}
	now := store.TimeToMillis(o.now())
	rows, err := o.db.QueryContext(ctx, `
		SELECT id, event_id, transition_seq, state, occurred_at, summary, environment, object
		  FROM notification_outbox
		 WHERE delivery_state = 'pending' AND next_attempt_at <= ? AND lease_until <= ?
		   AND attempts < ?
		 ORDER BY next_attempt_at, occurred_at, id
		 LIMIT ?`, now, now, MaxNotificationAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("alerts: read the notifications: %w", err)
	}
	defer rows.Close()

	var notifications []Notification
	for rows.Next() {
		notification, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		notifications = append(notifications, notification)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alerts: read the notifications: %w", err)
	}
	return notifications, nil
}

// ClaimNotification marks one notification as claimed until leaseUntil.
//
// The claim is durable: it counts the try and it holds the notification
// until the lease ends. The claim succeeds only when the notification
// still waits for its delivery and its lease has expired, so two workers
// never send the same notification at the same time. The number of the
// try is returned, so the sender knows how many tries the notification
// already had.
func (o *Outbox) ClaimNotification(ctx context.Context, id string, leaseUntil time.Time) (bool, int, error) {
	now := store.TimeToMillis(o.now())
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("alerts: start the claim of the notification %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET attempts = attempts + 1, lease_until = ?, next_attempt_at = ?
		 WHERE id = ? AND delivery_state = 'pending' AND delivered_at IS NULL
		   AND attempts < ? AND lease_until <= ?`,
		store.TimeToMillis(leaseUntil), store.TimeToMillis(leaseUntil), id,
		MaxNotificationAttempts, now)
	if err != nil {
		return false, 0, fmt.Errorf("alerts: claim the notification %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, 0, fmt.Errorf("alerts: claim the notification %s: %w", id, err)
	}
	if affected == 0 {
		// The notification is already delivered, already claimed or it
		// has no try left.
		return false, 0, nil
	}

	var attempt int
	if err := tx.QueryRowContext(ctx,
		`SELECT attempts FROM notification_outbox WHERE id = ?`, id).Scan(&attempt); err != nil {
		return false, 0, fmt.Errorf("alerts: read the attempt of the notification %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("alerts: commit the claim of the notification %s: %w", id, err)
	}
	return true, attempt, nil
}

// FailTheExhaustedNotifications ends the notifications that used all of
// their tries, and it returns the number of the notifications it ended.
//
// A worker that died after its last claim and before it wrote the result
// leaves a notification behind that no worker can claim again. Such a
// notification is the oldest one of the queue, so a batch that returns it
// delivers nothing and starves the notifications that follow it.
//
// The sweep only touches the notifications whose lease ended: a worker
// that is still at work keeps its notification. The sweep is idempotent:
// a notification that already left the queue is not ended again.
func (o *Outbox) FailTheExhaustedNotifications(ctx context.Context) (int, error) {
	now := store.TimeToMillis(o.now())
	result, err := o.db.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET delivery_state = 'failed', lease_until = 0, last_error = ?, next_attempt_at = ?
		 WHERE delivery_state = 'pending' AND attempts >= ? AND lease_until <= ?`,
		exhaustedFailure, now, MaxNotificationAttempts, now)
	if err != nil {
		return 0, fmt.Errorf("alerts: end the notifications that used all of their tries: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("alerts: end the notifications that used all of their tries: %w", err)
	}
	return int(affected), nil
}

// CompleteNotification records the result of the delivery of one
// notification.
//
// A delivered notification leaves the queue. A failed one waits until
// its next try, and it leaves the queue as well when its last try failed
// too.
func (o *Outbox) CompleteNotification(ctx context.Context, id string, delivery Delivery) error {
	now := o.now().UTC()
	var err error
	switch {
	case delivery.Delivered:
		_, err = o.db.ExecContext(ctx, `
			UPDATE notification_outbox
			   SET delivery_state = 'delivered', delivered_at = ?, lease_until = 0,
			       last_error = '', next_attempt_at = ?
			 WHERE id = ?`,
			store.TimeToMillis(now), store.TimeToMillis(now), id)
	case delivery.Terminal:
		_, err = o.db.ExecContext(ctx, `
			UPDATE notification_outbox
			   SET delivery_state = 'failed', lease_until = 0, last_error = ?, next_attempt_at = ?
			 WHERE id = ?`,
			delivery.Failure, store.TimeToMillis(now), id)
	default:
		next := delivery.NextAttemptAt
		if next.IsZero() {
			next = now
		}
		_, err = o.db.ExecContext(ctx, `
			UPDATE notification_outbox
			   SET lease_until = 0, last_error = ?, next_attempt_at = ?
			 WHERE id = ?`,
			delivery.Failure, store.TimeToMillis(next), id)
	}
	if err != nil {
		return fmt.Errorf("alerts: record the delivery of the notification %s: %w", id, err)
	}
	return nil
}

// UnfinishedNotifications counts the notifications that wait for their
// delivery.
func (o *Outbox) UnfinishedNotifications(ctx context.Context) (int, error) {
	var count int
	if err := o.db.QueryRowContext(ctx,
		`SELECT count(*) FROM notification_outbox WHERE delivery_state = 'pending'`).Scan(&count); err != nil {
		return 0, fmt.Errorf("alerts: count the notifications: %w", err)
	}
	return count, nil
}

// Counts describes the state of the queue of the notifications.
type Counts struct {
	// Pending is the number of the notifications that wait for their
	// delivery.
	Pending int
	// Delivered is the number of the notifications that reached their
	// target.
	Delivered int
	// Failed is the number of the notifications that gave up.
	Failed int
	// Skipped is the number of the notifications that were never queued.
	Skipped int
	// LastDeliveredAt is the time of the last delivery. It is zero when
	// no notification was ever delivered.
	LastDeliveredAt time.Time
}

// Counts returns the state of the queue of the notifications.
func (o *Outbox) Counts(ctx context.Context) (Counts, error) {
	var counts Counts
	rows, err := o.db.QueryContext(ctx,
		`SELECT delivery_state, count(*) FROM notification_outbox GROUP BY delivery_state`)
	if err != nil {
		return Counts{}, fmt.Errorf("alerts: count the notifications: %w", err)
	}
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return Counts{}, fmt.Errorf("alerts: count the notifications: %w", err)
		}
		switch state {
		case DeliveryPending:
			counts.Pending = count
		case DeliveryDelivered:
			counts.Delivered = count
		case DeliveryFailed:
			counts.Failed = count
		case DeliverySkipped:
			counts.Skipped = count
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Counts{}, fmt.Errorf("alerts: count the notifications: %w", err)
	}
	// The rows are closed before the next query: the write pool holds a
	// single connection, so a query that walks rows may not start
	// another one before it closed them.
	rows.Close()

	var lastDelivered sql.NullInt64
	if err := o.db.QueryRowContext(ctx,
		`SELECT max(delivered_at) FROM notification_outbox WHERE delivered_at IS NOT NULL`).
		Scan(&lastDelivered); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Counts{}, fmt.Errorf("alerts: read the last delivery: %w", err)
	}
	if lastDelivered.Valid {
		counts.LastDeliveredAt = store.TimeFromMillis(lastDelivered.Int64)
	}
	return counts, nil
}

// scanNotification reads one notification of a row.
func scanNotification(row scanner) (Notification, error) {
	var notification Notification
	var occurredAt int64
	if err := row.Scan(&notification.ID, &notification.EventID, &notification.Transition,
		&notification.State, &occurredAt, &notification.Summary,
		&notification.Environment, &notification.Object); err != nil {
		return Notification{}, fmt.Errorf("alerts: read a notification: %w", err)
	}
	notification.OccurredAt = store.TimeFromMillis(occurredAt)
	return notification, nil
}
