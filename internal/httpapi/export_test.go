package httpapi

import "time"

// SetLogReconnectGraceForTest replaces the grace period of a lost agent and
// returns the function that restores it. The tests of the reconnect and the
// disconnect of an agent need a grace period that fits into a test.
func SetLogReconnectGraceForTest(grace time.Duration) func() {
	previous := logReconnectGrace
	logReconnectGrace = grace
	return func() { logReconnectGrace = previous }
}
