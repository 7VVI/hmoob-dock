package metrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"hmoob-dock/internal/store"
)

// The limits of a write into the history.
const (
	// maxWriteBatch is the largest number of the buckets one transaction
	// holds. A transaction that is too large holds the single write
	// connection of the control端 for too long and delays the deployments.
	maxWriteBatch = 100
	// writeTimeout bounds one batch. A database that is busy with a
	// deployment must not hold the metrics request: the batch fails and
	// the missing minutes become visible as a gap of the history.
	writeTimeout = 500 * time.Millisecond
)

// The errors of the history store.
var (
	// ErrInvalidBucket reports a bucket that cannot be stored.
	ErrInvalidBucket = errors.New("metrics: the bucket is invalid")
	// ErrInvalidRange reports a query without a usable time range.
	ErrInvalidRange = errors.New("metrics: the time range is invalid")
	// ErrNoDatabase reports a store without a database handle.
	ErrNoDatabase = errors.New("metrics: a database handle is required")
)

// Store reads and writes the minute aggregates in SQLite.
//
// The store never queries the rows of one statement while it iterates
// another one. The write pool of the control端 holds a single connection, so
// a nested query would wait for the connection that the first query holds.
type Store struct {
	db *sql.DB
}

// NewStore builds the history store on top of the given database handle.
//
// The caller should hand the write pool to the writer and the read-only pool
// to the reader, so a query never waits for a deployment write. The store
// itself never writes and reads at the same time on one handle.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// encodedBucket is a bucket whose values are already JSON documents.
type encodedBucket struct {
	nodeID        string
	containerID   string
	bucketStart   int64
	sampleCount   int
	expectedCount int
	average       string
	peak          string
}

