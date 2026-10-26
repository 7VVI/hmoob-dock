// Package httpapi exposes the HTTP interface of the control端.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/auth"
	"hmoob-dock/internal/config"
	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// readinessTimeout bounds the database read of the readiness endpoint.
const readinessTimeout = 2 * time.Second

// Server serves the control端 HTTP API.
//
// Queries run on a read-only pool that is opened next to the write pool, so
// a slow query cannot block a write. The constructor proves that both pools
// address the same database, because a silent mismatch would let the server
// answer from a database other than the one it writes to. Later tasks extend
// the routing without changing the constructor.
type Server struct {
	db    *sql.DB
	reads *sql.DB
	box   *secret.Box
	cfg   config.Control
	mux   *http.ServeMux
	// auth manages the administrator, the sessions and the CSRF tokens.
	auth *auth.Service
	// loginLimiter counts the failed logins of a source.
	loginLimiter *auth.Limiter
	// preInitLimiter counts the login attempts of a source that happen
	// before the administrator exists. It has its own budget, so a flood of
	// anonymous requests cannot use up the budget of the administrator.
	preInitLimiter *auth.Limiter
	// enrollLimiter counts the failed enrollments of a source. It has its
	// own budget as well: the endpoint belongs to a machine and does not
	// use the session of the administrator, so its flood must not touch the
	// budget of the login.
	enrollLimiter *auth.Limiter
	// trustedProxies are the networks of the reverse proxies whose
	// forwarding headers the control端 believes. The list is empty by
	// default, so no forwarding header of a peer is believed.
	trustedProxies trustedProxyNetworks
	// hostWarning makes sure that a request host that differs from the
	// configured public address is reported only once.
	hostWarning sync.Once
	// enrollment manages the enrollment tokens and the credentials of the
	// nodes.
	enrollment *agentlink.Enrollment
	// hub keeps the live connections of the agents.
	hub *agentlink.Hub
	// apps owns the applications of the control端 and their deployments.
	apps *deploy.Service
	// dispatcher sends the deployments to the agents and applies their
	// reports.
	dispatcher *deploy.Dispatcher
	// events hands the changes of the deployments to the browser.
	events *eventBroker
	// ctx is the context of the server. The background work of the server
	// stops when it is canceled.
	ctx context.Context
	// cancel stops the background work of the server.
	cancel context.CancelFunc
}

// NewServer assembles the control端 HTTP handler.
//
// The write pool and the read pool must address the same database. A
// database handle does not expose its data source, so the identity is proven
// with a real query on both pools instead of being assumed. A mismatch is
// reported as an error, so a misconfigured control端 stops instead of
// serving data from an unexpected database.
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

	writeFile, writeMigrations, err := probeDatabase(db)
	if err != nil {
		return nil, fmt.Errorf("httpapi: inspect the write pool: %w", err)
	}

	// The read pool must not create a database that nobody uses, so the
	// file has to exist before the pool is opened.
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return nil, fmt.Errorf("httpapi: the database %s is not accessible: %w", cfg.DBPath, err)
	}

	reads, err := store.OpenReader(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("httpapi: open the read pool: %w", err)
	}

	readFile, readMigrations, err := probeDatabase(reads)
	if err != nil {
		_ = reads.Close()
		return nil, fmt.Errorf("httpapi: inspect the read pool: %w", err)
	}
	if !samePath(writeFile, readFile) {
		_ = reads.Close()
		return nil, fmt.Errorf(
			"httpapi: the read pool addresses %s while the write pool addresses %s",
			readFile, writeFile,
		)
	}
	if writeMigrations != readMigrations {
		_ = reads.Close()
		return nil, errors.New("httpapi: the read pool and the write pool disagree about the applied migrations")
	}

	// The networks of the reverse proxies are parsed before the server
	// serves anything, so a malformed network stops the control端
	// instead of silently changing the trust.
	trusted, err := parseTrustedProxies(cfg.TrustedProxyCIDRs)
	if err != nil {
		_ = reads.Close()
		return nil, err
	}

	server := &Server{
		db:             db,
		reads:          reads,
		box:            box,
		cfg:            cfg,
		mux:            http.NewServeMux(),
		auth:           auth.New(db, time.Now),
		trustedProxies: trustedProxyNetworks(trusted),
		enrollment:     agentlink.NewEnrollment(db, time.Now),
		// The hub only reads the records of the nodes, so it uses the read
		// pool: a slow read never holds back a write.
		hub:    agentlink.NewHub(reads),
		events: newEventBroker(),
	}
	server.ctx, server.cancel = context.WithCancel(context.Background())
	server.loginLimiter = auth.NewLimiter(loginMaxAttempts, loginRateWindow, time.Now)
	server.preInitLimiter = auth.NewLimiter(loginMaxAttempts, loginRateWindow, time.Now)
	server.enrollLimiter = auth.NewLimiter(enrollMaxAttempts, enrollRateWindow, time.Now)
	// The deployment service reads the state of the targets through the
	// hub, so a preview and a submission see the state that the agents
	// report.
	server.apps = deploy.New(db, box, server.hub)
	// The dispatcher sends the deployments through the hub of the agents,
	// so the control端 holds no connection of its own.
	server.dispatcher = deploy.NewDispatcher(server.apps, server.hub)
	server.mux.HandleFunc("/healthz", server.handleHealth)
	server.mux.HandleFunc("/readyz", server.handleReady)
	server.registerAuthRoutes()
	server.registerAgentRoutes()
	server.registerApplicationRoutes()
	server.registerDeploymentRoutes()
	server.registerEventRoutes()
	server.mux.HandleFunc("/", server.handleNotFound)

	// The reports of the agents reach the dispatcher, and the dispatcher
	// tells the browser about the changes of the deployments.
	server.subscribeDeployments()

	// A revoked credential closes the connection of its node, so a revoked
	// agent cannot keep talking to the control端.
	server.enrollment.OnRevoke(server.hub.Disconnect)

	// The bootstrap credential of the deployment opens the first
	// initialization. It is only stored while no administrator exists.
	if err := server.seedBootstrap(); err != nil {
		server.cancel()
		_ = reads.Close()
		return nil, err
	}
	return server, nil
}

