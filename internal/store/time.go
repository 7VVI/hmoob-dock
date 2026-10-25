package store

import "time"

// The schema stores every timestamp as an integer that holds the UTC
// milliseconds since the Unix epoch. The conversions live in this file so
// that every package writes and reads timestamps in the same way.

// TimeToMillis converts a time into the integer representation of the
// schema. The time is converted to UTC first, so the stored value does not
// depend on the location of the caller.
func TimeToMillis(t time.Time) int64 {
	return t.UTC().UnixMilli()
}

// TimeFromMillis converts a stored timestamp back into a UTC time.
func TimeFromMillis(millis int64) time.Time {
	return time.UnixMilli(millis).UTC()
}
