package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/metrics"
	"hmoob-dock/internal/protocol"
)

// The limits of the history API and of the metrics writer.
const (
	// metricsWindow is the longest time span one history request may ask.
	// The history is retained for the same span, so a larger request would
	// only hide that the older data does not exist any more.
	metricsWindow = 24 * time.Hour
	// metricsDefaultLimit is the number of the buckets of one page. A
	// complete day has one bucket per minute.
	metricsDefaultLimit = 1440
	// metricsMaxLimit is the largest page a request may ask.
	metricsMaxLimit = 1440
	// metricsFlushTimeout bounds one write of the completed minutes. The
	// store bounds its own batches as well; this deadline keeps the writer
	// from waiting for the database forever.
	metricsFlushTimeout = 2 * time.Second
	// metricsQueryTimeout bounds the reads of one history request.
	metricsQueryTimeout = 3 * time.Second
	// metricsFlushInterval is the time between two writes of the completed
	// minutes.
	metricsFlushInterval = 15 * time.Second
)

// metricsUnits names the unit of every value of a bucket. The frontend uses
// the names for the axes and the labels of the charts.
var metricsUnits = map[string]string{
	"cpu_percent":      "percent_of_core",
	"memory_bytes":     "bytes",
	"net_rx_per_sec":   "bytes_per_second",
	"net_tx_per_sec":   "bytes_per_second",
	"io_read_per_sec":  "bytes_per_second",
	"io_write_per_sec": "bytes_per_second",
}

// hubContainers answers the inventory check of the metrics ingestion from
// the live connection of the agents.
type hubContainers struct {
	hub *agentlink.Hub
}

// HasContainer reports whether the recent inventory of the node holds the
// container.
func (h hubContainers) HasContainer(nodeID, containerID string) bool {
	if h.hub == nil {
		return false
	}
	found, known := h.hub.RecentContainer(nodeID, containerID)
	return known && found
}

// metricsBucketAnswer is one minute in the history answer.
type metricsBucketAnswer struct {
	// Start is the start of the minute in UTC.
	Start string `json:"start"`
	// Average is the mean of the valid samples of the minute.
	Average metrics.Values `json:"average"`
	// Peak is the largest value of every field of the minute.
	Peak metrics.Values `json:"peak"`
	// SampleCount is the number of the valid samples.
	SampleCount int `json:"sample_count"`
	// ExpectedCount is the number of the samples a complete minute holds.
	ExpectedCount int `json:"expected_count"`
	// Complete reports whether the minute holds every expected sample.
	Complete bool `json:"complete"`
}

// registerMetricsRoutes mounts the history of the container metrics.
func (s *Server) registerMetricsRoutes() {
	s.mux.HandleFunc("/api/v1/metrics", s.handleMetricsHistory)
}

// handleMetricsSample accepts one raw sample of an authenticated agent.
//
// The node of the sample is the node of the connection, not a field of the
// payload. The sample is handed to the ingestion, which validates the clock
// and the membership of the container in the recent inventory of the node.
func (s *Server) handleMetricsSample(_ context.Context, nodeID string, envelope protocol.Envelope) error {
	var sample protocol.RawSample
	if err := json.Unmarshal(envelope.Payload, &sample); err != nil {
		return fmt.Errorf("httpapi: decode the metrics sample: %w", err)
	}
	if strings.TrimSpace(sample.ContainerID) == "" {
		return errors.New("httpapi: the metrics sample names no container")
	}
	if s.metricsIngest != nil {
		s.metricsIngest.Observe(nodeID, time.Now().UTC(), sample)
	}
	return nil
}

// startMetricsWriter starts the background writer of the completed minutes.
func (s *Server) startMetricsWriter() {
	if s.metricsIngest == nil {
		return
	}
	s.backgroundWorkers.Add(1)
	go func() {
		defer s.backgroundWorkers.Done()
		ticker := time.NewTicker(metricsFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case now := <-ticker.C:
				ctx, cancel := context.WithTimeout(s.ctx, metricsFlushTimeout)
				err := s.metricsIngest.Flush(ctx, now)
				cancel()
				if err != nil && s.ctx.Err() == nil {
					log.Printf("control: flush the completed metric minutes: %v", err)
				}
			}
		}
	}()
}

// closeMetricsWriter stops the background writer and saves the last
// completed minutes. The current partial minute stays open: it is not a
// complete observation and would be written again after a restart.
func (s *Server) closeMetricsWriter() {
	s.backgroundWorkers.Wait()
	if s.metricsIngest == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), metricsFlushTimeout)
	defer cancel()
	if err := s.metricsIngest.FlushCompleted(ctx, time.Now()); err != nil {
		log.Printf("control: flush the metric minutes on shutdown: %v", err)
	}
}

