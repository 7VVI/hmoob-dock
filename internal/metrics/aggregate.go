package metrics

import (
	"sort"
	"sync"
	"time"
)

// The defaults of the minute aggregation.
const (
	// DefaultSampleInterval is the interval at which an agent samples a
	// container.
	DefaultSampleInterval = 10 * time.Second
	// DefaultExpectedSamplesPerMinute is the number of the samples a
	// complete minute holds at the default interval. A bucket keeps the
	// number, so a reader tells a partial minute from a complete one
	// without guessing the interval.
	DefaultExpectedSamplesPerMinute = int(time.Minute / DefaultSampleInterval)
	// DefaultMaxBuckets is the number of the unflushed minute buckets the
	// aggregator keeps in memory. The queue is bounded: an agent that
	// floods the control端 or a database that is busy must not grow the
	// memory of the process without a limit.
	DefaultMaxBuckets = 6000
	// DefaultClockSkewLimit is the largest difference between the time a
	// sample claims and the time it was received. A sample beyond the limit
	// belongs to a clock that is not trustworthy and is not a live value.
	DefaultClockSkewLimit = 30 * time.Second
)

// Point is the observation of one container at one point in time.
//
// At is the collection time of the sample that produced the values. Valid
// reports whether the values describe an observed interval; an invalid
// point is a gap of the observation and not an idle container.
type Point struct {
	// NodeID is the enrolled node the sample came from. The identity of the
	// authenticated connection provides it, not the payload of the agent.
	NodeID string
	// ContainerID is the container the sample belongs to.
	ContainerID string
	// At is the time the sample was collected.
	At time.Time
	// Values are the rates and the usage of the sample.
	Values Values
	// Valid reports whether the values describe an observed interval.
	Valid bool
}

// Bucket is the aggregate of one container in one minute.
//
// Average is the sum of the valid samples divided by their count, never an
// average of averages. Peak keeps the largest value of every field on its
// own, so a spike of one metric is not hidden by another one. SampleCount
// is the number of the valid samples of the bucket, ExpectedCount is the
// number a complete minute holds at the default interval.
type Bucket struct {
	// NodeID is the node the bucket belongs to.
	NodeID string
	// ContainerID is the container the bucket belongs to.
	ContainerID string
	// Start is the start of the minute in UTC.
	Start time.Time
	// Average is the mean of the valid samples of the minute.
	Average Values
	// Peak is the largest value of every field of the minute.
	Peak Values
	// SampleCount is the number of the valid samples of the minute.
	SampleCount int
	// ExpectedCount is the number of the samples a complete minute holds.
	ExpectedCount int
}

// Complete reports whether the bucket holds every expected sample. A bucket
// with fewer samples is a partial minute; the reader must not present it as
// a complete one.
func (b Bucket) Complete() bool {
	return b.ExpectedCount > 0 && b.SampleCount >= b.ExpectedCount
}

// AggregatorOptions changes the limits of the aggregator. The zero value
// keeps the defaults, so a caller that only needs the standard behavior
// uses NewAggregator.
type AggregatorOptions struct {
	// MaxBuckets bounds the number of the unflushed minute buckets. A
	// larger value is capped by nothing but the memory of the process;
	// the caller is responsible for a sensible limit.
	MaxBuckets int
	// ExpectedSamplesPerMinute is the number of the samples a complete
	// minute holds.
	ExpectedSamplesPerMinute int
}

// objectKey identifies a container of a node without the minute.
type objectKey struct {
	nodeID      string
	containerID string
}

// bucketKey identifies one minute of one container of one node.
type bucketKey struct {
	objectKey
	start time.Time
}

// accumulator keeps the sum and the peak of the samples of one minute.
//
// The average is calculated once, from the sum and the count. Keeping the
// average of the minute and averaging that value again would weight a
// minute with few samples as much as a complete minute.
type accumulator struct {
	key   bucketKey
	count int
	sum   Values
	peak  Values
	// lastAt is the newest collection time folded into the bucket. The
	// agent samples a container once per round, so a point that does not
	// move the time forward is a repeated delivery of the same sample.
	lastAt time.Time
}

