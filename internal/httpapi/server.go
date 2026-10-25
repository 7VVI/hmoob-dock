// Package httpapi exposes the HTTP interface of the control端.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"hmoob-dock/internal/config"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// healthTimeout bounds the database check of the health endpoint.
const healthTimeout = 2 * time.Second

// Server serves the control端 HTTP API.
//
// Queries run on a read-only pool that is opened next to the write pool, so
// a slow query cannot block a write. Later tasks extend the routing without
// changing the constructor.
type Server struct {
	db    *sql.DB
	reads *sql.DB
	box   *secret.Box
	cfg   config.Control
	mux   *http.ServeMux
}

// NewServer assembles the control端 HTTP handler.
func NewServer(db *sql.DB, box *secret.Box, cfg config.Control) (http.Handler, error) {
	if db == nil {
		return nil, errors.New("httpapi: a database handle is required")
	}
	if box == nil {
		return nil, errors.New("httpapi: a secret box is required")
	}
	if strings.TrimSpace(cfg.DBPath) == "" {
		return nil, errors.New("httpapi: a database path is required")
	}

	reads, err := store.OpenReader(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("httpapi: open the read pool: %w", err)
	}

	server := &Server{
		db:    db,
		reads: reads,
		box:   box,
		cfg:   cfg,
		mux:   http.NewServeMux(),
	}
	server.mux.HandleFunc("/healthz", server.handleHealth)
	server.mux.HandleFunc("/", server.handleNotFound)
	return server, nil
}

// ServeHTTP dispatches a request to the registered handlers.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Close releases the read pool of the server.
func (s *Server) Close() error {
	return s.reads.Close()
}

// handleHealth reports that the control端 is alive. It exposes neither the
// configuration nor the database location.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := s.reads.PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "数据库当前不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleNotFound answers unknown routes with the common error format.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "未找到请求的资源")
}

// writeJSON writes a JSON document with the given status code. The status
// line is sent before the body is encoded, so a failing encoder can only be
// reported through the server log of the caller.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeError writes the common error document.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{
		"code":    code,
		"message": message,
	})
}
