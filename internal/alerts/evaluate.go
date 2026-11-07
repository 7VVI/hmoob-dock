package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"hmoob-dock/internal/store"
)

// Observe evaluates one sample of an object against one rule.
//
// The answer is nil unless the sample changed the state of the alert: the
// first sample above the threshold of a rule waits for its duration, every
// further sample of the same anomaly only updates the event, and a gap of
// the data changes nothing. A changed state is one transition: the event and
// its notification are written in the same transaction, so a restart of the
// control端 neither loses a notification nor sends a second one.
func (e *Engine) Observe(ctx context.Context, ruleID string, value Observation) (*Event, error) {
	rule, err := e.rule(ctx, ruleID)
	if err != nil {
		return nil, err
	}
	if !rule.Enabled {
		return nil, nil
	}
	if value.ObjectID == "" {
		return nil, errors.New("alerts: the observation names no object")
	}

	at := value.At.UTC()
	condition, err := e.condition(ctx, ruleID, value.ObjectID)
	if err != nil {
		return nil, err
	}

	// A gap longer than two intervals interrupts the continuity: the first
	// sample after it proves the condition again from scratch.
	if condition.lastObservedAt != 0 &&
		at.Sub(store.TimeFromMillis(condition.lastObservedAt)) > 2*DefaultSampleInterval {
		condition.pendingSince = 0
		condition.recoveringSince = 0
	}

	// An invalid observation is a gap of the data: it neither opens a new
	// alert, nor continues a pending one, nor resolves an active one.
	if !value.Valid {
		condition.pendingSince = 0
		condition.recoveringSince = 0
		condition.lastObservedAt = store.TimeToMillis(at)
		if err := e.writeCondition(ctx, ruleID, value.ObjectID, condition); err != nil {
			return nil, err
		}
		return nil, nil
	}

	anomaly := e.isAnomaly(rule, value)
	if value.ExpectedAction {
		// The end of the container has an explanation the platform knows,
		// so it is not an anomaly. An event that is already open is over.
		anomaly = false
		condition.pendingSince = 0
	}

	active, err := e.activeEvent(ctx, ruleID, value.ObjectID)
	if err != nil {
		return nil, err
	}

	if anomaly {
		condition.recoveringSince = 0
		if active != nil {
			// The anomaly continues: the event keeps its start and no
			// notification is sent again.
			condition.pendingSince = 0
			condition.lastObservedAt = store.TimeToMillis(at)
			if err := e.touchEvent(ctx, active.ID, at); err != nil {
				return nil, err
			}
			if err := e.writeCondition(ctx, ruleID, value.ObjectID, condition); err != nil {
				return nil, err
			}
			return nil, nil
		}
		if condition.pendingSince == 0 {
			condition.pendingSince = store.TimeToMillis(at)
		}
		condition.lastObservedAt = store.TimeToMillis(at)
		if at.Sub(store.TimeFromMillis(condition.pendingSince)) < rule.For {
			if err := e.writeCondition(ctx, ruleID, value.ObjectID, condition); err != nil {
				return nil, err
			}
			return nil, nil
		}
		condition.pendingSince = 0
		opened, err := e.openEvent(ctx, rule, value.ObjectID, at)
		if err != nil {
			return nil, err
		}
		if err := e.writeCondition(ctx, ruleID, value.ObjectID, condition); err != nil {
			return nil, err
		}
		return opened, nil
	}

	condition.pendingSince = 0
	condition.lastObservedAt = store.TimeToMillis(at)
	if active == nil {
		condition.recoveringSince = 0
		if err := e.writeCondition(ctx, ruleID, value.ObjectID, condition); err != nil {
			return nil, err
		}
		return nil, nil
	}

	// An expected action explains the end at once; any other quiet sample
	// has to hold for the duration of the rule, so a value that flickers
	// around the threshold does not send a recovery every few seconds.
	if !value.ExpectedAction {
		if condition.recoveringSince == 0 {
			condition.recoveringSince = store.TimeToMillis(at)
		}
		if at.Sub(store.TimeFromMillis(condition.recoveringSince)) < rule.For {
			if err := e.touchEvent(ctx, active.ID, at); err != nil {
				return nil, err
			}
			if err := e.writeCondition(ctx, ruleID, value.ObjectID, condition); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}
	condition.recoveringSince = 0
	resolved, err := e.resolveEvent(ctx, rule, active, at)
	if err != nil {
		return nil, err
	}
	if err := e.writeCondition(ctx, ruleID, value.ObjectID, condition); err != nil {
		return nil, err
	}
	return resolved, nil
}

// isAnomaly reports whether the sample meets the condition of the rule.
//
// A resource rule compares the observed value with its threshold in the
// direction its operator names. An exit or an offline node is a flag: any
// non zero value reports it.
func (e *Engine) isAnomaly(rule Rule, value Observation) bool {
	if rule.Kind == KindResource {
		switch rule.Operator {
		case OperatorGreaterOrEqual:
			return value.Value >= rule.Threshold
		case OperatorLessThan:
			return value.Value < rule.Threshold
		case OperatorLessOrEqual:
			return value.Value <= rule.Threshold
		default:
			return value.Value > rule.Threshold
		}
	}
	// An exit or an offline node is a flag: any non zero value reports it.
	return value.Value != 0
}

// condition is the continuity of one rule and one object.
type condition struct {
	pendingSince    int64
	recoveringSince int64
	lastObservedAt  int64
}

// condition reads the continuity of one rule and one object.
func (e *Engine) condition(ctx context.Context, ruleID, objectID string) (condition, error) {
	var value condition
	err := e.db.QueryRowContext(ctx, `
		SELECT pending_since, recovering_since, last_observed_at
		  FROM alert_conditions
		 WHERE rule_id = ? AND object_id = ?`, ruleID, objectID).
		Scan(&value.pendingSince, &value.recoveringSince, &value.lastObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return condition{}, nil
	}
	if err != nil {
		return condition{}, fmt.Errorf("alerts: read the condition of %s: %w", objectID, err)
	}
	return value, nil
}

// writeCondition writes the continuity of one rule and one object.
func (e *Engine) writeCondition(ctx context.Context, ruleID, objectID string, value condition) error {
	if _, err := e.db.ExecContext(ctx, `
		INSERT INTO alert_conditions (rule_id, object_id, pending_since, recovering_since, last_observed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (rule_id, object_id) DO UPDATE SET
		    pending_since = excluded.pending_since,
		    recovering_since = excluded.recovering_since,
		    last_observed_at = excluded.last_observed_at`,
		ruleID, objectID, value.pendingSince, value.recoveringSince, value.lastObservedAt); err != nil {
		return fmt.Errorf("alerts: write the condition of %s: %w", objectID, err)
	}
	return nil
}

// rule reads one rule.
func (e *Engine) rule(ctx context.Context, ruleID string) (Rule, error) {
	var rule Rule
	var forMS int64
	var enabled, recoverNotify int
	err := e.db.QueryRowContext(ctx, `
		SELECT id, name, description, kind, severity, scope_kind, scope_id, metric, operator, threshold, for_ms, enabled, recover_notify
		  FROM alert_rules
		 WHERE id = ?`, ruleID).
		Scan(&rule.ID, &rule.Name, &rule.Description, &rule.Kind, &rule.Severity,
			&rule.ScopeKind, &rule.ScopeID, &rule.Metric, &rule.Operator, &rule.Threshold,
			&forMS, &enabled, &recoverNotify)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, fmt.Errorf("%w: %s", ErrRuleNotFound, ruleID)
	}
	if err != nil {
		return Rule{}, fmt.Errorf("alerts: read the rule %s: %w", ruleID, err)
	}
	rule.For = time.Duration(forMS) * time.Millisecond
	rule.ForSeconds = int(rule.For / time.Second)
	rule.Enabled = enabled != 0
	rule.RecoverQuiet = recoverNotify == 0
	return rule, nil
}

