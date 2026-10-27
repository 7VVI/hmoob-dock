package httpapi_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	api "hmoob-dock/internal/httpapi"
	"hmoob-dock/internal/metrics"
	"hmoob-dock/internal/protocol"
)

// metricHistoryStart is an exact minute boundary for the API tests.
var metricHistoryStart = time.Unix(1800000000, 0).UTC().Truncate(time.Minute)

// historyBucket builds one stored minute of a container.
func historyBucket(nodeID, containerID string, start time.Time, sampleCount int) metrics.Bucket {
	return metrics.Bucket{
		NodeID:      nodeID,
		ContainerID: containerID,
		Start:       start,
		Average: metrics.Values{
			CPUPercent: 12.5, MemoryBytes: 1024, NetRXPerSec: 2.5,
			NetTXPerSec: 3.5, IOReadPerSec: 4.5, IOWritePerSec: 5.5,
		},
		Peak: metrics.Values{
			CPUPercent: 25, MemoryBytes: 2048, NetRXPerSec: 7.5,
			NetTXPerSec: 8.5, IOReadPerSec: 9.5, IOWritePerSec: 10.5,
		},
		SampleCount:   sampleCount,
		ExpectedCount: 6,
	}
}

// saveMetricHistory writes buckets into the database of a test control端.
func saveMetricHistory(t *testing.T, db *sql.DB, buckets ...metrics.Bucket) {
	t.Helper()
	if err := metrics.NewStore(db).Save(context.Background(), buckets); err != nil {
		t.Fatalf("save the metric history: %v", err)
	}
}

// metricHistoryPath builds a history request.
func metricHistoryPath(nodeID, containerID string, from, to time.Time, extra map[string]string) string {
	values := url.Values{}
	values.Set("node_id", nodeID)
	values.Set("container_id", containerID)
	values.Set("from", from.UTC().Format(time.RFC3339))
	values.Set("to", to.UTC().Format(time.RFC3339))
	for key, value := range extra {
		values.Set(key, value)
	}
	return "/api/v1/metrics?" + values.Encode()
}

// enrollTestNode registers a standalone test node and returns its ID.
func enrollTestNode(t *testing.T, server *authServer, csrf string, identity protocol.NodeIdentity) string {
	t.Helper()
	token := server.createAgentToken(t, csrf)
	payload := server.enroll(t, token, identity)
	nodeID, _ := payload["node_id"].(string)
	if nodeID == "" {
		t.Fatalf("the enrollment answered %v, want a node", payload)
	}
	return nodeID
}

// decodeHistory reads a successful history answer.
func decodeHistory(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode the history: %v", err)
	}
	return payload
}

// expectErrorCode reads an error answer and checks the common shape.
func expectErrorCode(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != status {
		t.Fatalf("status = %d, want %d", response.StatusCode, status)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode the error: %v", err)
	}
	if payload["code"] != code {
		t.Fatalf("code = %v, want %q", payload["code"], code)
	}
	if message, _ := payload["message"].(string); message == "" {
		t.Fatal("the error has no message")
	}
}

func TestMetricsHistoryNeedsASession(t *testing.T) {
	server := newAuthServer(t)
	response := server.get(metricHistoryPath("n1", "c1", metricHistoryStart, metricHistoryStart.Add(time.Hour), nil))
	expectErrorCode(t, response, http.StatusUnauthorized, "unauthenticated")
}

func TestMetricsHistoryRejectsOtherMethods(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	response := server.post("/api/v1/metrics", map[string]any{}, withHeader("X-CSRF-Token", "unused"))
	expectErrorCode(t, response, http.StatusMethodNotAllowed, "method_not_allowed")
}

func TestMetricsHistoryValidatesTheQuery(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	from := metricHistoryStart
	to := metricHistoryStart.Add(time.Hour)
	cases := []struct {
		name  string
		query string
		code  string
	}{
		{"missing node", metricHistoryPath("", "c1", from, to, nil), "validation_failed"},
		{"missing container", metricHistoryPath("n1", "", from, to, nil), "validation_failed"},
		{"missing range", "/api/v1/metrics?node_id=n1&container_id=c1", "validation_failed"},
		{"malformed from", "/api/v1/metrics?node_id=n1&container_id=c1&from=yesterday&to=" + url.QueryEscape(to.Format(time.RFC3339)), "validation_failed"},
		{"non UTC from", "/api/v1/metrics?node_id=n1&container_id=c1&from=2026-01-01T00%3A00%3A00%2B08%3A00&to=" + url.QueryEscape(to.Format(time.RFC3339)), "validation_failed"},
		{"empty range", metricHistoryPath("n1", "c1", from, from, nil), "validation_failed"},
		{"window too large", metricHistoryPath("n1", "c1", from, from.Add(25*time.Hour), nil), "window_too_large"},
		{"zero limit", metricHistoryPath("n1", "c1", from, to, map[string]string{"limit": "0"}), "validation_failed"},
		{"huge limit", metricHistoryPath("n1", "c1", from, to, map[string]string{"limit": "100000"}), "validation_failed"},
		{"text limit", metricHistoryPath("n1", "c1", from, to, map[string]string{"limit": "many"}), "validation_failed"},
		{"negative offset", metricHistoryPath("n1", "c1", from, to, map[string]string{"offset": "-1"}), "validation_failed"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := server.get(testCase.query)
			expectErrorCode(t, response, http.StatusBadRequest, testCase.code)
		})
	}
}

