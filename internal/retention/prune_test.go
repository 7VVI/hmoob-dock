package retention_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"hmoob-dock/internal/retention"
	"hmoob-dock/internal/store"
)

// newDB opens a database with the schema of the control端.
func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// count reads the number of the rows of a table.
func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var value int
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&value); err != nil {
		t.Fatalf("count the rows of %s: %v", table, err)
	}
	return value
}

// insertApp writes an application and returns its identifier.
func insertApp(t *testing.T, db *sql.DB, id string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO apps (id, name, mode, target_id, namespace, created_at)
		VALUES (?, ?, 'compose', 'node-1', ?, ?)`, id, id, id, at.UnixMilli()); err != nil {
		t.Fatalf("insert the application %s: %v", id, err)
	}
}

// insertVersion writes a version of an application.
func insertVersion(t *testing.T, db *sql.DB, id, appID string, at time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO versions (id, app_id, digest, yaml_ciphertext, variables_ciphertext, created_at)
		VALUES (?, ?, 'digest', x'00', x'00', ?)`, id, appID, at.UnixMilli()); err != nil {
		t.Fatalf("insert the version %s: %v", id, err)
	}
}

// insertTask writes a task of an application.
func insertTask(t *testing.T, db *sql.DB, id, appID, versionID, state string, finishedAt *time.Time) {
	t.Helper()
	var finished any
	if finishedAt != nil {
		finished = finishedAt.UnixMilli()
	}
	if _, err := db.Exec(`INSERT INTO tasks
		    (id, app_id, version_id, state, digest, idempotency_key, request_digest, created_at, updated_at, finished_at)
		VALUES (?, ?, ?, ?, 'digest', ?, 'request', ?, ?, ?)`,
		id, appID, versionID, state, id, time.Now().UnixMilli(), time.Now().UnixMilli(), finished); err != nil {
		t.Fatalf("insert the task %s: %v", id, err)
	}
}

// TestTheMinuteAggregatesPassTheirLimit proves that the history keeps the
// last 24 hours and removes the rest.
func TestTheMinuteAggregatesPassTheirLimit(t *testing.T) {
	db := newDB(t)
	now := time.Unix(1800000000, 0).UTC()
	for _, at := range []time.Time{now.Add(-25 * time.Hour), now.Add(-23 * time.Hour)} {
		if _, err := db.Exec(`INSERT INTO metric_minutes
			    (node_id, container_id, bucket_start, sample_count, expected_count, average_json, peak_json)
			VALUES ('n1', 'c1', ?, 6, 6, '{}', '{}')`, at.UnixMilli()); err != nil {
			t.Fatalf("insert a minute: %v", err)
		}
	}
	counts, err := retention.New(db).RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("run the cleaner: %v", err)
	}
	if counts.Metrics != 1 {
		t.Fatalf("removed %d minutes, want 1", counts.Metrics)
	}
	if kept := count(t, db, "metric_minutes"); kept != 1 {
		t.Fatalf("kept %d minutes, want 1", kept)
	}
}

// TestTheFinishedTasksPassTheirLimit proves that a finished task leaves with
// its output while an unfinished one stays.
func TestTheFinishedTasksPassTheirLimit(t *testing.T) {
	db := newDB(t)
	now := time.Unix(1800000000, 0).UTC()
	insertApp(t, db, "app-1", now)
	insertVersion(t, db, "version-1", "app-1", now)

	old := now.Add(-91 * retention.Day)
	insertTask(t, db, "task-old", "app-1", "version-1", "succeeded", &old)
	insertTask(t, db, "task-new", "app-1", "version-1", "failed", nil)
	if _, err := db.Exec(`INSERT INTO task_output (task_id, offset, data, created_at) VALUES ('task-old', 0, x'00', ?)`, old.UnixMilli()); err != nil {
		t.Fatalf("insert the output: %v", err)
	}

	if _, err := retention.New(db).RunOnce(context.Background(), now); err != nil {
		t.Fatalf("run the cleaner: %v", err)
	}
	if kept := count(t, db, "tasks"); kept != 1 {
		t.Fatalf("kept %d tasks, want 1", kept)
	}
	if kept := count(t, db, "task_output"); kept != 0 {
		t.Fatalf("kept %d outputs, want 0", kept)
	}
}

