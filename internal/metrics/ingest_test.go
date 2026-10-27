package metrics_test

import (
	"context"
	"testing"
	"time"

	"hmoob-dock/internal/metrics"
	"hmoob-dock/internal/protocol"
)

// fakeContainers is a recent inventory per node.
type fakeContainers map[string]map[string]bool

func (f fakeContainers) HasContainer(nodeID, containerID string) bool {
	return f[nodeID][containerID]
}

// baseSample is the first raw sample of a running container.
func baseSample(at time.Time) protocol.RawSample {
	return protocol.RawSample{
		ContainerID:   "c1",
		CollectedAt:   at,
		CPUUsageNS:    1e9,
		SystemUsageNS: 10e9,
		OnlineCPUs:    2,
		MemoryUsage:   1000,
		NetRX:         1000,
		NetTX:         2000,
		IORead:        3000,
		IOWrite:       4000,
	}
}

// nextSample continues baseSample over ten seconds. The CPU rises by one
// core-second over ten host-core-seconds, so the rate is 20 percent.
func nextSample(previous protocol.RawSample, at time.Time) protocol.RawSample {
	return protocol.RawSample{
		ContainerID:   previous.ContainerID,
		CollectedAt:   at,
		CPUUsageNS:    previous.CPUUsageNS + 1e9,
		SystemUsageNS: previous.SystemUsageNS + 10e9,
		OnlineCPUs:    previous.OnlineCPUs,
		MemoryUsage:   2000,
		NetRX:         previous.NetRX + 1000,
		NetTX:         previous.NetTX + 2000,
		IORead:        previous.IORead + 2000,
		IOWrite:       previous.IOWrite + 4000,
	}
}

func newTestIngest(t *testing.T, containers metrics.ContainerSource) (*metrics.Ingest, *metrics.Aggregator) {
	t.Helper()
	agg := metrics.NewAggregator()
	return metrics.NewIngest(nil, agg, containers), agg
}

func TestIngestCalculatesARealtimePointAndFeedsTheAggregator(t *testing.T) {
	ingest, agg := newTestIngest(t, fakeContainers{"n1": {"c1": true}})

	first := baseSample(aggregateStart)
	if point := ingest.Observe("n1", first.CollectedAt, first); point.Valid {
		t.Fatalf("the first sample was accepted as a rate: %+v", point)
	}
	if _, ok := ingest.Latest("n1", "c1"); ok {
		t.Fatal("the first sample became the live point")
	}

	second := nextSample(first, aggregateStart.Add(10*time.Second))
	point := ingest.Observe("n1", second.CollectedAt, second)
	if !point.Valid {
		t.Fatalf("the second sample is a gap: %+v", point)
	}
	if point.Values.CPUPercent != 20 {
		t.Fatalf("cpu = %v, want 20", point.Values.CPUPercent)
	}
	if point.Values.NetRXPerSec != 100 || point.Values.NetTXPerSec != 200 {
		t.Fatalf("network rates = %+v, want 100/200", point.Values)
	}

	latest, ok := ingest.Latest("n1", "c1")
	if !ok || !latest.Valid || !latest.At.Equal(second.CollectedAt) {
		t.Fatalf("latest = %+v/%v, want the valid second point", latest, ok)
	}

	buckets := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(buckets))
	}
	if buckets[0].SampleCount != 1 || buckets[0].Average.CPUPercent != 20 || buckets[0].Peak.CPUPercent != 20 {
		t.Fatalf("bucket = %+v, want one sample of 20", buckets[0])
	}
}

