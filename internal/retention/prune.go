// Package retention removes the records the control端 does not need any
// more.
//
// Every limit is a bound, not a promise of a scheduler: the cleaner runs in
// short batches, so a busy database delays a batch instead of holding a long
// write transaction. It never removes a record that a task still refers to,
// an event that is still active, or a version that the application deploys
// or that a retained task used.
package retention

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"hmoob-dock/internal/store"
)

// Day is the length of a day of the history.
const Day = 24 * time.Hour

// The limits of the history.
const (
	// MetricsRetention is the time the minute aggregates are kept.
	MetricsRetention = 24 * time.Hour
	// HistoryRetention is the time the finished deployments, the resolved
	// alerts, the notifications and the audit events are kept.
	HistoryRetention = 90 * Day
	// SessionsRetention is the time a session that ended stays in the table
	// before it is removed.
	SessionsRetention = 24 * time.Hour
	// KeptVersionsPerApp is the number of the newest versions one
	// application keeps. The versions of the application that are still
	// deployed, the last successful one and the ones a retained task refers
	// to are kept even when they fall out of the window.
	KeptVersionsPerApp = 50
	// batchSize bounds one write transaction.
	batchSize = 500
	// batchTimeout bounds one write transaction.
	batchTimeout = 2 * time.Second
)

// Counts reports what one run removed.
type Counts struct {
	// Metrics is the number of the removed minute aggregates.
	Metrics int `json:"metrics"`
	// Deployments is the number of the removed finished tasks.
	Deployments int `json:"deployments"`
	// Versions is the number of the removed versions.
	Versions int `json:"versions"`
	// Alerts is the number of the removed resolved events.
	Alerts int `json:"alerts"`
	// Audit is the number of the removed audit events.
	Audit int `json:"audit"`
	// Notifications is the number of the removed notifications.
	Notifications int `json:"notifications"`
	// Sessions is the number of the removed sessions that ended.
	Sessions int `json:"sessions"`
}

// Cleaner removes the records that passed their limit.
type Cleaner struct {
	// db is the write pool of the control端.
	db *sql.DB
}

// New opens the cleaner.
func New(db *sql.DB) *Cleaner {
	return &Cleaner{db: db}
}

// RunOnce removes the records that passed their limit at the given time.
//
// Every step works in batches of its own transaction, so a busy database
// delays the step instead of holding one long write.
func (c *Cleaner) RunOnce(ctx context.Context, now time.Time) (Counts, error) {
	var counts Counts
	now = now.UTC()

	metricsBefore := store.TimeToMillis(now.Add(-MetricsRetention))
	historyBefore := store.TimeToMillis(now.Add(-HistoryRetention))
	sessionsBefore := store.TimeToMillis(now.Add(-SessionsRetention))

	var err error
	if counts.Metrics, err = c.deleteBatches(ctx,
		`DELETE FROM metric_minutes WHERE rowid IN (
		    SELECT rowid FROM metric_minutes WHERE bucket_start < ? LIMIT ?)`,
		metricsBefore); err != nil {
		return counts, err
	}
	notificationsDeleted, err := c.deleteBatches(ctx,
		`DELETE FROM notification_outbox WHERE rowid IN (
		    SELECT rowid FROM notification_outbox
		     WHERE delivery_state IN ('delivered', 'failed', 'skipped')
		       AND occurred_at < ? LIMIT ?)`,
		historyBefore)
	counts.Notifications += notificationsDeleted
	if err != nil {
		return counts, err
	}
	alertsDeleted, err := c.deleteBatches(ctx,
		`DELETE FROM alert_events WHERE rowid IN (
		    SELECT rowid FROM alert_events
		     WHERE state = 'resolved' AND resolved_at IS NOT NULL AND resolved_at < ? LIMIT ?)`,
		historyBefore)
	counts.Alerts += alertsDeleted
	if err != nil {
		return counts, err
	}
	auditDeleted, err := c.deleteBatches(ctx,
		`DELETE FROM audit_events WHERE rowid IN (
		    SELECT rowid FROM audit_events WHERE occurred_at < ? LIMIT ?)`,
		historyBefore)
	counts.Audit += auditDeleted
	if err != nil {
		return counts, err
	}
	sessionsDeleted, err := c.deleteBatches(ctx,
		`DELETE FROM sessions WHERE rowid IN (
		    SELECT rowid FROM sessions
		     WHERE (expires_at < ? OR revoked_at IS NOT NULL)
		       AND created_at < ? LIMIT ?)`,
		store.TimeToMillis(now), sessionsBefore)
	counts.Sessions += sessionsDeleted
	if err != nil {
		return counts, err
	}

	// The output of a finished task goes first: the foreign key ties it to
	// the task, and a task without its output would be a record that
	// promises a log it cannot show.
	if _, err := c.deleteBatches(ctx,
		`DELETE FROM task_output WHERE rowid IN (
		    SELECT o.rowid FROM task_output o JOIN tasks t ON t.id = o.task_id
		     WHERE t.state IN ('succeeded', 'failed')
		       AND t.finished_at IS NOT NULL AND t.finished_at < ? LIMIT ?)`,
		historyBefore); err != nil {
		return counts, err
	}
	if counts.Deployments, err = c.deleteBatches(ctx,
		`DELETE FROM tasks WHERE rowid IN (
		    SELECT rowid FROM tasks
		     WHERE state IN ('succeeded', 'failed')
		       AND finished_at IS NOT NULL AND finished_at < ? LIMIT ?)`,
		historyBefore); err != nil {
		return counts, err
	}

	if counts.Versions, err = c.pruneVersions(ctx); err != nil {
		return counts, err
	}
	return counts, nil
}

