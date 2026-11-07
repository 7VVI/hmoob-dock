// Package alerts decides when the observation of a node becomes an alert.
//
// The engine keeps the continuity of the observation, so an anomaly is an
// event only after the duration of its rule, a gap of the data is neither a
// recovery nor a continuing anomaly, and a repeated sample never opens a
// second event or sends a second notification.
package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"hmoob-dock/internal/store"
)

// The kinds of a rule.
const (
	// KindContainerExit reports a container that stopped unexpectedly.
	KindContainerExit = "container_exit"
	// KindAgentOffline reports an agent that stopped sending heartbeats.
	KindAgentOffline = "agent_offline"
	// KindResource reports a container that stays above a threshold.
	KindResource = "resource"
)

// The scopes of a rule.
//
// The scope names the breadth a rule watches. A rule of one container
// watches the object of its scope, a rule of a namespace watches the
// containers of one Compose project or Swarm stack, and a rule of all
// watches every container the platform sees.
const (
	// ScopeAll reports every container of the platform.
	ScopeAll = "all"
	// ScopeContainer reports the one object of the rule.
	ScopeContainer = "container"
	// ScopeNamespace reports the containers of one namespace.
	ScopeNamespace = "namespace"
)

// The operators of a resource rule.
//
// The operator compares the observed value with the threshold of the rule,
// so a rule may fire above or below a limit.
const (
	// OperatorGreaterThan fires when the value stays above the threshold.
	OperatorGreaterThan = ">"
	// OperatorGreaterOrEqual fires when the value stays at or above the
	// threshold.
	OperatorGreaterOrEqual = ">="
	// OperatorLessThan fires when the value stays below the threshold.
	OperatorLessThan = "<"
	// OperatorLessOrEqual fires when the value stays at or below the
	// threshold.
	OperatorLessOrEqual = "<="
)

// The severities of a rule.
//
// The severity is the weight the administrator gives the rule. The engine
// does not evaluate it differently, the console reads it to color the rules
// and the events.
const (
	// SeverityInfo reports an alert that the administrator wants to see.
	SeverityInfo = "info"
	// SeverityWarning reports an alert that deserves attention.
	SeverityWarning = "warning"
	// SeverityCritical reports an alert that should not wait.
	SeverityCritical = "critical"
)

// The states of an event.
const (
	// StateActive reports an anomaly that is still present.
	StateActive = "active"
	// StateResolved reports an anomaly that is over.
	StateResolved = "resolved"
)

// ErrRuleNotFound reports a rule that the engine does not know.
var ErrRuleNotFound = errors.New("alerts: the rule does not exist")

// ErrEventNotFound reports an event that the engine does not know.
var ErrEventNotFound = errors.New("alerts: the event does not exist")

// DefaultSampleInterval is the interval at which an agent samples a
// container. A gap longer than two intervals interrupts the continuity of
// the observation.
const DefaultSampleInterval = 10 * time.Second

// Rule is the definition of an alert.
//
// Kind names what the rule watches, ScopeID the object it watches (a node, a
// container or an application), Metric the value of a resource rule and
// Threshold its limit. For is the time the condition has to hold before the
// rule opens or closes an event.
type Rule struct {
	// ID is the identifier of the rule.
	ID string `json:"id"`
	// Name is the title the administrator reads. A rule without a name
	// falls back to the label of its kind in the console.
	Name string `json:"name"`
	// Description is the sentence the administrator wrote for the rule.
	// It is empty when the administrator wrote none.
	Description string `json:"description"`
	// Kind is the kind of the observation.
	Kind string `json:"kind"`
	// Severity is the weight of the rule.
	Severity string `json:"severity"`
	// ScopeKind names the breadth the rule watches.
	ScopeKind string `json:"scope_kind"`
	// ScopeID is the object the rule watches: a container, a namespace or
	// a node. It is empty for a rule of all containers.
	ScopeID string `json:"scope_id"`
	// Metric is the value of a resource rule.
	Metric string `json:"metric"`
	// Operator compares the observed value with the threshold.
	Operator string `json:"operator"`
	// Threshold is the limit of a resource rule.
	Threshold float64 `json:"threshold"`
	// For is the time the condition has to hold.
	For time.Duration `json:"-"`
	// ForSeconds is the duration of For in seconds, for the API.
	ForSeconds int `json:"for_seconds"`
	// Enabled reports whether the rule is evaluated.
	Enabled bool `json:"enabled"`
	// RecoverQuiet silences the recovery of the rule. The opening of an
	// event always notifies; a rule whose recovery is quiet only resolves
	// its event without a word. The field is an opt in to the silence, so
	// a rule that nobody configured keeps a recovery that speaks.
	RecoverQuiet bool `json:"-"`
}