// TestTheVersionsKeepTheOnesTheApplicationNeeds proves that the window of
// the versions never removes the version an application deploys, the last
// successful one, or the one a retained task used.
func TestTheVersionsKeepTheOnesTheApplicationNeeds(t *testing.T) {
	db := newDB(t)
	now := time.Unix(1800000000, 0).UTC()
	insertApp(t, db, "app-1", now)

	// Sixty versions, the oldest ones outside the window of fifty.
	for index := 0; index < 60; index++ {
		insertVersion(t, db, versionID(index), "app-1", now.Add(time.Duration(index-60)*time.Minute))
	}
	// The application deploys the oldest version and the last successful
	// one is the second oldest: both are outside the window.
	if _, err := db.Exec(`UPDATE apps SET latest_version_id = ?, successful_version_id = ? WHERE id = 'app-1'`,
		versionID(0), versionID(1)); err != nil {
		t.Fatalf("point the application at its versions: %v", err)
	}
	// A task still refers to the third oldest version.
	insertTask(t, db, "task-1", "app-1", versionID(2), "running", nil)

	if _, err := retention.New(db).RunOnce(context.Background(), now); err != nil {
		t.Fatalf("run the cleaner: %v", err)
	}
	if kept := count(t, db, "versions"); kept != 53 {
		t.Fatalf("kept %d versions, want 53 (50 + the deployed, the successful and the used one)", kept)
	}
	for _, id := range []string{versionID(0), versionID(1), versionID(2), versionID(59)} {
		var found int
		if err := db.QueryRow(`SELECT count(*) FROM versions WHERE id = ?`, id).Scan(&found); err != nil {
			t.Fatalf("read the version %s: %v", id, err)
		}
		if found != 1 {
			t.Fatalf("the version %s was removed", id)
		}
	}
}

// versionID names the version of a test. The zero padding keeps the order of
// the names the same as the order of the creation times.
func versionID(index int) string {
	return "version-" + string(rune('a'+index/26)) + string(rune('a'+index%26))
}

// TestTheAlertsKeepTheActiveOnes proves that a resolved event passes its
// limit while an active one stays.
func TestTheAlertsKeepTheActiveOnes(t *testing.T) {
	db := newDB(t)
	now := time.Unix(1800000000, 0).UTC()
	old := now.Add(-91 * retention.Day).UnixMilli()
	if _, err := db.Exec(`INSERT INTO alert_events (id, rule_id, object_id, state, summary, started_at, last_observed_at, resolved_at)
		VALUES ('resolved-1', 'r1', 'c1', 'resolved', '对象 c1 已恢复正常', ?, ?, ?)`,
		old, old, old); err != nil {
		t.Fatalf("insert the resolved event: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO alert_events (id, rule_id, object_id, state, summary, started_at, last_observed_at)
		VALUES ('active-1', 'r1', 'c2', 'active', '对象 c2 意外退出', ?, ?)`,
		old, old); err != nil {
		t.Fatalf("insert the active event: %v", err)
	}

	if _, err := retention.New(db).RunOnce(context.Background(), now); err != nil {
		t.Fatalf("run the cleaner: %v", err)
	}
	if kept := count(t, db, "alert_events"); kept != 1 {
		t.Fatalf("kept %d events, want 1", kept)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM alert_events`).Scan(&state); err != nil {
		t.Fatalf("read the event: %v", err)
	}
	if state != "active" {
		t.Fatalf("the active event was removed")
	}
}

// TestTheSessionsThatEndedAreRemoved proves that an expired or revoked
// session leaves while a live one stays.
func TestTheSessionsThatEndedAreRemoved(t *testing.T) {
	db := newDB(t)
	now := time.Unix(1800000000, 0).UTC()
	old := now.Add(-2 * retention.Day).UnixMilli()
	insert := func(id string, expiresAt int64, revoked any) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO sessions (id, token_hash, csrf_hash, created_at, expires_at, revoked_at)
			VALUES (?, ?, 'csrf', ?, ?, ?)`, id, id, old, expiresAt, revoked); err != nil {
			t.Fatalf("insert the session %s: %v", id, err)
		}
	}
	insert("expired", old, nil)
	insert("revoked", now.Add(24*time.Hour).UnixMilli(), old)
	insert("live", now.Add(24*time.Hour).UnixMilli(), nil)

	if _, err := retention.New(db).RunOnce(context.Background(), now); err != nil {
		t.Fatalf("run the cleaner: %v", err)
	}
	if kept := count(t, db, "sessions"); kept != 1 {
		t.Fatalf("kept %d sessions, want 1", kept)
	}
}

// TestThePendingNotificationsAreKept proves that a notification that was
// never delivered stays, because the sender still owes it.
func TestThePendingNotificationsAreKept(t *testing.T) {
	db := newDB(t)
	now := time.Unix(1800000000, 0).UTC()
	old := now.Add(-91 * retention.Day).UnixMilli()
	for index, state := range []string{"pending", "delivered"} {
		if _, err := db.Exec(`INSERT INTO notification_outbox
			    (id, event_id, transition_seq, state, occurred_at, summary, object, delivery_state)
			VALUES (?, 'event-1', ?, 'active', ?, 'summary', 'c1', ?)`,
			state, index+1, old, state); err != nil {
			t.Fatalf("insert the %s notification: %v", state, err)
		}
	}

	if _, err := retention.New(db).RunOnce(context.Background(), now); err != nil {
		t.Fatalf("run the cleaner: %v", err)
	}
	if kept := count(t, db, "notification_outbox"); kept != 1 {
		t.Fatalf("kept %d notifications, want 1", kept)
	}
}
