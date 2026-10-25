package auth

import (
	"sync"
	"time"
)

// Limiter counts the failed attempts of a source and blocks further attempts
// of that source once it used up its budget. The window starts with the
// first failure, so a source that keeps failing stays blocked until the
// window passed.
type Limiter struct {
	mu       sync.Mutex
	attempts map[string]*attempt
	max      int
	window   time.Duration
	now      func() time.Time
}

// attempt holds the failures of one source.
type attempt struct {
	count int
	first time.Time
}

// NewLimiter builds a limiter that allows max failed attempts per window. A
// nil clock falls back to the system clock.
func NewLimiter(max int, window time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		attempts: make(map[string]*attempt),
		max:      max,
		window:   window,
		now:      now,
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
	record, ok := l.attempts[source]
	if !ok || now.Sub(record.first) >= l.window {
		l.attempts[source] = &attempt{count: 1, first: now}
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
