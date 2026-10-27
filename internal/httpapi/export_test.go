package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"hmoob-dock/internal/metrics"
)

// SetLogReconnectGraceForTest replaces the grace period of a lost agent and
// returns the function that restores it. The tests of the reconnect and the
// disconnect of an agent need a grace period that fits into a test.
func SetLogReconnectGraceForTest(grace time.Duration) func() {
	previous := logReconnectGrace
	logReconnectGrace = grace
	return func() { logReconnectGrace = previous }
}

// FlushMetricsForTest writes the completed minutes of the metrics ingest of
// a server. The test controls the clock, so it does not wait for a real
// minute to end.
func FlushMetricsForTest(handler http.Handler, now time.Time) error {
	server, ok := handler.(*Server)
	if !ok {
		return errors.New("httpapi: the handler is not a server")
	}
	if server.metricsIngest == nil {
		return errors.New("httpapi: the server has no metrics ingest")
	}
	return server.metricsIngest.FlushCompleted(context.Background(), now)
}

// MetricsLatestForTest returns the newest valid live point of a container
// of a server.
func MetricsLatestForTest(handler http.Handler, nodeID, containerID string) (metrics.Point, bool) {
	server, ok := handler.(*Server)
	if !ok || server.metricsIngest == nil {
		return metrics.Point{}, false
	}
	return server.metricsIngest.Latest(nodeID, containerID)
}
