package metrics_test

import (
	"testing"
	"time"

	"hmoob-dock/internal/metrics"
)

// aggregateStart is an exact minute boundary, so a sample added with an
// offset of i*10s stays in the minute that starts here.
var aggregateStart = time.Unix(1800000000, 0).UTC().Truncate(time.Minute)

func TestMinuteBucketRetainsPeakAndMissingSamples(t *testing.T) {
	agg := metrics.NewAggregator()
	start := time.Unix(1800000000, 0).UTC().Truncate(time.Minute)
	for i, cpu := range []float64{10, 90} {
		agg.Add(metrics.Point{
			NodeID:      "n1",
			ContainerID: "c1",
			At:          start.Add(time.Duration(i) * 10 * time.Second),
			Valid:       true,
			Values:      metrics.Values{CPUPercent: cpu},
		})
	}
	buckets := agg.FlushBefore(start.Add(time.Minute))
	if len(buckets) != 1 {
		t.Fatalf("buckets=%d", len(buckets))
	}
	b := buckets[0]
	if b.Average.CPUPercent != 50 || b.Peak.CPUPercent != 90 || b.SampleCount != 2 || b.ExpectedCount != 6 {
		t.Fatalf("wrong aggregate: %+v", b)
	}
}

func TestAggregatorKeepsTheSumAndTheCountInsteadOfAveragingAverages(t *testing.T) {
	agg := metrics.NewAggregator()

	// The three samples average to 40, not to the average of an earlier
	// average of 15 and a later sample of 90.
	for i, cpu := range []float64{10, 20, 90} {
		agg.Add(metrics.Point{
			NodeID:      "n1",
			ContainerID: "c1",
			At:          aggregateStart.Add(time.Duration(i) * 10 * time.Second),
			Valid:       true,
			Values:      metrics.Values{CPUPercent: cpu},
		})
	}

	buckets := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(buckets) != 1 {
		t.Fatalf("buckets=%d, want 1", len(buckets))
	}
	bucket := buckets[0]
	if bucket.Average.CPUPercent != 40 {
		t.Fatalf("average = %v, want 40 (sum/count, not an average of averages)", bucket.Average.CPUPercent)
	}
	if bucket.SampleCount != 3 {
		t.Fatalf("sample count = %d, want 3", bucket.SampleCount)
	}
}

func TestAggregatorKeepsThePeakOfEveryField(t *testing.T) {
	agg := metrics.NewAggregator()
	samples := []metrics.Values{
		{CPUPercent: 90, MemoryBytes: 100, NetRXPerSec: 5, NetTXPerSec: 50, IOReadPerSec: 7, IOWritePerSec: 70},
		{CPUPercent: 10, MemoryBytes: 500, NetRXPerSec: 50, NetTXPerSec: 5, IOReadPerSec: 70, IOWritePerSec: 7},
	}
	for i, values := range samples {
		agg.Add(metrics.Point{
			NodeID:      "n1",
			ContainerID: "c1",
			At:          aggregateStart.Add(time.Duration(i) * 10 * time.Second),
			Valid:       true,
			Values:      values,
		})
	}

	buckets := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(buckets) != 1 {
		t.Fatalf("buckets=%d, want 1", len(buckets))
	}
	bucket := buckets[0]
	wantAverage := metrics.Values{
		CPUPercent: 50, MemoryBytes: 300, NetRXPerSec: 27.5, NetTXPerSec: 27.5,
		IOReadPerSec: 38.5, IOWritePerSec: 38.5,
	}
	wantPeak := metrics.Values{
		CPUPercent: 90, MemoryBytes: 500, NetRXPerSec: 50, NetTXPerSec: 50,
		IOReadPerSec: 70, IOWritePerSec: 70,
	}
	if bucket.Average != wantAverage {
		t.Fatalf("average = %+v, want %+v", bucket.Average, wantAverage)
	}
	if bucket.Peak != wantPeak {
		t.Fatalf("peak = %+v, want %+v", bucket.Peak, wantPeak)
	}
}