// Observation is one sample of the object a rule watches.
//
// Valid reports whether the sample describes an observed interval. An
// invalid observation is a gap of the data: it never opens, continues or
// closes an event. ExpectedAction reports an end of a container that a known
// action explains, so the sample is not an anomaly.
type Observation struct {
	// ObjectID is the object the sample belongs to.
	ObjectID string
	// At is the time of the sample.
	At time.Time
	// Value is the observed value. A resource rule compares it with its
	// threshold, the other rules treat a non zero value as an anomaly.
	Value float64
	// Valid reports whether the sample describes an observed interval.
	Valid bool
	// ExpectedAction reports an end that a known action explains.
	ExpectedAction bool
}

// Event is one alert of one object.
type Event struct {
	// ID is the identifier of the event.
	ID string `json:"id"`
	// RuleID is the rule that opened the event.
	RuleID string `json:"rule_id"`
	// ObjectID is the object of the event.
	ObjectID string `json:"object_id"`
	// State is the state of the event.
	State string `json:"state"`
	// Summary describes the event.
	Summary string `json:"summary"`
	// StartedAt is the time the event opened.
	StartedAt time.Time `json:"started_at"`
	// ResolvedAt is the time the event closed, nil while it is active.
	ResolvedAt *time.Time `json:"resolved_at"`
	// AcknowledgedAt is the time the administrator acknowledged the event.
	AcknowledgedAt *time.Time `json:"acknowledged_at"`
}

// Notification is the record of one transition of an event, for the sender
// of the notifications.
type Notification struct {
	// ID is the identifier of the notification.
	ID string `json:"id"`
	// EventID is the event the notification belongs to.
	EventID string `json:"event_id"`
	// Transition is the number of the transition of the event. The
	// opening of an event and its recovery are two transitions, so a
	// receiver that deduplicates by the event alone would drop the
	// recovery.
	Transition int64 `json:"transition"`
	// State is the state the event reached.
	State string `json:"state"`
	// OccurredAt is the time of the transition.
	OccurredAt time.Time `json:"occurred_at"`
	// Summary describes the event.
	Summary string `json:"summary"`
	// Environment names the target of the event.
	Environment string `json:"environment"`
	// Object is the object of the event.
	Object string `json:"object"`
}

// NewRuleID makes the identifier of a new rule.
func NewRuleID() string { return "rule-" + randomID() }

// Engine evaluates the observations against the rules and keeps the events.
type Engine struct {
	// db is the write pool of the control端.
	db *sql.DB
	// now reads the clock; the tests replace it.
	now func() time.Time
	// outbox is the queue of the notifications of the events.
	outbox *Outbox
}

// New opens the engine on the database.
//
// Every pending continuity is cleared: a restart of the control端 cannot
// prove that a condition held while the process was down, so the continuity
// has to be proven again by fresh samples. An event that was already open
// stays open.
func New(db *sql.DB) *Engine {
	e := &Engine{db: db, now: func() time.Time { return time.Now().UTC() }}
	e.outbox = NewOutbox(db)
	e.outbox.UseClock(e.now)
	e.clearContinuity(context.Background())
	return e
}

// UseClock replaces the clock of the engine. The tests use it to drive the
// evaluation without waiting.
func (e *Engine) UseClock(now func() time.Time) {
	e.now = now
	e.outbox.UseClock(now)
}