// Aggregator collects the live points of the containers into minute
// buckets.
//
// The aggregator is safe for concurrent use. A bucket is identified by the
// node, the container and the start of the minute. A completed bucket is
// handed to the writer by FlushBefore and is never reopened: a sample that
// arrives after its minute was written is dropped, so a replay does not
// count a sample twice.
type Aggregator struct {
	mu sync.Mutex
	// maxBuckets bounds the unflushed buckets.
	maxBuckets int
	// expected is the number of the samples of a complete minute.
	expected int
	// buckets holds the open buckets by their identity.
	buckets map[bucketKey]*accumulator
	// flushed is the start of the newest minute that was written for a
	// container. A sample of that minute or an older one is a replay and
	// is dropped.
	flushed map[objectKey]time.Time
	// dropped counts the buckets that were lost because the queue was
	// full. The counter is visible through DroppedBuckets, so the loss is
	// not silent.
	dropped uint64
}

// NewAggregator builds an aggregator with the default limits.
func NewAggregator() *Aggregator {
	return NewAggregatorWithOptions(AggregatorOptions{})
}

// NewAggregatorWithOptions builds an aggregator with the given limits. A
// zero limit falls back to the default, so a configuration that does not
// name a value does not create an unusable aggregator.
func NewAggregatorWithOptions(options AggregatorOptions) *Aggregator {
	maxBuckets := options.MaxBuckets
	if maxBuckets <= 0 {
		maxBuckets = DefaultMaxBuckets
	}
	expected := options.ExpectedSamplesPerMinute
	if expected <= 0 {
		expected = DefaultExpectedSamplesPerMinute
	}
	return &Aggregator{
		maxBuckets: maxBuckets,
		expected:   expected,
		buckets:    make(map[bucketKey]*accumulator),
		flushed:    make(map[objectKey]time.Time),
	}
}

// Add adds a live point to its minute bucket.
//
// Invalid points, points without an object and points of a minute that was
// already written are ignored. A sample that arrives after the flush is a
// replay or a late observation; adding it would either count the same
// sample twice or change a value that a reader may have seen already.
func (a *Aggregator) Add(point Point) {
	if a == nil || !point.Valid || point.NodeID == "" || point.ContainerID == "" || point.At.IsZero() {
		return
	}
	start := point.At.UTC().Truncate(time.Minute)
	object := objectKey{nodeID: point.NodeID, containerID: point.ContainerID}
	key := bucketKey{objectKey: object, start: start}

	a.mu.Lock()
	defer a.mu.Unlock()

	if last, ok := a.flushed[object]; ok && !start.After(last) {
		return
	}
	if existing, ok := a.buckets[key]; ok {
		if !point.At.After(existing.lastAt) {
			return
		}
		existing.add(point.Values, point.At)
		return
	}

	bucket := &accumulator{key: key}
	bucket.add(point.Values, point.At)
	a.buckets[key] = bucket
	for len(a.buckets) > a.maxBuckets {
		a.evictOldestLocked()
	}
}

// add folds one sample into the accumulator.
func (a *accumulator) add(values Values, at time.Time) {
	if a.count == 0 {
		a.peak = values
	} else {
		a.peak = maxValues(a.peak, values)
	}
	a.sum = addValues(a.sum, values)
	a.count++
	a.lastAt = at
}

// FlushBefore closes every bucket that starts before the given time and
// returns them in time order. The returned buckets are no longer held by
// the aggregator.
//
// A missing minute has no bucket: a quiet minute of a container that sent
// no sample is a gap of the observation and not a zero. The reader maps
// the missing minutes to an empty value.
func (a *Aggregator) FlushBefore(before time.Time) []Bucket {
	if a == nil {
		return nil
	}
	before = before.UTC()

	a.mu.Lock()
	defer a.mu.Unlock()

	buckets := make([]Bucket, 0)
	for key, closed := range a.buckets {
		if !key.start.Before(before) {
			continue
		}
		buckets = append(buckets, Bucket{
			NodeID:        key.nodeID,
			ContainerID:   key.containerID,
			Start:         key.start,
			Average:       averageValues(closed.sum, closed.count),
			Peak:          closed.peak,
			SampleCount:   closed.count,
			ExpectedCount: a.expected,
		})
		delete(a.buckets, key)
		object := objectKey{nodeID: key.nodeID, containerID: key.containerID}
		if last, ok := a.flushed[object]; !ok || key.start.After(last) {
			a.flushed[object] = key.start
		}
	}
	sort.Slice(buckets, func(left, right int) bool {
		if !buckets[left].Start.Equal(buckets[right].Start) {
			return buckets[left].Start.Before(buckets[right].Start)
		}
		if buckets[left].NodeID != buckets[right].NodeID {
			return buckets[left].NodeID < buckets[right].NodeID
		}
		return buckets[left].ContainerID < buckets[right].ContainerID
	})
	a.pruneFlushedLocked(before)
	return buckets
}

