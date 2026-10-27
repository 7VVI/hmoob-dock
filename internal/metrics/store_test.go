package metrics_test

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"hmoob-dock/internal/metrics"
	"hmoob-dock/internal/store"
)

// historyStart is an exact minute boundary for the history tests.
var historyStart = time.Unix(1800000000, 0).UTC().Truncate(time.Minute)

func testBucket(start time.Time, sampleCount int) metrics.Bucket {
	return metrics.Bucket{
		NodeID:      "n1",
		ContainerID: "c1",
		Start:       start,
		Average: metrics.Values{
			CPUPercent: 12.5, MemoryBytes: 1024, NetRXPerSec: 2.5,
			NetTXPerSec: 3.5, IOReadPerSec: 4.5, IOWritePerSec: 5.5,
		},
		Peak: metrics.Values{
			CPUPercent: 25, MemoryBytes: 2048, NetRXPerSec: 7.5,
			NetTXPerSec: 8.5, IOReadPerSec: 9.5, IOWritePerSec: 10.5,
		},
		SampleCount:   sampleCount,
		ExpectedCount: 6,
	}
}

func openMetricsDB(t *testing.T) (string, *metrics.Store, *sql.DB, func()) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "metrics.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return path, metrics.NewStore(db), db, func() { _ = db.Close() }
}

func TestStoreSavesAndReadsBuckets(t *testing.T) {
	path, writer, _, closeDB := openMetricsDB(t)
	defer closeDB()

	buckets := []metrics.Bucket{
		testBucket(historyStart, 6),
		testBucket(historyStart.Add(time.Minute), 2),
	}
	if err := writer.Save(context.Background(), buckets); err != nil {
		t.Fatalf("Save: %v", err)
	}

	readerDB, err := store.OpenReader(path)
	if err != nil {
		t.Fatalf("store.OpenReader: %v", err)
	}
	defer readerDB.Close()
	reader := metrics.NewStore(readerDB)

	got, err := reader.Query(context.Background(), "n1", "c1", historyStart, historyStart.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("buckets = %d, want 2", len(got))
	}
	for index, want := range buckets {
		if got[index] != want {
			t.Fatalf("bucket %d = %+v, want %+v", index, got[index], want)
		}
	}
}