// Save writes the buckets into the history in batches of at most 100 rows.
//
// Every batch runs in its own transaction with a deadline of 500ms, so a
// busy database refuses the write instead of blocking the deployments and
// the other requests of the control端. The primary key of the table makes the
// write an idempotent upsert: a replayed batch replaces the rows of its
// minutes and never counts the samples twice.
func (s *Store) Save(ctx context.Context, buckets []Bucket) error {
	if len(buckets) == 0 {
		return nil
	}
	if s == nil || s.db == nil {
		return ErrNoDatabase
	}

	// The values are encoded before the first transaction starts, so a
	// bucket that cannot be encoded does not leave a partially written
	// batch behind.
	encoded := make([]encodedBucket, 0, len(buckets))
	for _, bucket := range buckets {
		row, err := encodeBucket(bucket)
		if err != nil {
			return err
		}
		encoded = append(encoded, row)
	}

	for start := 0; start < len(encoded); start += maxWriteBatch {
		end := start + maxWriteBatch
		if end > len(encoded) {
			end = len(encoded)
		}
		if err := s.writeBatch(ctx, encoded[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// encodeBucket validates a bucket and encodes its values.
func encodeBucket(bucket Bucket) (encodedBucket, error) {
	if strings.TrimSpace(bucket.NodeID) == "" || strings.TrimSpace(bucket.ContainerID) == "" {
		return encodedBucket{}, fmt.Errorf("%w: the node and the container are required", ErrInvalidBucket)
	}
	if bucket.Start.IsZero() {
		return encodedBucket{}, fmt.Errorf("%w: the start of the minute is required", ErrInvalidBucket)
	}
	start := bucket.Start.UTC()
	if !start.Equal(start.Truncate(time.Minute)) {
		return encodedBucket{}, fmt.Errorf("%w: the start %s is not a minute boundary", ErrInvalidBucket, start)
	}
	if bucket.SampleCount <= 0 {
		return encodedBucket{}, fmt.Errorf("%w: a bucket without a sample is a gap and not a row", ErrInvalidBucket)
	}
	if bucket.ExpectedCount <= 0 {
		return encodedBucket{}, fmt.Errorf("%w: the expected sample count is required", ErrInvalidBucket)
	}

	average, err := json.Marshal(bucket.Average)
	if err != nil {
		return encodedBucket{}, fmt.Errorf("%w: encode the average: %v", ErrInvalidBucket, err)
	}
	peak, err := json.Marshal(bucket.Peak)
	if err != nil {
		return encodedBucket{}, fmt.Errorf("%w: encode the peak: %v", ErrInvalidBucket, err)
	}
	return encodedBucket{
		nodeID:        bucket.NodeID,
		containerID:   bucket.ContainerID,
		bucketStart:   store.TimeToMillis(start),
		sampleCount:   bucket.SampleCount,
		expectedCount: bucket.ExpectedCount,
		average:       string(average),
		peak:          string(peak),
	}, nil
}

// upsertBucket writes one bucket or replaces the row of its minute when the
// batch is replayed.
const upsertBucket = `
INSERT INTO metric_minutes
    (node_id, container_id, bucket_start, sample_count, expected_count, average_json, peak_json)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (node_id, container_id, bucket_start) DO UPDATE SET
    sample_count = excluded.sample_count,
    expected_count = excluded.expected_count,
    average_json = excluded.average_json,
    peak_json = excluded.peak_json`

// writeBatch writes one bounded batch in its own transaction.
func (s *Store) writeBatch(parent context.Context, rows []encodedBucket) error {
	ctx, cancel := context.WithTimeout(parent, writeTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metrics: start the batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statement, err := tx.PrepareContext(ctx, upsertBucket)
	if err != nil {
		return fmt.Errorf("metrics: prepare the batch: %w", err)
	}
	for _, row := range rows {
		if _, err := statement.ExecContext(ctx,
			row.nodeID, row.containerID, row.bucketStart, row.sampleCount,
			row.expectedCount, row.average, row.peak); err != nil {
			_ = statement.Close()
			return fmt.Errorf("metrics: write the bucket of %s/%s: %w", row.nodeID, row.containerID, err)
		}
	}
	if err := statement.Close(); err != nil {
		return fmt.Errorf("metrics: close the batch statement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metrics: commit the batch: %w", err)
	}
	return nil
}

// Query returns the buckets of one container in the half-open interval
// [from, to), ordered by their minute.
//
// The query returns only the minutes that were observed. A minute without a
// row is a gap; the query does not invent a zero row for it, because a
// reader must not show a missing observation as an idle container.
func (s *Store) Query(ctx context.Context, nodeID, containerID string, from, to time.Time) ([]Bucket, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoDatabase
	}
	nodeID = strings.TrimSpace(nodeID)
	containerID = strings.TrimSpace(containerID)
	if nodeID == "" || containerID == "" {
		return nil, fmt.Errorf("%w: the node and the container are required", ErrInvalidRange)
	}
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return nil, fmt.Errorf("%w: the end has to be after the start", ErrInvalidRange)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT bucket_start, sample_count, expected_count, average_json, peak_json
		 FROM metric_minutes
		 WHERE node_id = ? AND container_id = ? AND bucket_start >= ? AND bucket_start < ?
		 ORDER BY bucket_start`,
		nodeID, containerID, store.TimeToMillis(from), store.TimeToMillis(to))
	if err != nil {
		return nil, fmt.Errorf("metrics: query the history: %w", err)
	}
	defer rows.Close()

	buckets := make([]Bucket, 0)
	for rows.Next() {
		var startMillis int64
		var sampleCount, expectedCount int
		var averageJSON, peakJSON string
		if err := rows.Scan(&startMillis, &sampleCount, &expectedCount, &averageJSON, &peakJSON); err != nil {
			return nil, fmt.Errorf("metrics: scan the history: %w", err)
		}
		var average, peak Values
		if err := json.Unmarshal([]byte(averageJSON), &average); err != nil {
			return nil, fmt.Errorf("metrics: decode the average of %s/%s: %w", nodeID, containerID, err)
		}
		if err := json.Unmarshal([]byte(peakJSON), &peak); err != nil {
			return nil, fmt.Errorf("metrics: decode the peak of %s/%s: %w", nodeID, containerID, err)
		}
		buckets = append(buckets, Bucket{
			NodeID:        nodeID,
			ContainerID:   containerID,
			Start:         store.TimeFromMillis(startMillis),
			Average:       average,
			Peak:          peak,
			SampleCount:   sampleCount,
			ExpectedCount: expectedCount,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metrics: read the history: %w", err)
	}
	return buckets, nil
}

// HasHistory reports whether the database holds at least one bucket of the
// container of the node.
//
// The history outlives the container: a container that was removed has no
// recent inventory entry any more, but its minutes stay readable. The API
// uses the check to keep the history of a node readable while the node is
// away, without telling whether another node's containers exist.
func (s *Store) HasHistory(ctx context.Context, nodeID, containerID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, ErrNoDatabase
	}
	nodeID = strings.TrimSpace(nodeID)
	containerID = strings.TrimSpace(containerID)
	if nodeID == "" || containerID == "" {
		return false, nil
	}

	var found int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM metric_minutes WHERE node_id = ? AND container_id = ? LIMIT 1`,
		nodeID, containerID).Scan(&found)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("metrics: read the history marker: %w", err)
	}
	return true, nil
}