func TestIngestIgnoresASecondCopyOfTheSample(t *testing.T) {
	ingest, agg := newTestIngest(t, fakeContainers{"n1": {"c1": true}})

	first := baseSample(aggregateStart)
	ingest.Observe("n1", first.CollectedAt, first)
	second := nextSample(first, aggregateStart.Add(10*time.Second))
	ingest.Observe("n1", second.CollectedAt, second)

	// The same sample arrives twice. It describes no new interval, so it
	// neither refreshes the live point nor adds a second sample.
	duplicate := ingest.Observe("n1", second.CollectedAt, second)
	if duplicate.Valid {
		t.Fatalf("the duplicate sample was counted again: %+v", duplicate)
	}
	latest, _ := ingest.Latest("n1", "c1")
	if !latest.At.Equal(second.CollectedAt) {
		t.Fatalf("the duplicate moved the live point to %v", latest.At)
	}
	buckets := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(buckets) != 1 || buckets[0].SampleCount != 1 {
		t.Fatalf("buckets = %+v, want one sample", buckets)
	}
}

func TestIngestRejectsClockSkewAndKeepsTheLiveValue(t *testing.T) {
	ingest, agg := newTestIngest(t, fakeContainers{"n1": {"c1": true}})
	first := baseSample(aggregateStart)
	ingest.Observe("n1", first.CollectedAt, first)
	second := nextSample(first, aggregateStart.Add(10*time.Second))
	ingest.Observe("n1", second.CollectedAt, second)

	// The sample claims to be 31 seconds older than its receipt.
	skewed := nextSample(second, aggregateStart.Add(20*time.Second))
	if point := ingest.Observe("n1", aggregateStart.Add(51*time.Second), skewed); point.Valid {
		t.Fatalf("a skewed sample refreshed the live point: %+v", point)
	}
	latest, _ := ingest.Latest("n1", "c1")
	if !latest.At.Equal(second.CollectedAt) {
		t.Fatalf("the skewed sample moved the live point to %v", latest.At)
	}
	buckets := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(buckets) != 1 || buckets[0].SampleCount != 1 {
		t.Fatalf("the skewed sample reached the aggregator: %+v", buckets)
	}
	if stats := ingest.Stats(); stats.ClockSkew != 1 {
		t.Fatalf("clock skew counter = %d, want 1", stats.ClockSkew)
	}

	// A difference of exactly the limit is still the same clock.
	boundary := ingest.Observe("n1", aggregateStart.Add(50*time.Second), skewed)
	if !boundary.Valid {
		t.Fatalf("a sample at the clock limit was rejected: %+v", boundary)
	}
	if stats := ingest.Stats(); stats.ClockSkew != 1 {
		t.Fatalf("clock skew counter = %d, want 1 after the boundary sample", stats.ClockSkew)
	}
}

func TestIngestRejectsAContainerThatIsNotInTheRecentInventory(t *testing.T) {
	containers := fakeContainers{"n1": {"c1": true}}
	ingest, agg := newTestIngest(t, containers)

	first := baseSample(aggregateStart)
	if point := ingest.Observe("n2", first.CollectedAt, first); point.Valid {
		t.Fatalf("a sample of another node refreshed a point: %+v", point)
	}
	if _, ok := ingest.Latest("n2", "c1"); ok {
		t.Fatal("a sample of another node became the live point")
	}
	if stats := ingest.Stats(); stats.UnknownContainers != 1 {
		t.Fatalf("unknown container counter = %d, want 1", stats.UnknownContainers)
	}

	containers["n2"] = map[string]bool{"c2": true}
	second := baseSample(aggregateStart)
	second.ContainerID = "c2"
	if point := ingest.Observe("n2", second.CollectedAt, second); point.Valid {
		t.Fatalf("the first sample of a new container was a rate: %+v", point)
	}
	third := nextSample(second, aggregateStart.Add(10*time.Second))
	if point := ingest.Observe("n2", third.CollectedAt, third); !point.Valid {
		t.Fatalf("the second sample of the new container is a gap: %+v", point)
	}
	if buckets := agg.FlushBefore(aggregateStart.Add(time.Minute)); len(buckets) != 1 {
		t.Fatalf("buckets = %d, want only the known container", len(buckets))
	}
}