func TestMetricsHistoryReportsCompletenessAndUnits(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := enrollTestNode(t, server, csrf, nodeIdentity)

	saveMetricHistory(t, server.db,
		historyBucket(nodeID, "c1", metricHistoryStart, 6),
		historyBucket(nodeID, "c1", metricHistoryStart.Add(2*time.Minute), 2),
	)

	response := server.get(metricHistoryPath(nodeID, "c1",
		metricHistoryStart, metricHistoryStart.Add(3*time.Minute), nil))
	payload := decodeHistory(t, response)

	if payload["node_id"] != nodeID || payload["container_id"] != "c1" {
		t.Fatalf("object = %v/%v, want %s/c1", payload["node_id"], payload["container_id"], nodeID)
	}
	units, _ := payload["units"].(map[string]any)
	if units["cpu_percent"] != "percent_of_core" || units["memory_bytes"] != "bytes" ||
		units["net_rx_per_sec"] != "bytes_per_second" {
		t.Fatalf("units = %v, want the units of every value", payload["units"])
	}

	buckets, _ := payload["buckets"].([]any)
	if len(buckets) != 2 {
		t.Fatalf("buckets = %d, want the two observed minutes and no zero row for the gap", len(buckets))
	}
	first, _ := buckets[0].(map[string]any)
	if first["start"] != metricHistoryStart.Format(time.RFC3339) {
		t.Fatalf("first start = %v, want %v", first["start"], metricHistoryStart)
	}
	if first["sample_count"] != float64(6) || first["expected_count"] != float64(6) || first["complete"] != true {
		t.Fatalf("complete bucket = %v, want 6/6 complete", first)
	}
	second, _ := buckets[1].(map[string]any)
	if second["sample_count"] != float64(2) || second["expected_count"] != float64(6) || second["complete"] != false {
		t.Fatalf("partial bucket = %v, want 2/6 incomplete", second)
	}
	if second["start"] != metricHistoryStart.Add(2*time.Minute).Format(time.RFC3339) {
		t.Fatalf("second start = %v, want the missing minute to stay a gap", second["start"])
	}
	average, _ := first["average"].(map[string]any)
	if average["cpu_percent"] != 12.5 {
		t.Fatalf("average cpu = %v, want 12.5", average["cpu_percent"])
	}
	peak, _ := first["peak"].(map[string]any)
	if peak["cpu_percent"] != 25.0 {
		t.Fatalf("peak cpu = %v, want 25", peak["cpu_percent"])
	}
}

func TestMetricsHistoryPaginates(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := enrollTestNode(t, server, csrf, nodeIdentity)

	for index := 0; index < 3; index++ {
		saveMetricHistory(t, server.db, historyBucket(nodeID, "c1",
			metricHistoryStart.Add(time.Duration(index)*time.Minute), 6))
	}
	from := metricHistoryStart
	to := metricHistoryStart.Add(3 * time.Minute)

	first := decodeHistory(t, server.get(metricHistoryPath(nodeID, "c1", from, to, map[string]string{
		"limit": "2", "offset": "0",
	})))
	buckets, _ := first["buckets"].([]any)
	if len(buckets) != 2 || first["has_more"] != true {
		t.Fatalf("first page = %d buckets, has_more = %v, want 2/true", len(buckets), first["has_more"])
	}
	second := decodeHistory(t, server.get(metricHistoryPath(nodeID, "c1", from, to, map[string]string{
		"limit": "2", "offset": "2",
	})))
	buckets, _ = second["buckets"].([]any)
	if len(buckets) != 1 || second["has_more"] != false {
		t.Fatalf("second page = %d buckets, has_more = %v, want 1/false", len(buckets), second["has_more"])
	}
}

func TestMetricsHistoryHidesAnUnknownNode(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	response := server.get(metricHistoryPath("missing-node", "c1",
		metricHistoryStart, metricHistoryStart.Add(time.Minute), nil))
	expectErrorCode(t, response, http.StatusNotFound, "node_not_found")
}

