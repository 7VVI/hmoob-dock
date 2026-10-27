package alerts

import (
	"context"
	"fmt"
	"time"

	"hmoob-dock/internal/store"
)

// ResourceSample is one value of one metric of one object.
//
// The sample is the input of a resource rule: the rule names the metric and
// the limit, the sample carries the value. An invalid sample is a gap of the
// data and never opens, continues or closes an event.
type ResourceSample struct {
	// ObjectID is the object the sample belongs to.
	ObjectID string
	// At is the time of the sample.
	At time.Time
	// Metric is the name of the measured value.
	Metric string
	// Value is the measured value.
	Value float64
	// Valid reports whether the sample describes an observed interval.
	Valid bool
}

// ObserveResource evaluates one sample against the resource rules.
//
// The rules of the other kinds are not evaluated: an exit and a heartbeat
// have their own sources.
func (e *Engine) ObserveResource(ctx context.Context, sample ResourceSample) ([]Event, error) {
	rules, err := e.Rules(ctx)
	if err != nil {
		return nil, err
	}
	var opened []Event
	for _, rule := range rules {
		if rule.Kind != KindResource || !rule.Enabled {
			continue
		}
		if rule.ScopeID != sample.ObjectID || rule.Metric != sample.Metric {
			continue
		}
		event, err := e.Observe(ctx, rule.ID, Observation{
			ObjectID: sample.ObjectID,
			At:       sample.At,
			Value:    sample.Value,
			Valid:    sample.Valid,
		})
		if err != nil {
			return opened, err
		}
		if event != nil {
			opened = append(opened, *event)
		}
	}
	return opened, nil
}

// OfflineSample is the observation of a node that stopped sending
// heartbeats.
//
// The time is the time the control端 received the last heartbeat, not the
// time the agent claims: a node with a wrong clock is not offline, and a
// node that is offline does not report it.
type OfflineSample struct {
	// NodeID is the node the sample belongs to.
	NodeID string
	// At is the time the control端 received the last heartbeat.
	At time.Time
	// Offline reports whether the node stayed away for too long.
	Offline bool
}

// ObserveOffline evaluates the heartbeat of a node against the rules of the
// nodes.
func (e *Engine) ObserveOffline(ctx context.Context, sample OfflineSample) ([]Event, error) {
	rules, err := e.Rules(ctx)
	if err != nil {
		return nil, err
	}
	var opened []Event
	for _, rule := range rules {
		if rule.Kind != KindAgentOffline || !rule.Enabled {
			continue
		}
		if rule.ScopeID != "" && rule.ScopeID != sample.NodeID {
			continue
		}
		value := 0.0
		if sample.Offline {
			value = 1
		}
		event, err := e.Observe(ctx, rule.ID, Observation{
			ObjectID: sample.NodeID,
			At:       sample.At,
			Value:    value,
			Valid:    true,
		})
		if err != nil {
			return opened, err
		}
		if event != nil {
			opened = append(opened, *event)
		}
	}
	return opened, nil
}

// ExitSample is the observation of a container that stopped.
type ExitSample struct {
	// NodeID is the node the container belongs to.
	NodeID string
	// ObjectID is the object the container belongs to.
	ObjectID string
	// At is the time Docker reported the end.
	At time.Time
	// ExitCode is the exit code of the container.
	ExitCode int
	// Expected reports whether a known action explains the end.
	Expected bool
}

// ObserveExit evaluates the end of a container against the rules of the
// exits.
//
// An end that a known action explains is not an anomaly. The event of an
// exit that is already open is closed at once, because the administrator
// knows that the container is gone on purpose.
func (e *Engine) ObserveExit(ctx context.Context, sample ExitSample) ([]Event, error) {
	rules, err := e.Rules(ctx)
	if err != nil {
		return nil, err
	}
	var opened []Event
	for _, rule := range rules {
		if rule.Kind != KindContainerExit || !rule.Enabled {
			continue
		}
		if rule.ScopeID != "" && rule.ScopeID != sample.ObjectID {
			continue
		}
		value := 1.0
		if sample.Expected {
			value = 0
		}
		event, err := e.Observe(ctx, rule.ID, Observation{
			ObjectID:       sample.ObjectID,
			At:             sample.At,
			Value:          value,
			Valid:          true,
			ExpectedAction: sample.Expected,
		})
		if err != nil {
			return opened, err
		}
		if event != nil {
			opened = append(opened, *event)
		}
	}
	return opened, nil
}

// NotificationOf reads the notification of one transition of an event.
//
// The sender of the notifications uses it to find the work that waits.
func (e *Engine) NotificationOf(ctx context.Context, eventID string, transition int64) (Notification, error) {
	var notification Notification
	var occurredAt int64
	err := e.db.QueryRowContext(ctx, `
		SELECT id, event_id, transition_seq, state, occurred_at, summary, environment, object
		  FROM notification_outbox
		 WHERE event_id = ? AND transition_seq = ?`, eventID, transition).
		Scan(&notification.ID, &notification.EventID, &notification.Transition,
			&notification.State, &occurredAt, &notification.Summary,
			&notification.Environment, &notification.Object)
	if err != nil {
		return Notification{}, fmt.Errorf("alerts: read the notification of %s: %w", eventID, err)
	}
	notification.OccurredAt = store.TimeFromMillis(occurredAt)
	return notification, nil
}