// PendingNotifications returns the notifications that wait for their
// delivery, the oldest first.
//
// The sender of the notifications uses it to find the work that waits.
func (e *Engine) PendingNotifications(ctx context.Context, limit int) ([]Notification, error) {
	return e.outbox.PendingNotifications(ctx, limit)
}

// Counts returns the state of the queue of the notifications.
func (e *Engine) Counts(ctx context.Context) (Counts, error) {
	return e.outbox.Counts(ctx)
}

// clearContinuity removes the pending starts of every condition.
func (e *Engine) clearContinuity(ctx context.Context) {
	if _, err := e.db.ExecContext(ctx,
		`UPDATE alert_conditions SET pending_since = 0, recovering_since = 0`); err != nil {
		// The engine still evaluates; the continuity of the first samples
		// after the restart is then proven from scratch.
		return
	}
}

// PutRule writes a rule.
func (e *Engine) PutRule(ctx context.Context, rule Rule) error {
	if rule.ID == "" {
		return errors.New("alerts: the rule has no identifier")
	}
	if !knownKind(rule.Kind) {
		return fmt.Errorf("alerts: the rule %s has an unknown kind %q", rule.ID, rule.Kind)
	}
	if rule.Severity == "" {
		rule.Severity = SeverityWarning
	}
	if !knownSeverity(rule.Severity) {
		return fmt.Errorf("alerts: the rule %s has an unknown severity %q", rule.ID, rule.Severity)
	}
	if rule.ScopeKind == "" {
		rule.ScopeKind = ScopeContainer
	}
	if !knownScopeKind(rule.ScopeKind) {
		return fmt.Errorf("alerts: the rule %s has an unknown scope %q", rule.ID, rule.ScopeKind)
	}
	if rule.Operator == "" {
		rule.Operator = OperatorGreaterThan
	}
	if !knownOperator(rule.Operator) {
		return fmt.Errorf("alerts: the rule %s has an unknown operator %q", rule.ID, rule.Operator)
	}
	now := store.TimeToMillis(e.now())
	enabled := 0
	if rule.Enabled {
		enabled = 1
	}
	recoverNotify := 1
	if rule.RecoverQuiet {
		recoverNotify = 0
	}
	if _, err := e.db.ExecContext(ctx, `
		INSERT INTO alert_rules (id, name, description, kind, severity, scope_kind, scope_id, metric, operator, threshold, for_ms, enabled, recover_notify, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
		    name = excluded.name,
		    description = excluded.description,
		    kind = excluded.kind,
		    severity = excluded.severity,
		    scope_kind = excluded.scope_kind,
		    scope_id = excluded.scope_id,
		    metric = excluded.metric,
		    operator = excluded.operator,
		    threshold = excluded.threshold,
		    for_ms = excluded.for_ms,
		    enabled = excluded.enabled,
		    recover_notify = excluded.recover_notify,
		    updated_at = excluded.updated_at`,
		rule.ID, rule.Name, rule.Description, rule.Kind, rule.Severity, rule.ScopeKind,
		rule.ScopeID, rule.Metric, rule.Operator, rule.Threshold, rule.For.Milliseconds(),
		enabled, recoverNotify, now, now); err != nil {
		return fmt.Errorf("alerts: write the rule %s: %w", rule.ID, err)
	}
	return nil
}

// knownKind reports whether the kind is one the engine evaluates.
func knownKind(kind string) bool {
	switch kind {
	case KindContainerExit, KindAgentOffline, KindResource:
		return true
	}
	return false
}

// knownSeverity reports whether the severity is one the console understands.
func knownSeverity(severity string) bool {
	switch severity {
	case SeverityInfo, SeverityWarning, SeverityCritical:
		return true
	}
	return false
}

// knownScopeKind reports whether the scope is one the engine matches.
func knownScopeKind(scopeKind string) bool {
	switch scopeKind {
	case ScopeAll, ScopeContainer, ScopeNamespace:
		return true
	}
	return false
}

// knownOperator reports whether the operator is one the engine compares with.
func knownOperator(operator string) bool {
	switch operator {
	case OperatorGreaterThan, OperatorGreaterOrEqual, OperatorLessThan, OperatorLessOrEqual:
		return true
	}
	return false
}