// deleteBatches removes the rows a query selects, one bounded batch at a
// time, until the query finds nothing.
func (c *Cleaner) deleteBatches(ctx context.Context, query string, args ...any) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, nil
		}
		removed, err := c.deleteBatch(ctx, query, args...)
		if err != nil {
			return total, err
		}
		total += removed
		if removed < batchSize {
			return total, nil
		}
	}
}

// deleteBatch runs one bounded batch.
func (c *Cleaner) deleteBatch(ctx context.Context, query string, args ...any) (int, error) {
	deadline, cancel := context.WithTimeout(ctx, batchTimeout)
	defer cancel()
	withLimit := append(append([]any{}, args...), batchSize)
	result, err := c.db.ExecContext(deadline, query, withLimit...)
	if err != nil {
		return 0, fmt.Errorf("retention: remove a batch: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("retention: count a batch: %w", err)
	}
	return int(affected), nil
}

// pruneVersions removes the versions of every application that passed the
// window of the newest ones.
//
// A version is kept when the application deploys it, when it is the last
// successful one, or when a task that is still retained used it. A version
// that a deployment record still names is part of that record: removing it
// would break the history the administrator reads.
func (c *Cleaner) pruneVersions(ctx context.Context) (int, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, latest_version_id, successful_version_id FROM apps`)
	if err != nil {
		return 0, fmt.Errorf("retention: list the applications: %w", err)
	}
	defer rows.Close()

	type application struct {
		id         string
		latest     string
		successful string
	}
	var apps []application
	for rows.Next() {
		var app application
		if err := rows.Scan(&app.id, &app.latest, &app.successful); err != nil {
			return 0, fmt.Errorf("retention: read an application: %w", err)
		}
		apps = append(apps, app)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("retention: read the applications: %w", err)
	}

	removed := 0
	for _, app := range apps {
		if err := ctx.Err(); err != nil {
			return removed, nil
		}
		count, err := c.pruneAppVersions(ctx, app.id, app.latest, app.successful)
		if err != nil {
			return removed, err
		}
		removed += count
	}
	return removed, nil
}

// pruneAppVersions removes the versions of one application that passed the
// window.
func (c *Cleaner) pruneAppVersions(ctx context.Context, appID, latest, successful string) (int, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT v.id
		  FROM versions v
		 WHERE v.app_id = ?
		   AND v.id <> ?
		   AND v.id <> ?
		   AND v.id NOT IN (SELECT version_id FROM tasks WHERE app_id = ?)
		 ORDER BY v.created_at DESC
		 LIMIT -1 OFFSET ?`,
		appID, latest, successful, appID, KeptVersionsPerApp)
	if err != nil {
		return 0, fmt.Errorf("retention: list the versions of %s: %w", appID, err)
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("retention: read a version of %s: %w", appID, err)
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("retention: read the versions of %s: %w", appID, err)
	}
	rows.Close()

	removed := 0
	for _, id := range stale {
		if err := ctx.Err(); err != nil {
			return removed, nil
		}
		result, err := c.db.ExecContext(ctx,
			`DELETE FROM versions WHERE id = ? AND id NOT IN (SELECT version_id FROM tasks)`, id)
		if err != nil {
			return removed, fmt.Errorf("retention: remove the version %s: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return removed, fmt.Errorf("retention: count the removal of %s: %w", id, err)
		}
		removed += int(affected)
	}
	return removed, nil
}