func TestMetricsHistoryDoesNotMixTheObjectsOfTwoNodes(t *testing.T) {
	control := newLogTestControl(t)

	// A second node has history for a container identifier that the first
	// node does not own.
	otherNodeID := enrollTestNode(t, control.authServer, control.csrf, protocol.NodeIdentity{
		EngineID: "engine-b",
		Hostname: "worker-2",
		Role:     protocol.RoleStandalone,
	})
	saveMetricHistory(t, control.db,
		historyBucket(otherNodeID, "c1", metricHistoryStart, 6),
		historyBucket(otherNodeID, "only-other", metricHistoryStart, 6),
	)
	from := metricHistoryStart
	to := metricHistoryStart.Add(time.Minute)

	// The first node owns c1, so the query is allowed but must return only
	// the rows of the first node. The row of the second node stays hidden.
	payload := decodeHistory(t, control.get(metricHistoryPath(control.nodeID, "c1", from, to, nil)))
	if buckets, _ := payload["buckets"].([]any); len(buckets) != 0 {
		t.Fatalf("the history of another node leaked into the answer: %v", buckets)
	}

	// The container of the second node is not in the inventory of the
	// first node, so the first node cannot name it at all.
	response := control.get(metricHistoryPath(control.nodeID, "only-other", from, to, nil))
	expectErrorCode(t, response, http.StatusNotFound, "container_not_found")

	// The second node is offline, but its own history stays readable.
	payload = decodeHistory(t, control.get(metricHistoryPath(otherNodeID, "only-other", from, to, nil)))
	if buckets, _ := payload["buckets"].([]any); len(buckets) != 1 {
		t.Fatalf("the history of the offline node = %v, want one bucket", buckets)
	}
}

func TestMetricsSamplesReachTheHistoryThroughTheAgentConnection(t *testing.T) {
	control := newLogTestControl(t)

	now := time.Now().UTC()
	firstAt := now.Add(-5 * time.Second)
	secondAt := now.Add(5 * time.Second)
	base := protocol.RawSample{
		ContainerID:   "c1",
		CollectedAt:   firstAt,
		CPUUsageNS:    1e9,
		SystemUsageNS: 10e9,
		OnlineCPUs:    2,
		MemoryUsage:   1000,
		NetRX:         1000,
		NetTX:         2000,
		IORead:        3000,
		IOWrite:       4000,
	}
	next := base
	next.CollectedAt = secondAt
	next.CPUUsageNS += 1e9
	next.SystemUsageNS += 10e9
	next.MemoryUsage = 2000
	next.NetRX += 1000
	next.NetTX += 2000
	next.IORead += 2000
	next.IOWrite += 4000

	sendTestAgentFrame(t, control.agent, protocol.TypeMetricsSample, base)
	sendTestAgentFrame(t, control.agent, protocol.TypeMetricsSample, next)

	deadline := time.Now().Add(5 * time.Second)
	for {
		latest, ok := api.MetricsLatestForTest(control.handler, control.nodeID, "c1")
		if ok && latest.Valid && latest.At.Equal(secondAt) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sample did not reach the live point: %+v/%v", latest, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := api.FlushMetricsForTest(control.handler, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("flush the metric minutes: %v", err)
	}

	from := now.Add(-2 * time.Minute).Truncate(time.Minute)
	to := now.Add(3 * time.Minute).Truncate(time.Minute)
	payload := decodeHistory(t, control.get(metricHistoryPath(control.nodeID, "c1", from, to, nil)))
	buckets, _ := payload["buckets"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("buckets = %v, want the one valid sample", buckets)
	}
	bucket, _ := buckets[0].(map[string]any)
	if bucket["sample_count"] != float64(1) || bucket["complete"] != false {
		t.Fatalf("bucket = %v, want one incomplete sample", bucket)
	}
	average, _ := bucket["average"].(map[string]any)
	if average["cpu_percent"] != 20.0 {
		t.Fatalf("average cpu = %v, want 20", average["cpu_percent"])
	}
	peak, _ := bucket["peak"].(map[string]any)
	if peak["cpu_percent"] != 20.0 {
		t.Fatalf("peak cpu = %v, want 20", peak["cpu_percent"])
	}
}

func TestMetricsHistoryHidesARevokedNode(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := enrollTestNode(t, server, csrf, nodeIdentity)
	if _, err := server.db.Exec(
		`UPDATE nodes SET revoked_at = ? WHERE id = ?`, time.Now().UnixMilli(), nodeID,
	); err != nil {
		t.Fatalf("revoke the node: %v", err)
	}

	response := server.get(metricHistoryPath(nodeID, "c1",
		metricHistoryStart, metricHistoryStart.Add(time.Minute), nil))
	expectErrorCode(t, response, http.StatusNotFound, "node_not_found")
}
