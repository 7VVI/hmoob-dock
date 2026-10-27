package metrics

import (
	"context"
	"strings"
	"sync"
	"time"

	"hmoob-dock/internal/protocol"
)

// The defaults of the ingestion of the samples.
const (
	// DefaultMaxTrackedContainers bounds the previous samples the control端
	// keeps for the rate calculation. A container that leaves the count is
	// sampled from scratch when it appears again, which is a gap and not a
	// wrong rate.
	DefaultMaxTrackedContainers = 10000
)

// ContainerSource reports whether a container of a node exists in the
// recent inventory of the node.
//
// The control端 checks the identity before it accepts a sample, so a node
// cannot report counters of a container that belongs to another node or
// that was never observed. The check is an interface, because the inventory
// lives in the connection of the agent and the metrics package stays
// independent of the transport.
type ContainerSource interface {
	// HasContainer reports whether the recent inventory of the node holds
	// the container.
	HasContainer(nodeID, containerID string) bool
}

// IngestOptions changes the limits of the ingestion. The zero value keeps
// the defaults.
type IngestOptions struct {
	// ClockSkewLimit is the largest difference between the collection
	// time of a sample and the time it was received.
	ClockSkewLimit time.Duration
	// MaxTrackedContainers bounds the previous samples kept in memory.
	MaxTrackedContainers int
}

// IngestStats counts what the ingestion did with the samples. The counters
// make a rejection visible without logging one line per sample.
type IngestStats struct {
	// Samples is the number of the structurally valid raw samples.
	Samples uint64
	// Points is the number of the valid live points.
	Points uint64
	// Gaps is the number of the first samples and counter resets. They are
	// observations without a usable interval, not zero rates.
	Gaps uint64
	// Duplicates is the number of the samples that did not move the time
	// forward.
	Duplicates uint64
	// ClockSkew is the number of the samples whose clock is not
	// trustworthy.
	ClockSkew uint64
	// UnknownContainers is the number of the samples of a container that
	// was not in the recent inventory of their node.
	UnknownContainers uint64
	// Malformed is the number of the samples without a node, a container or
	// a collection time.
	Malformed uint64
	// DroppedBuckets is the number of the minute buckets that were lost,
	// either because the in-memory queue was full or because the write
	// failed.
	DroppedBuckets uint64
	// WriteFailures is the number of the batches that the history store
	// refused.
	WriteFailures uint64
	// PendingBuckets is the number of the unflushed minute buckets.
	PendingBuckets int
}

// Ingest turns the raw samples of the authenticated nodes into live points,
// aggregates them and writes the completed minutes.
//
// The first sample of a container, a container that was replaced, a counter
// reset and a counter that did not move are gaps: the point is invalid and
// never becomes a zero. A sample whose clock differs from the receipt by
// more than the limit and a sample of a container outside the recent
// inventory of its node do not refresh the live value and do not reach the
// aggregator.
type Ingest struct {
	store      *Store
	aggregator *Aggregator
	containers ContainerSource
	now        func() time.Time
	clockSkew  time.Duration
	maxTracked int

	mu sync.Mutex
	// previous holds the last raw sample of every container. It is the
	// baseline of the next rate.
	previous map[containerKey]protocol.RawSample
	// latest holds the newest valid live point of every container.
	latest map[containerKey]Point
	// observers receive every calculated point, including a gap. The
	// callback runs in the goroutine that handles the sample, so it must
	// return quickly and must not call back into the ingest.
	observers []func(Point)
	stats     IngestStats
	// lostBuckets counts the buckets of a failed write. They are no longer
	// retried, so their minutes are a gap.
	lostBuckets uint64
}

// containerKey identifies a container of a node.
type containerKey struct {
	nodeID      string
	containerID string
}

// NewIngest builds the ingestion with the default limits.
func NewIngest(store *Store, aggregator *Aggregator, containers ContainerSource) *Ingest {
	return NewIngestWithOptions(store, aggregator, containers, IngestOptions{})
}

