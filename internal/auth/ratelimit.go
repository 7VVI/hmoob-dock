package auth

import (
	"sync"
	"time"
)

const (
	// defaultMaxSources is the number of sources the limiter keeps. A
	// flood of sources cannot grow the memory of the control端 without
	// bounds, because the oldest source is dropped when the limit is
	// reached.
	defaultMaxSources = 4096
	// cleanupInterval is the number of operations after which the limiter
	// removes the records whose window passed.
	cleanupInterval = 64
)

// Limiter counts the failed attempts of a source and blocks further attempts
// of that source once it used up its budget. The window starts with the
// first failure, so a source that keeps failing stays blocked until the
// window passed.
//
// The table of the limiter is bounded: it holds at most maxSources sources
// and the records whose window passed are removed from time to time. An
// attacker that uses many source addresses therefore cannot grow the
// memory of the control端 without bounds.
type Limiter struct {
	mu       sync.Mutex
	attempts map[string]*attempt
	max      int
	// maxSources is the number of sources the table may hold.
	maxSources int
	window     time.Duration
	now        func() time.Time
	// sequence numbers the records in the order of their creation. It
	// tells which record is the oldest one.
	sequence int64
	// operations counts the calls of the limiter. It drives the periodic
	// cleanup of the expired records.
	operations int
}

// attempt holds the failures of one source.
type attempt struct {
	count int
	first time.Time
	// sequence is the number of the record in the order of the creation.
	sequence int64
}

// NewLimiter builds a limiter that allows max failed attempts per window. A
// nil clock falls back to the system clock.
func NewLimiter(max int, window time.Duration, now func() time.Time) *Limiter {
	return NewLimiterWithCapacity(max, window, defaultMaxSources, now)
}

// NewLimiterWithCapacity builds a limiter that allows max failed attempts
// per window and keeps at most maxSources sources. A capacity of zero or
// less falls back to the default capacity. A nil clock falls back to the
// system clock.
func NewLimiterWithCapacity(max int, window time.Duration, maxSources int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	if maxSources <= 0 {
		maxSources = defaultMaxSources
	}
	return &Limiter{
		attempts:   make(map[string]*attempt),
		max:        max,
		maxSources: maxSources,
		window:     window,
		now:        now,
	}
}

// Allow reports whether the source may make another attempt.
func (l *Limiter) Allow(source string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	record, ok := l.attempts[source]
	if !ok {
		return true
	}
	if l.now().Sub(record.first) >= l.window {
		delete(l.attempts, source)
		return true
	}
	return record.count < l.max
}

// Fail records a failed attempt of the source.
func (l *Limiter) Fail(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.operations++
	if l.operations%cleanupInterval == 0 {
		l.removeExpired(now)
	}

	record, ok := l.attempts[source]
	if ok && now.Sub(record.first) >= l.window {
		// The window of the source passed, so its budget starts again.
		delete(l.attempts, source)
		ok = false
	}
	if !ok {
		l.sequence++
		l.attempts[source] = &attempt{count: 1, first: now, sequence: l.sequence}
		l.evictOldest()
		return
	}
	record.count++
}

// Reset clears the recorded failures of the source.
func (l *Limiter) Reset(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.attempts, source)
}

// removeExpired drops the records whose window passed. Without it the table
// would keep the sources that never come back.
func (l *Limiter) removeExpired(now time.Time) {
	for source, record := range l.attempts {
		if now.Sub(record.first) >= l.window {
			delete(l.attempts, source)
		}
	}
}

// evictOldest keeps the table of the limiter bounded. When the limit is
// reached, the oldest record is dropped.
func (l *Limiter) evictOldest() {
	for len(l.attempts) > l.maxSources {
		oldest := ""
		var oldestSequence int64
		first := true
		for source, record := range l.attempts {
			if first || record.sequence < oldestSequence {
				oldest = source
				oldestSequence = record.sequence
				first = false
			}
		}
		if oldest == "" {
			return
		}
		delete(l.attempts, oldest)
	}
}