func TestAggregatorSeparatesMinutesNodesAndContainers(t *testing.T) {
	agg := metrics.NewAggregator()
	points := []metrics.Point{
		{NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(10 * time.Second), Valid: true, Values: metrics.Values{CPUPercent: 10}},
		{NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(70 * time.Second), Valid: true, Values: metrics.Values{CPUPercent: 30}},
		{NodeID: "n1", ContainerID: "c2", At: aggregateStart.Add(10 * time.Second), Valid: true, Values: metrics.Values{CPUPercent: 50}},
		{NodeID: "n2", ContainerID: "c1", At: aggregateStart.Add(10 * time.Second), Valid: true, Values: metrics.Values{CPUPercent: 70}},
	}
	for _, point := range points {
		agg.Add(point)
	}

	buckets := agg.FlushBefore(aggregateStart.Add(2 * time.Minute))
	if len(buckets) != 4 {
		t.Fatalf("buckets=%d, want 4", len(buckets))
	}
	got := make(map[string]float64, len(buckets))
	for _, bucket := range buckets {
		got[bucket.NodeID+"/"+bucket.ContainerID+"@"+bucket.Start.Format(time.RFC3339)] = bucket.Average.CPUPercent
	}
	want := map[string]float64{
		"n1/c1@" + aggregateStart.Format(time.RFC3339):                  10,
		"n1/c1@" + aggregateStart.Add(time.Minute).Format(time.RFC3339): 30,
		"n1/c2@" + aggregateStart.Format(time.RFC3339):                  50,
		"n2/c1@" + aggregateStart.Format(time.RFC3339):                  70,
	}
	for key, average := range want {
		if got[key] != average {
			t.Fatalf("%s average = %v, want %v (all=%v)", key, got[key], average, got)
		}
	}
}

func TestAggregatorDoesNotCreateAZeroBucketForAMissingMinute(t *testing.T) {
	agg := metrics.NewAggregator()

	// The first minute only has an invalid point, which is a gap and not an
	// idle container.
	agg.Add(metrics.Point{NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(10 * time.Second), Valid: false})
	agg.Add(metrics.Point{NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(70 * time.Second), Valid: true, Values: metrics.Values{CPUPercent: 25}})

	buckets := agg.FlushBefore(aggregateStart.Add(2 * time.Minute))
	if len(buckets) != 1 {
		t.Fatalf("buckets=%d, want only the observed minute", len(buckets))
	}
	if !buckets[0].Start.Equal(aggregateStart.Add(time.Minute)) {
		t.Fatalf("bucket start = %v, want %v", buckets[0].Start, aggregateStart.Add(time.Minute))
	}
	if buckets[0].Average.CPUPercent != 25 {
		t.Fatalf("average = %v, want 25", buckets[0].Average.CPUPercent)
	}
}

func TestAggregatorIgnoresSamplesOfAFlushedMinute(t *testing.T) {
	agg := metrics.NewAggregator()
	point := metrics.Point{
		NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(10 * time.Second),
		Valid: true, Values: metrics.Values{CPUPercent: 10},
	}
	agg.Add(point)
	first := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(first) != 1 || first[0].Average.CPUPercent != 10 {
		t.Fatalf("first flush = %+v, want one bucket with 10", first)
	}

	// A sample that arrives after its minute was written must not reopen
	// the bucket or inflate its count on the next flush.
	agg.Add(metrics.Point{
		NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(20 * time.Second),
		Valid: true, Values: metrics.Values{CPUPercent: 90},
	})
	if late := agg.FlushBefore(aggregateStart.Add(2 * time.Minute)); len(late) != 0 {
		t.Fatalf("a late sample reopened the written minute: %+v", late)
	}
	if agg.PendingBuckets() != 0 {
		t.Fatalf("pending buckets = %d, want 0", agg.PendingBuckets())
	}
}

func TestAggregatorDropsTheOldestBucketUnderPressure(t *testing.T) {
	agg := metrics.NewAggregatorWithOptions(metrics.AggregatorOptions{MaxBuckets: 2})
	for i := 0; i < 3; i++ {
		agg.Add(metrics.Point{
			NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(time.Duration(i) * time.Minute),
			Valid: true, Values: metrics.Values{CPUPercent: float64(10 + i)},
		})
	}
	if agg.PendingBuckets() != 2 {
		t.Fatalf("pending buckets = %d, want 2", agg.PendingBuckets())
	}
	if agg.DroppedBuckets() != 1 {
		t.Fatalf("dropped buckets = %d, want 1", agg.DroppedBuckets())
	}

	buckets := agg.FlushBefore(aggregateStart.Add(3 * time.Minute))
	if len(buckets) != 2 {
		t.Fatalf("buckets=%d, want 2", len(buckets))
	}
	if !buckets[0].Start.Equal(aggregateStart.Add(time.Minute)) ||
		!buckets[1].Start.Equal(aggregateStart.Add(2*time.Minute)) {
		t.Fatalf("the oldest bucket was not the one dropped: %+v", buckets)
	}
}