// evictOldestLocked drops the open bucket with the oldest minute. The
// caller holds the lock and guarantees at least one open bucket.
func (a *Aggregator) evictOldestLocked() {
	var oldestKey bucketKey
	var oldest bool
	for key := range a.buckets {
		if !oldest || key.start.Before(oldestKey.start) ||
			(key.start.Equal(oldestKey.start) && (key.nodeID < oldestKey.nodeID ||
				(key.nodeID == oldestKey.nodeID && key.containerID < oldestKey.containerID))) {
			oldestKey = key
			oldest = true
		}
	}
	if !oldest {
		return
	}
	delete(a.buckets, oldestKey)
	a.dropped++
}

// pruneFlushedLocked drops the replay markers that are older than the
// retention of the history. A sample older than that is rejected by the
// clock check before it reaches the aggregator, so the marker is no longer
// needed. The caller holds the lock.
func (a *Aggregator) pruneFlushedLocked(before time.Time) {
	if len(a.flushed) <= a.maxBuckets {
		return
	}
	cutoff := before.Add(-24 * time.Hour)
	for object, start := range a.flushed {
		if start.Before(cutoff) {
			delete(a.flushed, object)
		}
	}
}

// PendingBuckets reports the number of the unflushed buckets.
func (a *Aggregator) PendingBuckets() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.buckets)
}

// DroppedBuckets reports the number of the buckets that were lost because
// the queue was full. The loss is a gap of the history and is visible to
// the reader through the missing minutes and to an operator through this
// counter.
func (a *Aggregator) DroppedBuckets() uint64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dropped
}

// addValues adds two value sets field by field.
func addValues(left, right Values) Values {
	return Values{
		CPUPercent:    left.CPUPercent + right.CPUPercent,
		MemoryBytes:   left.MemoryBytes + right.MemoryBytes,
		NetRXPerSec:   left.NetRXPerSec + right.NetRXPerSec,
		NetTXPerSec:   left.NetTXPerSec + right.NetTXPerSec,
		IOReadPerSec:  left.IOReadPerSec + right.IOReadPerSec,
		IOWritePerSec: left.IOWritePerSec + right.IOWritePerSec,
	}
}

// maxValues returns the larger value of every field.
func maxValues(left, right Values) Values {
	return Values{
		CPUPercent:    max(left.CPUPercent, right.CPUPercent),
		MemoryBytes:   max(left.MemoryBytes, right.MemoryBytes),
		NetRXPerSec:   max(left.NetRXPerSec, right.NetRXPerSec),
		NetTXPerSec:   max(left.NetTXPerSec, right.NetTXPerSec),
		IOReadPerSec:  max(left.IOReadPerSec, right.IOReadPerSec),
		IOWritePerSec: max(left.IOWritePerSec, right.IOWritePerSec),
	}
}

// averageValues divides the sum by the count. The caller guarantees a
// positive count.
func averageValues(sum Values, count int) Values {
	if count <= 0 {
		return Values{}
	}
	divisor := float64(count)
	return Values{
		CPUPercent:    sum.CPUPercent / divisor,
		MemoryBytes:   sum.MemoryBytes / divisor,
		NetRXPerSec:   sum.NetRXPerSec / divisor,
		NetTXPerSec:   sum.NetTXPerSec / divisor,
		IOReadPerSec:  sum.IOReadPerSec / divisor,
		IOWritePerSec: sum.IOWritePerSec / divisor,
	}
}

// max returns the larger of two values.
func max(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}