// activeEvent reads the open event of one rule and one object.
func (e *Engine) activeEvent(ctx context.Context, ruleID, objectID string) (*Event, error) {
	row := e.db.QueryRowContext(ctx, `
		SELECT id, rule_id, object_id, state, summary, started_at, resolved_at, acknowledged_at
		  FROM alert_events
		 WHERE rule_id = ? AND object_id = ? AND state = 'active'`, ruleID, objectID)
	event, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &event, nil
}

// openEvent writes the event of a new anomaly and its notification.
func (e *Engine) openEvent(ctx context.Context, rule Rule, objectID string, at time.Time) (*Event, error) {
	event := Event{
		ID:        e.newID(),
		RuleID:    rule.ID,
		ObjectID:  objectID,
		State:     StateActive,
		Summary:   e.summary(rule, objectID, StateActive),
		StartedAt: at,
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("alerts: start the transaction of the event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO alert_events (id, rule_id, object_id, state, summary, started_at, last_observed_at, transition_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)`,
		event.ID, event.RuleID, event.ObjectID, event.State, event.Summary,
		store.TimeToMillis(at), store.TimeToMillis(at)); err != nil {
		return nil, fmt.Errorf("alerts: open the event of %s: %w", objectID, err)
	}
	if err := e.enqueue(ctx, tx, event, 1, at); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("alerts: commit the event of %s: %w", objectID, err)
	}
	return &event, nil
}