// NewIngestWithOptions builds the ingestion with the given limits. A zero
// limit falls back to the default.
func NewIngestWithOptions(store *Store, aggregator *Aggregator, containers ContainerSource, options IngestOptions) *Ingest {
	clockSkew := options.ClockSkewLimit
	if clockSkew <= 0 {
		clockSkew = DefaultClockSkewLimit
	}
	maxTracked := options.MaxTrackedContainers
	if maxTracked <= 0 {
		maxTracked = DefaultMaxTrackedContainers
	}
	return &Ingest{
		store:      store,
		aggregator: aggregator,
		containers: containers,
		now:        time.Now,
		clockSkew:  clockSkew,
		maxTracked: maxTracked,
		previous:   make(map[containerKey]protocol.RawSample),
		latest:     make(map[containerKey]Point),
	}
}

// Observe accepts one raw sample of the authenticated node.
//
// The returned point reports whether the sample refreshed the live values.
// A rejection is not an error of the caller: it is a data quality fact that
// the counters of IngestStats make visible. A callback registered with
// OnPoint receives every calculated point, including a gap; it does not
// receive a duplicate, a clock anomaly or a sample of an unknown container.
func (i *Ingest) Observe(nodeID string, receivedAt time.Time, sample protocol.RawSample) Point {
	nodeID = strings.TrimSpace(nodeID)
	sample.ContainerID = strings.TrimSpace(sample.ContainerID)
	if receivedAt.IsZero() {
		receivedAt = i.now()
	}
	receivedAt = receivedAt.UTC()

	point := Point{
		NodeID:      nodeID,
		ContainerID: sample.ContainerID,
		At:          sample.CollectedAt.UTC(),
	}
	if nodeID == "" || sample.ContainerID == "" || sample.CollectedAt.IsZero() {
		i.mu.Lock()
		i.stats.Malformed++
		i.mu.Unlock()
		return point
	}

	key := containerKey{nodeID: nodeID, containerID: sample.ContainerID}

	i.mu.Lock()
	i.stats.Samples++
	i.mu.Unlock()

	// A container that is not in the recent inventory of the node is not
	// an observation of this node. The old counters are dropped, so a
	// container that comes back starts with a gap instead of a rate over
	// the time it was away.
	if i.containers != nil && !i.containers.HasContainer(nodeID, sample.ContainerID) {
		i.mu.Lock()
		delete(i.previous, key)
		delete(i.latest, key)
		i.stats.UnknownContainers++
		i.mu.Unlock()
		return point
	}

	// A clock that differs from the receipt is not trustworthy. The sample
	// is not a live observation and must not become the baseline of the
	// next rate either.
	if skew := sample.CollectedAt.Sub(receivedAt); absDuration(skew) > i.clockSkew {
		i.mu.Lock()
		i.stats.ClockSkew++
		i.mu.Unlock()
		return point
	}

	i.mu.Lock()
	previous, known := i.previous[key]
	if known && !sample.CollectedAt.After(previous.CollectedAt) {
		// The sample describes no new interval. Counting it again would
		// inflate the bucket and move the live value backwards.
		i.stats.Duplicates++
		i.mu.Unlock()
		return point
	}
	if !known && len(i.previous) >= i.maxTracked {
		i.evictOldestLocked()
	}
	i.previous[key] = sample

	if !known {
		// The first sample has no predecessor, so it is a gap and not a
		// rate of zero.
		i.stats.Gaps++
		observers := i.observersLocked()
		i.mu.Unlock()
		notify(observers, point)
		return point
	}

	derived := Calculate(previous, sample)
	point.Values = derived.Values
	point.Valid = derived.Valid
	if derived.Valid {
		i.latest[key] = point
		i.stats.Points++
	} else {
		// The counters were reset or the interval is not usable. The
		// current sample is the new baseline, so the next sample can be a
		// valid rate again.
		i.stats.Gaps++
	}
	observers := i.observersLocked()
	i.mu.Unlock()

	if derived.Valid && i.aggregator != nil {
		i.aggregator.Add(point)
	}
	notify(observers, point)
	return point
}