// DeleteRule removes a rule.
//
// The events of the rule stay in the history: the administrator still reads
// what happened. A deleted rule opens no event and sends no notification.
func (e *Engine) DeleteRule(ctx context.Context, ruleID string) error {
	if _, err := e.db.ExecContext(ctx, `DELETE FROM alert_rules WHERE id = ?`, ruleID); err != nil {
		return fmt.Errorf("alerts: delete the rule %s: %w", ruleID, err)
	}
	if _, err := e.db.ExecContext(ctx, `DELETE FROM alert_conditions WHERE rule_id = ?`, ruleID); err != nil {
		return fmt.Errorf("alerts: delete the condition of the rule %s: %w", ruleID, err)
	}
	return nil
}

// Rules lists the rules.
func (e *Engine) Rules(ctx context.Context) ([]Rule, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT id, name, description, kind, severity, scope_kind, scope_id, metric, operator, threshold, for_ms, enabled, recover_notify
		  FROM alert_rules
		 ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("alerts: list the rules: %w", err)
	}
	defer rows.Close()

	var rules []Rule
	for rows.Next() {
		var rule Rule
		var forMS int64
		var enabled, recoverNotify int
		if err := rows.Scan(&rule.ID, &rule.Name, &rule.Description, &rule.Kind, &rule.Severity,
			&rule.ScopeKind, &rule.ScopeID, &rule.Metric, &rule.Operator, &rule.Threshold,
			&forMS, &enabled, &recoverNotify); err != nil {
			return nil, fmt.Errorf("alerts: read a rule: %w", err)
		}
		rule.For = time.Duration(forMS) * time.Millisecond
		rule.ForSeconds = int(rule.For / time.Second)
		rule.Enabled = enabled != 0
		rule.RecoverQuiet = recoverNotify == 0
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alerts: read the rules: %w", err)
	}
	return rules, nil
}

// Events lists the events, the newest first.
//
// activeOnly keeps the events that are still open.
func (e *Engine) Events(ctx context.Context, activeOnly bool) ([]Event, error) {
	query := `
		SELECT id, rule_id, object_id, state, summary, started_at, resolved_at, acknowledged_at
		  FROM alert_events`
	if activeOnly {
		query += ` WHERE state = 'active'`
	}
	query += ` ORDER BY started_at DESC, id`

	rows, err := e.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("alerts: list the events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alerts: read the events: %w", err)
	}
	return events, nil
}

// scanner is the part of the row types the event scan needs.
type scanner interface {
	Scan(dest ...any) error
}

// scanEvent reads one event of a row.
func scanEvent(row scanner) (Event, error) {
	var event Event
	var startedAt int64
	var resolvedAt, acknowledgedAt sql.NullInt64
	if err := row.Scan(&event.ID, &event.RuleID, &event.ObjectID, &event.State,
		&event.Summary, &startedAt, &resolvedAt, &acknowledgedAt); err != nil {
		return Event{}, fmt.Errorf("alerts: read an event: %w", err)
	}
	event.StartedAt = store.TimeFromMillis(startedAt)
	if resolvedAt.Valid {
		at := store.TimeFromMillis(resolvedAt.Int64)
		event.ResolvedAt = &at
	}
	if acknowledgedAt.Valid {
		at := store.TimeFromMillis(acknowledgedAt.Int64)
		event.AcknowledgedAt = &at
	}
	return event, nil
}

// Acknowledge marks an event as seen by the administrator.
//
// Acknowledging is not a recovery: the event keeps its state and its
// resolved_at stays empty until the observation shows that the anomaly is
// over.
func (e *Engine) Acknowledge(ctx context.Context, eventID string, at time.Time) error {
	result, err := e.db.ExecContext(ctx,
		`UPDATE alert_events SET acknowledged_at = ? WHERE id = ?`,
		store.TimeToMillis(at), eventID)
	if err != nil {
		return fmt.Errorf("alerts: acknowledge the event %s: %w", eventID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("alerts: acknowledge the event %s: %w", eventID, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s", ErrEventNotFound, eventID)
	}
	return nil
}