// resolveEvent closes an open event and records its notification.
func (e *Engine) resolveEvent(ctx context.Context, rule Rule, active *Event, at time.Time) (*Event, error) {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("alerts: start the transaction of the recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT transition_seq FROM alert_events WHERE id = ?`, active.ID).Scan(&sequence); err != nil {
		return nil, fmt.Errorf("alerts: read the event %s: %w", active.ID, err)
	}
	next := sequence + 1
	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_events
		   SET state = 'resolved', resolved_at = ?, last_observed_at = ?, transition_seq = ?
		 WHERE id = ? AND state = 'active'`,
		store.TimeToMillis(at), store.TimeToMillis(at), next, active.ID); err != nil {
		return nil, fmt.Errorf("alerts: resolve the event %s: %w", active.ID, err)
	}

	resolved := *active
	resolved.State = StateResolved
	resolved.ResolvedAt = &at
	resolved.Summary = e.summary(rule, active.ObjectID, StateResolved)
	// A rule with a quiet recovery resolves without a word: the event of
	// the platform records the recovery, no notification leaves.
	if !rule.RecoverQuiet {
		if err := e.enqueue(ctx, tx, resolved, next, at); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("alerts: commit the recovery of %s: %w", active.ID, err)
	}
	return &resolved, nil
}

// enqueue writes the notification of one transition.
//
// The queue of the notifications is bounded: when it is full, the event
// stays in the platform and the notification is marked as one that was
// not queued, so the queue never grows without a bound.
func (e *Engine) enqueue(ctx context.Context, tx *sql.Tx, event Event, sequence int64, at time.Time) error {
	pending, err := countPendingNotifications(ctx, tx)
	if err != nil {
		return err
	}
	state := DeliveryPending
	failure := ""
	if pending >= MaxPendingNotifications {
		state = DeliverySkipped
		failure = "the queue of the notifications is full, the notification was not queued"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notification_outbox
		    (id, event_id, transition_seq, state, occurred_at, summary, environment, object, next_attempt_at, delivery_state, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (event_id, transition_seq) DO NOTHING`,
		e.newID(), event.ID, sequence, event.State, store.TimeToMillis(at),
		event.Summary, event.RuleID, event.ObjectID, store.TimeToMillis(at),
		state, failure); err != nil {
		return fmt.Errorf("alerts: record the notification of %s: %w", event.ID, err)
	}
	return nil
}

// countPendingNotifications counts the notifications that wait for their
// delivery inside a transaction.
func countPendingNotifications(ctx context.Context, tx *sql.Tx) (int, error) {
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM notification_outbox WHERE delivery_state = 'pending'`).Scan(&count); err != nil {
		return 0, fmt.Errorf("alerts: count the notifications: %w", err)
	}
	return count, nil
}

// touchEvent updates the last observation of an open event.
func (e *Engine) touchEvent(ctx context.Context, eventID string, at time.Time) error {
	if _, err := e.db.ExecContext(ctx,
		`UPDATE alert_events SET last_observed_at = ? WHERE id = ? AND state = 'active'`,
		store.TimeToMillis(at), eventID); err != nil {
		return fmt.Errorf("alerts: update the event %s: %w", eventID, err)
	}
	return nil
}

// summary describes an event in the language of its rule.
func (e *Engine) summary(rule Rule, objectID, state string) string {
	if state == StateResolved {
		return fmt.Sprintf("对象 %s 已恢复正常", objectID)
	}
	switch rule.Kind {
	case KindResource:
		direction := "超过"
		if rule.Operator == OperatorLessThan || rule.Operator == OperatorLessOrEqual {
			direction = "低于"
		}
		return fmt.Sprintf("对象 %s 的 %s 持续%s阈值 %g", objectID, rule.Metric, direction, rule.Threshold)
	case KindContainerExit:
		return fmt.Sprintf("对象 %s 意外退出", objectID)
	case KindAgentOffline:
		return fmt.Sprintf("节点 %s 已经失联", objectID)
	}
	return fmt.Sprintf("对象 %s 触发告警", objectID)
}

// newID makes an identifier for an event or a notification.
func (e *Engine) newID() string {
	return randomID()
}
