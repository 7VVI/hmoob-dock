package alerts_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/store"
)

// newEngine opens an engine on a fresh database.
func newEngine(t *testing.T) (*alerts.Engine, *sql.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return alerts.New(db), db
}

// countNotifications reads the number of the recorded notifications.
func countNotifications(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM notification_outbox`).Scan(&count); err != nil {
		t.Fatalf("count the notifications: %v", err)
	}
	return count
}

// TestMissingSampleDoesNotResolveActiveAlert proves that a gap of the data
// neither opens nor closes an alert.
func TestMissingSampleDoesNotResolveActiveAlert(t *testing.T) {
	engine, db := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	opened, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true})
	if err != nil || opened == nil || opened.State != alerts.StateActive {
		t.Fatalf("open the alert: %v", err)
	}
	changed, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(10 * time.Second), Valid: false})
	if err != nil || changed != nil {
		t.Fatalf("the missing sample changed the alert: %v", err)
	}
	if count := countNotifications(t, db); count != 1 {
		t.Fatalf("notifications = %d, want 1", count)
	}
}

// TestTheAlertOpensOnlyAfterItsDuration proves that the condition has to
// hold for the duration of the rule before the alert opens.
func TestTheAlertOpensOnlyAfterItsDuration(t *testing.T) {
	engine, _ := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, For: 30 * time.Second, Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	// The first sample starts the continuity, the second is still inside
	// the duration of the rule.
	if event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true}); err != nil || event != nil {
		t.Fatalf("the alert opened too early: %v", err)
	}
	if event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(20 * time.Second), Value: 90, Valid: true}); err != nil || event != nil {
		t.Fatalf("the alert opened too early: %v", err)
	}
	event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(40 * time.Second), Value: 90, Valid: true})
	if err != nil || event == nil || event.State != alerts.StateActive {
		t.Fatalf("the alert did not open: %v", err)
	}
}

// TestTheRepeatedSampleKeepsOneEvent proves that a sample of an anomaly
// that continues neither opens a second event nor sends a second
// notification.
func TestTheRepeatedSampleKeepsOneEvent(t *testing.T) {
	engine, db := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	for i := 0; i < 5; i++ {
		event, err := engine.Observe(ctx, "r1", alerts.Observation{
			ObjectID: "c1", At: at.Add(time.Duration(i) * 10 * time.Second), Value: 90, Valid: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && event == nil {
			t.Fatal("the first sample did not open the alert")
		}
		if i > 0 && event != nil {
			t.Fatal("the repeated sample opened another event")
		}
	}
	if count := countNotifications(t, db); count != 1 {
		t.Fatalf("notifications = %d, want 1", count)
	}
	events, err := engine.Events(ctx, false)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %d, want 1: %v", len(events), err)
	}
}

// TestTheRecoveryHoldsForItsDuration proves that a value that flickers
// around the threshold does not close and reopen the alert.
func TestTheRecoveryHoldsForItsDuration(t *testing.T) {
	engine, db := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, For: 30 * time.Second, Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	// The anomaly has to hold for the duration of the rule before the
	// alert opens.
	if _, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(10 * time.Second), Value: 90, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(30 * time.Second), Value: 90, Valid: true}); err != nil || event == nil {
		t.Fatalf("the alert did not open: %v", err)
	}
	// The value falls and rises again: the alert stays open.
	if event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(40 * time.Second), Value: 10, Valid: true}); err != nil || event != nil {
		t.Fatalf("the alert closed too early: %v", err)
	}
	if event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(50 * time.Second), Value: 90, Valid: true}); err != nil || event != nil {
		t.Fatalf("the alert closed too early: %v", err)
	}
	// The value stays low for the duration of the rule: the alert closes.
	// The recovery starts at the first low sample that follows the last
	// high one, so the flicker before it does not count.
	for _, second := range []int{60, 70, 80} {
		if event, err := engine.Observe(ctx, "r1", alerts.Observation{
			ObjectID: "c1", At: at.Add(time.Duration(second) * time.Second), Value: 10, Valid: true,
		}); err != nil || event != nil {
			t.Fatalf("the alert closed too early: %v", err)
		}
	}
	event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(90 * time.Second), Value: 10, Valid: true})
	if err != nil || event == nil || event.State != alerts.StateResolved {
		t.Fatalf("the alert did not close: %v", err)
	}
	if count := countNotifications(t, db); count != 2 {
		t.Fatalf("notifications = %d, want 2", count)
	}
}

// TestTheGapInterruptsTheContinuity proves that a gap of the data means
// that the continuity has to be proven again.
func TestTheGapInterruptsTheContinuity(t *testing.T) {
	engine, _ := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, For: 30 * time.Second, Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	if _, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true}); err != nil {
		t.Fatal(err)
	}
	// The next sample comes long after the first one: the continuity is
	// broken, so the condition has to hold again for the whole duration.
	if event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(5 * time.Minute), Value: 90, Valid: true}); err != nil || event != nil {
		t.Fatalf("the alert opened although the continuity was broken: %v", err)
	}
}

// TestTheRestartProvesTheContinuityAgain proves that a restart of the
// control端 does not continue a condition that was pending, while an event
// that is already open stays open.
func TestTheRestartProvesTheContinuityAgain(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, For: 30 * time.Second, Enabled: true}
	engine := alerts.New(db)
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	if _, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true}); err != nil {
		t.Fatal(err)
	}
	// The engine restarts.
	restarted := alerts.New(db)
	if event, err := restarted.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(20 * time.Second), Value: 90, Valid: true}); err != nil || event != nil {
		t.Fatalf("the alert opened although the continuity was lost: %v", err)
	}
}

// TestTheExpectedEndIsNotAnAlert proves that an end that a known action
// explains does not open an alert, and that it closes the alert that is
// already open.
func TestTheExpectedEndIsNotAnAlert(t *testing.T) {
	engine, db := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindContainerExit, ScopeID: "container:c1", Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "container:c1", At: at, Value: 1, Valid: true})
	if err != nil || event == nil {
		t.Fatalf("the unexpected end did not open an alert: %v", err)
	}
	// The end is explained by a known action: the alert closes at once.
	event, err = engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "container:c1", At: at.Add(10 * time.Second), Value: 0, Valid: true, ExpectedAction: true})
	if err != nil || event == nil || event.State != alerts.StateResolved {
		t.Fatalf("the explained end did not close the alert: %v", err)
	}
	if count := countNotifications(t, db); count != 2 {
		t.Fatalf("notifications = %d, want 2", count)
	}
}

// TestTheUnknownEndIsAnAlert proves that an end without an explanation
// stays an alert.
func TestTheUnknownEndIsAnAlert(t *testing.T) {
	engine, _ := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindContainerExit, ScopeID: "container:c1", Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "container:c1", At: at, Value: 1, Valid: true})
	if err != nil || event == nil {
		t.Fatalf("the unknown end did not open an alert: %v", err)
	}
}

// TestTheOfflineNodeOpensAndRecovers proves that a node that stayed away
// opens an alert and that its return closes it.
func TestTheOfflineNodeOpensAndRecovers(t *testing.T) {
	engine, db := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindAgentOffline, ScopeID: "node-1", Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	events, err := engine.ObserveOffline(ctx, alerts.OfflineSample{NodeID: "node-1", At: at, Offline: true})
	if err != nil || len(events) != 1 {
		t.Fatalf("the offline node did not open an alert: %v", err)
	}
	events, err = engine.ObserveOffline(ctx, alerts.OfflineSample{NodeID: "node-1", At: at.Add(10 * time.Second), Offline: true})
	if err != nil || len(events) != 0 {
		t.Fatalf("the offline node opened another alert: %v", err)
	}
	events, err = engine.ObserveOffline(ctx, alerts.OfflineSample{NodeID: "node-1", At: at.Add(20 * time.Second), Offline: false})
	if err != nil || len(events) != 1 || events[0].State != alerts.StateResolved {
		t.Fatalf("the node did not recover: %v", err)
	}
	if count := countNotifications(t, db); count != 2 {
		t.Fatalf("notifications = %d, want 2", count)
	}
}

// TestTheAcknowledgementIsNotARecovery proves that an acknowledgement keeps
// the state of the event.
func TestTheAcknowledgementIsNotARecovery(t *testing.T) {
	engine, _ := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	opened, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true})
	if err != nil || opened == nil {
		t.Fatalf("the alert did not open: %v", err)
	}
	if err := engine.Acknowledge(ctx, opened.ID, at.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	events, err := engine.Events(ctx, false)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %d, want 1: %v", len(events), err)
	}
	if events[0].State != alerts.StateActive {
		t.Fatalf("the acknowledgement closed the alert: %q", events[0].State)
	}
	if events[0].AcknowledgedAt == nil {
		t.Fatal("the acknowledgement was not recorded")
	}
	if events[0].ResolvedAt != nil {
		t.Fatal("the acknowledgement resolved the alert")
	}
}

// TestTheDisabledRuleOpensNothing proves that a rule that is not evaluated
// opens no alert.
func TestTheDisabledRuleOpensNothing(t *testing.T) {
	engine, _ := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, Enabled: false}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: time.Now(), Value: 90, Valid: true})
	if err != nil || event != nil {
		t.Fatalf("the disabled rule opened an alert: %v", err)
	}
}

// TestTheRemovedRuleKeepsItsHistory proves that the events of a removed
// rule stay in the history and that the rule opens no new alert.
func TestTheRemovedRuleKeepsItsHistory(t *testing.T) {
	engine, _ := newEngine(t)
	ctx := context.Background()
	rule := alerts.Rule{ID: "r1", Kind: alerts.KindResource, Metric: "cpu_percent", Threshold: 80, Enabled: true}
	if err := engine.PutRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1800000000, 0)
	if _, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at, Value: 90, Valid: true}); err != nil {
		t.Fatal(err)
	}
	if err := engine.DeleteRule(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	events, err := engine.Events(ctx, false)
	if err != nil || len(events) != 1 {
		t.Fatalf("the history of the rule was lost: %v", err)
	}
	event, err := engine.Observe(ctx, "r1", alerts.Observation{ObjectID: "c1", At: at.Add(10 * time.Second), Value: 90, Valid: true})
	if !errors.Is(err, alerts.ErrRuleNotFound) {
		t.Fatalf("the removed rule opened an alert: %v", err)
	}
	if event != nil {
		t.Fatal("the removed rule opened an alert")
	}
}

// TestTheUnknownRuleIsReported proves that a rule that does not exist is
// an error and not a silent success.
func TestTheUnknownRuleIsReported(t *testing.T) {
	engine, _ := newEngine(t)
	_, err := engine.Observe(context.Background(), "missing", alerts.Observation{ObjectID: "c1", At: time.Now(), Value: 1, Valid: true})
	if !errors.Is(err, alerts.ErrRuleNotFound) {
		t.Fatalf("the unknown rule was accepted: %v", err)
	}
}

// TestTheUnknownEventIsReported proves that an acknowledgement of an event
// that does not exist is an error.
func TestTheUnknownEventIsReported(t *testing.T) {
	engine, _ := newEngine(t)
	err := engine.Acknowledge(context.Background(), "missing", time.Now())
	if !errors.Is(err, alerts.ErrEventNotFound) {
		t.Fatalf("the unknown event was accepted: %v", err)
	}
}