// handleMetricsHistory answers the minute history of one container.
//
// The request names the node, the container and a half-open window in UTC.
// The window is limited to the retention of the history, the answer is
// paginated and every bucket carries its sample completeness. The endpoint
// belongs to the administrator and never exposes the objects of another
// node: the history is read with the node and the container together.
func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}
	if _, ok := s.requireSession(w, r); !ok {
		return
	}

	query := r.URL.Query()
	nodeID := strings.TrimSpace(query.Get("node_id"))
	containerID := strings.TrimSpace(query.Get("container_id"))
	fields := map[string]string{}
	if nodeID == "" {
		fields["node_id"] = "必须指定节点"
	}
	if containerID == "" {
		fields["container_id"] = "必须指定容器"
	}

	var from, to time.Time
	if raw := strings.TrimSpace(query.Get("from")); raw == "" {
		fields["from"] = "必须指定开始时间"
	} else if parsed, err := parseMetricsTime(raw); err != nil {
		fields["from"] = "时间必须是 UTC RFC3339"
	} else {
		from = parsed
	}
	if raw := strings.TrimSpace(query.Get("to")); raw == "" {
		fields["to"] = "必须指定结束时间"
	} else if parsed, err := parseMetricsTime(raw); err != nil {
		fields["to"] = "时间必须是 UTC RFC3339"
	} else {
		to = parsed
	}

	limit := metricsDefaultLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > metricsMaxLimit {
			fields["limit"] = fmt.Sprintf("分页大小必须在 1 到 %d 之间", metricsMaxLimit)
		} else {
			limit = value
		}
	}
	offset := 0
	if raw := strings.TrimSpace(query.Get("offset")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			fields["offset"] = "偏移量不能为负数"
		} else {
			offset = value
		}
	}
	if len(fields) > 0 {
		writeErrorFields(w, http.StatusBadRequest, "validation_failed", "指标查询的参数无效", fields)
		return
	}
	if !to.After(from) {
		writeErrorFields(w, http.StatusBadRequest, "validation_failed", "指标查询的参数无效", map[string]string{
			"to": "结束时间必须晚于开始时间",
		})
		return
	}
	if to.Sub(from) > metricsWindow {
		writeErrorFields(w, http.StatusBadRequest, "window_too_large", "指标查询窗口不能超过 24 小时", map[string]string{
			"to": "窗口不能超过 24 小时",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), metricsQueryTimeout)
	defer cancel()

	available, err := s.metricsNodeAvailable(ctx, nodeID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取节点")
		return
	}
	if !available {
		writeError(w, http.StatusNotFound, "node_not_found", "未找到该节点")
		return
	}

	// The container has to belong to the node. A live inventory entry
	// proves it; a stored minute proves it as well, so the history of a
	// removed container stays readable. A pair without either proof is
	// refused before the history of another node can be named.
	found, known := s.hub.RecentContainer(nodeID, containerID)
	if !known || !found {
		hasHistory, err := s.metricsHistory.HasHistory(ctx, nodeID, containerID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "无法读取指标历史")
			return
		}
		if !hasHistory {
			writeError(w, http.StatusNotFound, "container_not_found", "该容器不属于指定节点")
			return
		}
	}

	buckets, err := s.metricsHistory.Query(ctx, nodeID, containerID, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "无法读取指标历史")
		return
	}

	page := make([]metrics.Bucket, 0, limit)
	hasMore := false
	if offset < len(buckets) {
		end := offset + limit
		if end > len(buckets) {
			end = len(buckets)
		}
		page = buckets[offset:end]
		hasMore = end < len(buckets)
	}

	answer := make([]metricsBucketAnswer, 0, len(page))
	for _, bucket := range page {
		answer = append(answer, metricsBucketAnswer{
			Start:         formatTimestamp(bucket.Start),
			Average:       bucket.Average,
			Peak:          bucket.Peak,
			SampleCount:   bucket.SampleCount,
			ExpectedCount: bucket.ExpectedCount,
			Complete:      bucket.Complete(),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":      nodeID,
		"container_id": containerID,
		"from":         formatTimestamp(from),
		"to":           formatTimestamp(to),
		"units":        metricsUnits,
		"buckets":      answer,
		"limit":        limit,
		"offset":       offset,
		"has_more":     hasMore,
	})
}

// metricsNodeAvailable reports whether the node is enrolled and not revoked.
func (s *Server) metricsNodeAvailable(ctx context.Context, nodeID string) (bool, error) {
	var revokedAt sql.NullInt64
	err := s.reads.QueryRowContext(ctx,
		`SELECT revoked_at FROM nodes WHERE id = ?`, nodeID).Scan(&revokedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("httpapi: read the node of the metrics: %w", err)
	}
	return !revokedAt.Valid, nil
}

// parseMetricsTime reads an RFC3339 timestamp in UTC.
func parseMetricsTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	if parsed.Location() != time.UTC {
		return time.Time{}, errors.New("the time is not in UTC")
	}
	return parsed.UTC(), nil
}