func TestAggregatorUsesTheConfiguredExpectedSampleCount(t *testing.T) {
	agg := metrics.NewAggregatorWithOptions(metrics.AggregatorOptions{
		ExpectedSamplesPerMinute: 12,
	})
	agg.Add(metrics.Point{
		NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(5 * time.Second),
		Valid: true, Values: metrics.Values{CPUPercent: 5},
	})
	buckets := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(buckets) != 1 {
		t.Fatalf("buckets=%d, want 1", len(buckets))
	}
	if buckets[0].ExpectedCount != 12 {
		t.Fatalf("expected count = %d, want 12", buckets[0].ExpectedCount)
	}
}

func TestAggregatorIgnoresMalformedPoints(t *testing.T) {
	agg := metrics.NewAggregator()
	agg.Add(metrics.Point{At: aggregateStart, Valid: true, Values: metrics.Values{CPUPercent: 100}})
	agg.Add(metrics.Point{NodeID: "n1", ContainerID: "c1", Valid: true, Values: metrics.Values{CPUPercent: 100}})
	if agg.PendingBuckets() != 0 {
		t.Fatalf("pending buckets = %d, want 0", agg.PendingBuckets())
	}
}

func TestAggregatorDropsTheOldestMinuteEvenWhenItArrivedLast(t *testing.T) {
	agg := metrics.NewAggregatorWithOptions(metrics.AggregatorOptions{MaxBuckets: 2})
	// The minutes arrive out of order. The pressure must drop the oldest
	// minute, not the first one that was created.
	for _, offset := range []int{2, 0, 1} {
		agg.Add(metrics.Point{
			NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(time.Duration(offset) * time.Minute),
			Valid: true, Values: metrics.Values{CPUPercent: float64(10 + offset)},
		})
	}
	buckets := agg.FlushBefore(aggregateStart.Add(4 * time.Minute))
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(buckets))
	}
	if !buckets[0].Start.Equal(aggregateStart.Add(time.Minute)) ||
		!buckets[1].Start.Equal(aggregateStart.Add(2*time.Minute)) {
		t.Fatalf("the oldest minute was not dropped: %+v", buckets)
	}
}

func TestAggregatorCountsARepeatedSampleOnce(t *testing.T) {
	agg := metrics.NewAggregator()
	point := metrics.Point{
		NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(10 * time.Second),
		Valid: true, Values: metrics.Values{CPUPercent: 40},
	}
	agg.Add(point)
	agg.Add(point)

	buckets := agg.FlushBefore(aggregateStart.Add(time.Minute))
	if len(buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(buckets))
	}
	if buckets[0].SampleCount != 1 || buckets[0].Average.CPUPercent != 40 {
		t.Fatalf("bucket = %+v, want one sample of 40", buckets[0])
	}
}

func TestAggregatorDropsTheNewSampleWhenItIsTheOldestMinute(t *testing.T) {
	agg := metrics.NewAggregatorWithOptions(metrics.AggregatorOptions{MaxBuckets: 2})
	for _, offset := range []int{2, 3, 1} {
		agg.Add(metrics.Point{
			NodeID: "n1", ContainerID: "c1", At: aggregateStart.Add(time.Duration(offset) * time.Minute),
			Valid: true, Values: metrics.Values{CPUPercent: float64(10 + offset)},
		})
	}
	if agg.DroppedBuckets() != 1 {
		t.Fatalf("dropped buckets = %d, want 1", agg.DroppedBuckets())
	}
	buckets := agg.FlushBefore(aggregateStart.Add(5 * time.Minute))
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want 2", len(buckets))
	}
	if !buckets[0].Start.Equal(aggregateStart.Add(2*time.Minute)) ||
		!buckets[1].Start.Equal(aggregateStart.Add(3*time.Minute)) {
		t.Fatalf("the newest minutes were not kept: %+v", buckets)
	}
}
