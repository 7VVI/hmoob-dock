package notify

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/secret"
)

// The limits of the worker.
const (
	// LeaseDuration is the time a worker holds a notification it
	// claimed. A worker that died releases the notification when its
	// lease ends, so the notification is sent again.
	LeaseDuration = 60 * time.Second
	// MaxAttempts is the number of the tries of one notification.
	MaxAttempts = alerts.MaxNotificationAttempts
	// ResultTimeout is the time the result of a try needs to reach the
	// database.
	//
	// The result of the last try of a notification is written with a
	// context that the end of the run does not stop, so a run that ends
	// while it writes the result does not leave a notification behind
	// that waits for a try that never comes.
	ResultTimeout = 5 * time.Second
	// notificationBatch is the number of the notifications one run
	// reads.
	notificationBatch = 20
)

// retryDelays are the waits between the tries of a notification.
//
// The wait grows with every failed try, so a target that is down is not
// flooded and the queue of the notifications drains slowly.
var retryDelays = []time.Duration{
	30 * time.Second,
	2 * time.Minute,
	10 * time.Minute,
}

// RetryDelay returns the wait before the next try of a notification.
//
// The wait of the first try is 30 seconds, the wait of the second try is
// 2 minutes and the wait of the third try is 10 minutes. A notification
// that failed all of its tries leaves the queue.
func RetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	if attempt > len(retryDelays) {
		return retryDelays[len(retryDelays)-1]
	}
	return retryDelays[attempt-1]
}

// Worker sends the notifications of the alerts to the target of the
// administrator.
//
// The worker claims every notification durably before it sends it, so a
// restart of the control端 neither loses a notification nor sends one
// twice at the same time. The result of the try is stored, and a failed
// notification waits for its next try. The delivery is made at least
// once: a worker that died after it sent a notification and before it
// recorded the result sends it again, and the receiver deduplicates it
// by the identifier of the notification.
type Worker struct {
	// now reads the clock; the tests replace it.
	now func() time.Time
	// store reads the target of the administrator.
	store *Store
	// outbox is the queue of the notifications.
	outbox *alerts.Outbox
	// client sends the notifications. It is empty in the production
	// path, which builds the client of the sender.
	client *http.Client
}

// NewWorker opens the worker on the database.
//
// The worker reads the target of the administrator and the queue of the
// notifications. It never evaluates an observation, so it never touches
// the continuity of the rules.
func NewWorker(db *sql.DB, box *secret.Box, now func() time.Time) *Worker {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	outbox := alerts.NewOutbox(db)
	outbox.UseClock(now)
	return &Worker{
		now:    now,
		store:  NewStore(db, box),
		outbox: outbox,
	}
}

// UseClient replaces the client of the worker.
//
// The tests use it to receive the notifications without a connection. The
// production path builds the client of the sender, which carries the
// policy of the addresses.
func (w *Worker) UseClient(client *http.Client) *Worker {
	w.client = client
	return w
}

// RunOnce sends the notifications that wait for their delivery.
//
// Every notification is claimed before it is sent, so a run that is
// interrupted leaves the notification to the next run instead of losing
// it. The failures of the run are reported, but they never stop the other
// notifications.
func (w *Worker) RunOnce(ctx context.Context) error {
	var failures []error
	// The notifications that used all of their tries leave the queue, so
	// they do not block the notifications that follow them.
	if err := w.sweep(ctx); err != nil {
		failures = append(failures, err)
	}

	target, found, err := w.store.Target(ctx, DefaultTargetID)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	if !found || !target.Enabled || strings.TrimSpace(target.URL) == "" {
		return errors.Join(failures...)
	}
	sender, err := w.sender(target)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	pending, err := w.outbox.PendingNotifications(ctx, notificationBatch)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}

	for _, notification := range pending {
		if ctx.Err() != nil {
			// The run is over: the notifications that follow wait for
			// the next run instead of holding the end of it.
			break
		}
		if err := w.deliver(ctx, sender, notification); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// sweep ends the notifications that used all of their tries.
//
// A worker that died after its last claim leaves a notification behind
// that no worker can claim again. The sweep ends it, so it does not
// starve the notifications that follow it. The sweep is idempotent.
func (w *Worker) sweep(ctx context.Context) error {
	_, err := w.outbox.FailTheExhaustedNotifications(ctx)
	return err
}

// sender builds the sender of the target.
func (w *Worker) sender(target Target) (*Sender, error) {
	config := WebhookConfig{
		ID:                  target.ID,
		URL:                 target.URL,
		AllowedPrivateCIDRs: target.AllowedPrivateCIDRs,
		Enabled:             target.Enabled,
	}
	if w.client != nil {
		return NewSenderWithClient(config, w.client)
	}
	return NewSender(config)
}

// deliver sends one notification and records the result.
//
// The claim comes first, so the notification is sent at most once at a
// time and a worker that dies in the middle does not lose it.
func (w *Worker) deliver(ctx context.Context, sender *Sender, notification alerts.Notification) error {
	now := w.now().UTC()
	claimed, attempt, err := w.outbox.ClaimNotification(ctx, notification.ID, now.Add(LeaseDuration))
	if err != nil {
		return err
	}
	if !claimed {
		// Another worker holds the notification, it was delivered or it
		// has no try left.
		return nil
	}

	// The try runs with a context that the end of the run does not stop:
	// the target either answers or it does not, so the result of the try
	// tells whether the notification reached it. The client of the
	// sender bounds the try with its own timeout.
	sendErr := sender.Send(context.WithoutCancel(ctx), notification)
	if sendErr == nil {
		return w.complete(ctx, notification.ID, alerts.Delivery{Delivered: true})
	}

	failure := DescribeFailure(sendErr)
	delivery := alerts.Delivery{Failure: failure}
	if attempt >= MaxAttempts {
		// The notification used all of its tries, so it leaves the
		// queue instead of waiting forever.
		delivery.Terminal = true
	} else {
		delivery.NextAttemptAt = now.Add(RetryDelay(attempt))
	}
	if err := w.complete(ctx, notification.ID, delivery); err != nil {
		return err
	}
	return fmt.Errorf("notify: the notification %s was not delivered: %s", notification.ID, failure)
}

// complete records the result of a try.
//
// The result is written with a context that the end of the run does not
// stop and with a short timeout: the result of the last try of a
// notification must reach the database, or the notification waits for a
// try that never comes and it starves the notifications that follow it.
func (w *Worker) complete(ctx context.Context, id string, delivery alerts.Delivery) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ResultTimeout)
	defer cancel()
	return w.outbox.CompleteNotification(writeCtx, id, delivery)
}

// DescribeFailure describes why a delivery failed.
//
// The address of the target is not part of the description: it can carry
// a secret in its path, and the description ends up in the database and
// in the log of the control端.
func DescribeFailure(err error) string {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		if urlError.Timeout() {
			return "the target did not answer in time"
		}
		if urlError.Err != nil {
			return urlError.Err.Error()
		}
	}
	return err.Error()
}

// TestNotification builds the notification of a manual test.
//
// The notification is not stored: a test is an action of the
// administrator and not a transition of an event.
func TestNotification() alerts.Notification {
	return alerts.Notification{
		ID:          randomID(),
		EventID:     "test",
		Transition:  1,
		State:       alerts.StateActive,
		OccurredAt:  time.Now().UTC(),
		Summary:     "这是一条测试通知",
		Environment: "test",
		Object:      "test",
	}
}

// randomID makes an identifier for a notification.
func randomID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// The identifier of a test is not a secret: the time keeps the
		// values apart when the system source is not available.
		return fmt.Sprintf("test-%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(buffer[:])
}