// ForgetNode drops the previous samples and the live points of a node.
//
// It is called when the connection of the node ends. The next sample starts
// a new observation, so the rate never spans the outage and the page never
// shows a live value of a node that is gone.
func (i *Ingest) ForgetNode(nodeID string) {
	nodeID = strings.TrimSpace(nodeID)
	if i == nil || nodeID == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for key := range i.previous {
		if key.nodeID == nodeID {
			delete(i.previous, key)
			delete(i.latest, key)
		}
	}
}

// Latest returns the newest valid live point of a container.
func (i *Ingest) Latest(nodeID, containerID string) (Point, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	point, ok := i.latest[containerKey{
		nodeID:      strings.TrimSpace(nodeID),
		containerID: strings.TrimSpace(containerID),
	}]
	return point, ok
}

// OnPoint registers a callback that receives every calculated point of an
// accepted sample. The callback runs synchronously in the goroutine that
// handles the sample; it must return quickly and must not call back into
// the ingest. The At field orders the points of one container.
func (i *Ingest) OnPoint(fn func(Point)) {
	if fn == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.observers = append(i.observers, fn)
}

// Flush writes the minutes that are completed and no longer accept a late
// sample.
//
// A sample may arrive up to the clock limit after its collection, so a
// minute is flushed only after that window passed. The current minute stays
// open.
func (i *Ingest) Flush(ctx context.Context, now time.Time) error {
	return i.flushBefore(ctx, now.UTC().Truncate(time.Minute).Add(-i.clockSkew))
}

// FlushCompleted writes every minute that ended before the given time.
//
// It is used when the control端 stops: the last completed minute should
// survive a restart, while the current partial minute stays unsaved.
func (i *Ingest) FlushCompleted(ctx context.Context, now time.Time) error {
	return i.flushBefore(ctx, now.UTC().Truncate(time.Minute))
}

// flushBefore closes the buckets before the given time and writes them.
func (i *Ingest) flushBefore(ctx context.Context, before time.Time) error {
	if i == nil || i.aggregator == nil {
		return nil
	}
	buckets := i.aggregator.FlushBefore(before)
	if len(buckets) == 0 || i.store == nil {
		return nil
	}
	if err := i.store.Save(ctx, buckets); err != nil {
		i.mu.Lock()
		i.stats.WriteFailures++
		i.lostBuckets += uint64(len(buckets))
		i.mu.Unlock()
		return err
	}
	return nil
}

// Stats returns a snapshot of the counters of the ingestion.
func (i *Ingest) Stats() IngestStats {
	if i == nil {
		return IngestStats{}
	}
	i.mu.Lock()
	stats := i.stats
	stats.DroppedBuckets = i.lostBuckets
	i.mu.Unlock()

	if i.aggregator != nil {
		stats.DroppedBuckets += i.aggregator.DroppedBuckets()
		stats.PendingBuckets = i.aggregator.PendingBuckets()
	}
	return stats
}

// evictOldestLocked drops the containers that were sampled longest ago. The
// caller holds the lock.
func (i *Ingest) evictOldestLocked() {
	var oldestKey containerKey
	var oldestAt time.Time
	found := false
	for key, sample := range i.previous {
		if !found || sample.CollectedAt.Before(oldestAt) {
			oldestKey = key
			oldestAt = sample.CollectedAt
			found = true
		}
	}
	if !found {
		return
	}
	delete(i.previous, oldestKey)
	delete(i.latest, oldestKey)
}

// observersLocked copies the callbacks so they run outside the lock. The
// caller holds the lock.
func (i *Ingest) observersLocked() []func(Point) {
	if len(i.observers) == 0 {
		return nil
	}
	observers := make([]func(Point), len(i.observers))
	copy(observers, i.observers)
	return observers
}

// notify hands one point to the callbacks.
func notify(observers []func(Point), point Point) {
	for _, observer := range observers {
		observer(point)
	}
}

// absDuration returns the absolute value of a duration.
func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}
