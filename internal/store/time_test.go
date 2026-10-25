package store

import (
	"testing"
	"time"
)

func TestTimeToMillisStoresUTC(t *testing.T) {
	moment := time.Date(2026, 10, 25, 11, 5, 30, 123_000_000, time.FixedZone("east", 8*60*60))

	millis := TimeToMillis(moment)
	if millis != 1792897530123 {
		t.Fatalf("milliseconds = %d, want 1792897530123", millis)
	}
}

func TestTimeFromMillisReadsBackTheSameMoment(t *testing.T) {
	moment := time.Date(2026, 10, 25, 11, 5, 30, 123_000_000, time.UTC)

	read := TimeFromMillis(TimeToMillis(moment))
	if !read.Equal(moment) {
		t.Fatalf("read time = %v, want %v", read, moment)
	}
	if read.Location() != time.UTC {
		t.Fatalf("read location = %v, want UTC", read.Location())
	}
}