// ServeHTTP dispatches a request to the registered handlers.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Initialized reports whether the administrator of the control端 exists
// already. The command uses it to report the state of the deployment
// without reading the bootstrap credential a second time.
func (s *Server) Initialized(ctx context.Context) (bool, error) {
	return s.auth.Initialized(ctx)
}

// Close releases the read pool of the server.
func (s *Server) Close() error {
	// The background work of the server stops before the database is
	// closed, so it does not write into a closed database.
	s.cancel()
	return s.reads.Close()
}

// Recover reconciles the deployments that did not finish.
//
// The control端 asks the nodes about the tasks it does not know the outcome
// of, so a restart does not run a deployment a second time. The command
// asks the server to do it after it started, because the nodes may not be
// connected yet.
func (s *Server) Recover(ctx context.Context) error {
	return s.dispatcher.Recover(ctx)
}

// handleHealth reports that the control端 is alive. It answers without
// touching the database, so a locked or busy database never marks the
// process as unhealthy. It exposes neither the configuration nor the
// database location.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady reports whether the control端 can serve requests. It performs
// a real read instead of a ping, so a database that lost its schema is
// reported as not ready.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "该端点只接受 GET 请求")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	if _, err := readMigrationCount(ctx, s.reads); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "数据库当前不可用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleNotFound answers unknown routes with the common error format.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "未找到请求的资源")
}

// probeDatabase returns the file of the main database and the number of
// applied migrations. The migration count comes from a real read, so a
// database without the expected schema is detected instead of accepted.
func probeDatabase(db *sql.DB) (string, int, error) {
	file, err := databaseFile(db)
	if err != nil {
		return "", 0, err
	}
	migrations, err := readMigrationCount(context.Background(), db)
	if err != nil {
		return "", 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	return file, migrations, nil
}

// databaseFile returns the file of the main database of db.
func databaseFile(db *sql.DB) (string, error) {
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		return "", fmt.Errorf("read the database list: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", fmt.Errorf("scan the database list: %w", err)
		}
		if name == "main" {
			if file == "" {
				return "", errors.New("the main database is not backed by a file")
			}
			return file, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read the database list: %w", err)
	}
	return "", errors.New("the database list does not contain the main database")
}

// readMigrationCount reads the number of applied migrations. It is a real
// read of the schema table and not a ping of the connection.
func readMigrationCount(ctx context.Context, db *sql.DB) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// samePath reports whether two paths address the same location.
func samePath(a, b string) bool {
	left, err := canonicalPath(a)
	if err != nil {
		return false
	}
	right, err := canonicalPath(b)
	if err != nil {
		return false
	}
	return left == right
}

// canonicalPath turns a path into a comparable form: absolute, free of
// symbolic links and, on the platforms that ignore the letter case, lower
// case.
func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		absolute = strings.ToLower(absolute)
	}
	return absolute, nil
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

// writeErrorFields writes the common error document with a message for
// every field that failed the validation.
func writeErrorFields(w http.ResponseWriter, status int, code, message string, fields map[string]string) {
	payload := map[string]any{
		"code":    code,
		"message": message,
	}
	if len(fields) > 0 {
		payload["fields"] = fields
	}
	writeJSON(w, status, payload)
}