func TestStoreBatchesMoreThanOneHundredBuckets(t *testing.T) {
	_, writer, _, closeDB := openMetricsDB(t)
	defer closeDB()

	buckets := make([]metrics.Bucket, 0, 105)
	for index := 0; index < 105; index++ {
		buckets = append(buckets, testBucket(historyStart.Add(time.Duration(index)*time.Minute), 6))
	}
	if err := writer.Save(context.Background(), buckets); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := writer.Query(context.Background(), "n1", "c1", historyStart, historyStart.Add(105*time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != len(buckets) {
		t.Fatalf("buckets = %d, want %d", len(got), len(buckets))
	}
}

func TestStoreReplayIsIdempotent(t *testing.T) {
	_, db, _, closeDB := openMetricsDB(t)
	defer closeDB()

	bucket := testBucket(historyStart, 6)
	if err := db.Save(context.Background(), []metrics.Bucket{bucket}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := db.Save(context.Background(), []metrics.Bucket{bucket}); err != nil {
		t.Fatalf("replayed Save: %v", err)
	}

	got, err := db.Query(context.Background(), "n1", "c1", historyStart, historyStart.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("buckets = %d, want 1 after a replay", len(got))
	}
	if got[0].SampleCount != 6 {
		t.Fatalf("sample count = %d, want 6 after a replay", got[0].SampleCount)
	}
}

func TestStoreQueryDoesNotInventMissingMinutes(t *testing.T) {
	_, db, _, closeDB := openMetricsDB(t)
	defer closeDB()

	if err := db.Save(context.Background(), []metrics.Bucket{
		testBucket(historyStart, 6),
		testBucket(historyStart.Add(2*time.Minute), 6),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := db.Query(context.Background(), "n1", "c1", historyStart, historyStart.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("buckets = %d, want only the two observed minutes", len(got))
	}
	if !got[0].Start.Equal(historyStart) || !got[1].Start.Equal(historyStart.Add(2*time.Minute)) {
		t.Fatalf("bucket starts = %v/%v, want a visible gap for the missing minute", got[0].Start, got[1].Start)
	}
}

func TestStoreQueryIsEmptyForAnUnobservedWindow(t *testing.T) {
	_, db, _, closeDB := openMetricsDB(t)
	defer closeDB()

	got, err := db.Query(context.Background(), "n1", "c1", historyStart, historyStart.Add(time.Hour))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("buckets = %d, want 0", len(got))
	}
}

func TestStoreHistorySurvivesAReopen(t *testing.T) {
	path, writer, _, closeWriter := openMetricsDB(t)
	if err := writer.Save(context.Background(), []metrics.Bucket{testBucket(historyStart, 6)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	closeWriter()

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got, err := metrics.NewStore(reopened).Query(context.Background(), "n1", "c1", historyStart, historyStart.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query after reopen: %v", err)
	}
	if len(got) != 1 || got[0].Average.CPUPercent != 12.5 {
		t.Fatalf("history after reopen = %+v, want the saved bucket", got)
	}
}

func TestStoreHasHistoryDistinguishesANodeAndAContainerPair(t *testing.T) {
	_, db, _, closeDB := openMetricsDB(t)
	defer closeDB()

	if err := db.Save(context.Background(), []metrics.Bucket{testBucket(historyStart, 6)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	known, err := db.HasHistory(context.Background(), "n1", "c1")
	if err != nil {
		t.Fatalf("HasHistory: %v", err)
	}
	if !known {
		t.Fatal("HasHistory did not find the saved history")
	}
	for _, pair := range [][2]string{{"n2", "c1"}, {"n1", "c2"}} {
		known, err := db.HasHistory(context.Background(), pair[0], pair[1])
		if err != nil {
			t.Fatalf("HasHistory(%s, %s): %v", pair[0], pair[1], err)
		}
		if known {
			t.Fatalf("HasHistory(%s, %s) found the history of another object", pair[0], pair[1])
		}
	}
}

func TestStoreSaveDoesNotWaitForTheDeploymentWriter(t *testing.T) {
	_, db, raw, closeDB := openMetricsDB(t)
	defer closeDB()

	// The write pool has a single connection. A deployment that holds it
	// must not hold the metrics writer for longer than its own deadline.
	tx, err := raw.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO metric_minutes
		 (node_id, container_id, bucket_start, sample_count, expected_count, average_json, peak_json)
		 VALUES ('busy', 'busy', 1, 6, 6, '{}', '{}')`); err != nil {
		t.Fatalf("hold the write transaction: %v", err)
	}

	started := time.Now()
	err = db.Save(context.Background(), []metrics.Bucket{testBucket(historyStart, 6)})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Save succeeded while another transaction held the only write connection")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Save waited %v for the write connection, want a bounded wait", elapsed)
	}
}

func TestStoreSaveRejectsMalformedBuckets(t *testing.T) {
	_, db, _, closeDB := openMetricsDB(t)
	defer closeDB()

	invalid := testBucket(historyStart, 6)
	invalid.NodeID = ""
	if err := db.Save(context.Background(), []metrics.Bucket{invalid}); err == nil {
		t.Fatal("Save accepted a bucket without a node")
	}
}

func TestStoreSaveReportsAnUnencodableValue(t *testing.T) {
	_, db, _, closeDB := openMetricsDB(t)
	defer closeDB()

	invalid := testBucket(historyStart, 6)
	invalid.Average.CPUPercent = math.NaN()
	if err := db.Save(context.Background(), []metrics.Bucket{invalid}); err == nil {
		t.Fatal("Save accepted a NaN value")
	}
}

func TestStoreQueryRejectsAnInvalidRange(t *testing.T) {
	_, db, _, closeDB := openMetricsDB(t)
	defer closeDB()

	if _, err := db.Query(context.Background(), "", "c1", historyStart, historyStart.Add(time.Minute)); err == nil {
		t.Fatal("Query accepted an empty node")
	}
	if _, err := db.Query(context.Background(), "n1", "", historyStart, historyStart.Add(time.Minute)); err == nil {
		t.Fatal("Query accepted an empty container")
	}
	if _, err := db.Query(context.Background(), "n1", "c1", historyStart, historyStart); err == nil {
		t.Fatal("Query accepted an empty time range")
	}
}

func TestStoreSaveIsBoundedWhileAnotherConnectionHoldsTheWriteLock(t *testing.T) {
	path, db, _, closeDB := openMetricsDB(t)
	defer closeDB()

	other, err := store.Open(path)
	if err != nil {
		t.Fatalf("open the other pool: %v", err)
	}
	defer other.Close()

	tx, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO metric_minutes
		 (node_id, container_id, bucket_start, sample_count, expected_count, average_json, peak_json)
		 VALUES ('busy', 'busy', 1, 6, 6, '{}', '{}')`); err != nil {
		t.Fatalf("hold the write lock: %v", err)
	}

	started := time.Now()
	err = db.Save(context.Background(), []metrics.Bucket{testBucket(historyStart, 6)})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Save succeeded while another connection held the write lock")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Save waited %v on a busy database, want a bounded wait", elapsed)
	}
}