func TestIngestForgetsTheCountersOfARemovedContainer(t *testing.T) {
	containers := fakeContainers{"n1": {"c1": true}}
	ingest, _ := newTestIngest(t, containers)

	first := baseSample(aggregateStart)
	ingest.Observe("n1", first.CollectedAt, first)
	second := nextSample(first, aggregateStart.Add(10*time.Second))
	ingest.Observe("n1", second.CollectedAt, second)

	// The container disappears from the inventory.
	delete(containers["n1"], "c1")
	third := nextSample(second, aggregateStart.Add(20*time.Second))
	if point := ingest.Observe("n1", third.CollectedAt, third); point.Valid {
		t.Fatalf("a sample of a removed container was accepted: %+v", point)
	}

	// The identifier comes back, for example after a container rebuild.
	// The old counters must not make the first sample look like a rate.
	containers["n1"]["c1"] = true
	fourth := nextSample(third, aggregateStart.Add(30*time.Second))
	if point := ingest.Observe("n1", fourth.CollectedAt, fourth); point.Valid {
		t.Fatalf("the first sample after the rebuild continued the old counters: %+v", point)
	}
	fifth := nextSample(fourth, aggregateStart.Add(40*time.Second))
	if point := ingest.Observe("n1", fifth.CollectedAt, fifth); !point.Valid {
		t.Fatalf("the rebuilt container never recovered: %+v", point)
	}
}

func TestIngestStartsANewBaselineAfterACounterReset(t *testing.T) {
	ingest, agg := newTestIngest(t, fakeContainers{"n1": {"c1": true}})
	first := baseSample(aggregateStart)
	ingest.Observe("n1", first.CollectedAt, first)
	second := nextSample(first, aggregateStart.Add(10*time.Second))
	if point := ingest.Observe("n1", second.CollectedAt, second); !point.Valid {
		t.Fatalf("the second sample is a gap: %+v", point)
	}

	reset := protocol.RawSample{
		ContainerID:   "c1",
		CollectedAt:   aggregateStart.Add(20 * time.Second),
		CPUUsageNS:    5e8,
		SystemUsageNS: 5e9,
		OnlineCPUs:    2,
		MemoryUsage:   3000,
		NetRX:         10,
		NetTX:         20,
		IORead:        30,
		IOWrite:       40,
	}
	if point := ingest.Observe("n1", reset.CollectedAt, reset); point.Valid {
		t.Fatalf("the reset was accepted as a rate: %+v", point)
	}
	latest, _ := ingest.Latest("n1", "c1")
	if !latest.At.Equal(second.CollectedAt) {
		t.Fatalf("the reset moved the live point to %v", latest.At)
	}

	recovered := nextSample(reset, aggregateStart.Add(30*time.Second))
	if point := ingest.Observe("n1", recovered.CollectedAt, recovered); !point.Valid {
		t.Fatalf("the baseline was not renewed after the reset: %+v", point)
	}
	buckets := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(buckets) != 1 || buckets[0].SampleCount != 2 {
		t.Fatalf("buckets = %+v, want two valid samples", buckets)
	}
}

