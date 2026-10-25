package auth

import (
	"testing"
	"time"
)

// TestTheLimiterKeepsItsTableBounded proves that a flood of sources cannot
// grow the table of the limiter without bounds: the oldest source is
// dropped when the limit is reached.
func TestTheLimiterKeepsItsTableBounded(t *testing.T) {
	now := time.Unix(1800000000, 0)
	limiter := NewLimiterWithCapacity(5, 15*time.Minute, 2, func() time.Time { return now })

	limiter.Fail("first")
	limiter.Fail("second")
	limiter.Fail("third")

	if len(limiter.attempts) != 2 {
		t.Fatalf("the limiter holds %d sources, want 2", len(limiter.attempts))
	}
	if _, ok := limiter.attempts["first"]; ok {
		t.Fatal("the limiter kept the oldest source")
	}
	if _, ok := limiter.attempts["third"]; !ok {
		t.Fatal("the limiter dropped the newest source")
	}
}

// TestTheLimiterRemovesTheExpiredSources proves that the sources that never
// come back do not stay in the table of the limiter.
func TestTheLimiterRemovesTheExpiredSources(t *testing.T) {
	now := time.Unix(1800000000, 0)
	limiter := NewLimiterWithCapacity(5, time.Minute, 100, func() time.Time { return now })

	limiter.Fail("first")
	now = now.Add(2 * time.Minute)
	for operation := 0; operation < cleanupInterval; operation++ {
		limiter.Fail("second")
	}

	if len(limiter.attempts) != 1 {
		t.Fatalf("the limiter holds %d sources, want 1", len(limiter.attempts))
	}
	if _, ok := limiter.attempts["first"]; ok {
		t.Fatal("the limiter kept a source whose window passed")
	}
}

// TestTheLimiterCountsTheAttemptsOfEverySource proves that the budget of a
// source does not touch the budget of another one.
func TestTheLimiterCountsTheAttemptsOfEverySource(t *testing.T) {
	now := time.Unix(1800000000, 0)
	limiter := NewLimiterWithCapacity(5, 15*time.Minute, 100, func() time.Time { return now })

	for attempt := 0; attempt < 5; attempt++ {
		limiter.Fail("first")
	}
	if limiter.Allow("first") {
		t.Fatal("the source may make another attempt")
	}
	if !limiter.Allow("second") {
		t.Fatal("the budget of the first source reached the budget of the second one")
	}
}