func TestIngestFlushesOnlyCompletedMinutes(t *testing.T) {
	_, storeDB, _, closeDB := openMetricsDB(t)
	defer closeDB()

	agg := metrics.NewAggregator()
	ingest := metrics.NewIngest(storeDB, agg, fakeContainers{"n1": {"c1": true}})
	first := baseSample(aggregateStart)
	ingest.Observe("n1", first.CollectedAt, first)
	second := nextSample(first, aggregateStart.Add(10*time.Second))
	ingest.Observe("n1", second.CollectedAt, second)

	// The minute is still open, so the writer does not save a partial
	// bucket yet.
	if err := ingest.Flush(context.Background(), aggregateStart.Add(10*time.Second)); err != nil {
		t.Fatalf("Flush of the open minute: %v", err)
	}
	buckets, err := storeDB.Query(context.Background(), "n1", "c1", aggregateStart, aggregateStart.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(buckets) != 0 {
		t.Fatalf("the open minute was written: %+v", buckets)
	}

	// The late-sample window has passed, so the minute is complete.
	if err := ingest.Flush(context.Background(), aggregateStart.Add(91*time.Second)); err != nil {
		t.Fatalf("Flush of the completed minute: %v", err)
	}
	buckets, err = storeDB.Query(context.Background(), "n1", "c1", aggregateStart, aggregateStart.Add(time.Minute))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(buckets) != 1 || buckets[0].SampleCount != 1 || buckets[0].Average.CPUPercent != 20 {
		t.Fatalf("written buckets = %+v, want one sample of 20", buckets)
	}
}

func TestIngestReportsTheDroppedAndWrittenBuckets(t *testing.T) {
	agg := metrics.NewAggregatorWithOptions(metrics.AggregatorOptions{MaxBuckets: 1})
	ingest := metrics.NewIngest(nil, agg, fakeContainers{"n1": {"c1": true}})
	first := baseSample(aggregateStart)
	ingest.Observe("n1", first.CollectedAt, first)
	second := nextSample(first, aggregateStart.Add(10*time.Second))
	ingest.Observe("n1", second.CollectedAt, second)
	third := nextSample(second, aggregateStart.Add(70*time.Second))
	ingest.Observe("n1", third.CollectedAt, third)

	stats := ingest.Stats()
	if stats.DroppedBuckets != 1 {
		t.Fatalf("dropped buckets = %d, want 1", stats.DroppedBuckets)
	}
	if stats.Points != 2 {
		t.Fatalf("points = %d, want 2", stats.Points)
	}
}

func TestIngestHandsAcceptedSamplesToTheObservers(t *testing.T) {
	ingest, _ := newTestIngest(t, fakeContainers{"n1": {"c1": true}})
	var observed []metrics.Point
	ingest.OnPoint(func(point metrics.Point) {
		observed = append(observed, point)
	})

	first := baseSample(aggregateStart)
	ingest.Observe("n1", first.CollectedAt, first)
	second := nextSample(first, aggregateStart.Add(10*time.Second))
	ingest.Observe("n1", second.CollectedAt, second)
	skewed := nextSample(second, aggregateStart.Add(20*time.Second))
	ingest.Observe("n1", aggregateStart.Add(60*time.Second), skewed)

	if len(observed) != 2 {
		t.Fatalf("observed = %d points, want the first gap and the first rate", len(observed))
	}
	if observed[0].Valid || !observed[1].Valid {
		t.Fatalf("observed validity = %v/%v, want false/true", observed[0].Valid, observed[1].Valid)
	}
}

func TestIngestForgetsTheCountersOfADisconnectedNode(t *testing.T) {
	ingest, _ := newTestIngest(t, fakeContainers{"n1": {"c1": true}})
	first := baseSample(aggregateStart)
	ingest.Observe("n1", first.CollectedAt, first)
	second := nextSample(first, aggregateStart.Add(10*time.Second))
	ingest.Observe("n1", second.CollectedAt, second)

	ingest.ForgetNode("n1")
	if _, ok := ingest.Latest("n1", "c1"); ok {
		t.Fatal("the live point survived the end of the connection")
	}
	third := nextSample(second, aggregateStart.Add(20*time.Second))
	if point := ingest.Observe("n1", third.CollectedAt, third); point.Valid {
		t.Fatalf("the first sample after the reconnect continued the old counters: %+v", point)
	}
	fourth := nextSample(third, aggregateStart.Add(30*time.Second))
	if point := ingest.Observe("n1", fourth.CollectedAt, fourth); !point.Valid {
		t.Fatalf("the ingest did not recover after the reconnect: %+v", point)
	}
}
