package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/config"
	"hmoob-dock/internal/httpapi"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// bootstrapCredential is the one time credential of the test deployments.
const bootstrapCredential = "bootstrap-token-of-the-test"

// adminPassword is the password of the administrator of the tests.
const adminPassword = "a-strong-test-password"

// fakeEngine is the Docker engine of the tests.
type fakeEngine struct {
	identity  protocol.NodeIdentity
	inventory protocol.Inventory
}

// Identity returns the identity of the engine.
func (f fakeEngine) Identity(context.Context) (protocol.NodeIdentity, error) {
	return f.identity, nil
}

// Inventory returns the containers of the engine.
func (f fakeEngine) Inventory(context.Context) (protocol.Inventory, error) {
	return f.inventory, nil
}

// testControl is a control端 that accepts the agents.
type testControl struct {
	server *httptest.Server
	// db is the database of the control端.
	db *sql.DB
	// connections counts the connection attempts of the agents.
	connections atomic.Int64
}

// newTestControl starts a control端 that accepts the agents.
func newTestControl(t *testing.T) *testControl {
	t.Helper()

	db, handler := newControl(t)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	return &testControl{server: server, db: db}
}

// newDroppingControl starts a control端 that closes the connection of an
// agent shortly after it opened it.
func newDroppingControl(t *testing.T) *testControl {
	t.Helper()

	db, handler := newControl(t)
	enrollment := agentlink.NewEnrollment(db, time.Now)
	hub := agentlink.NewHub(db)

	control := &testControl{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/connect", func(w http.ResponseWriter, r *http.Request) {
		control.connections.Add(1)

		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		credential, err := enrollment.Authenticate(r.Context(), token)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		go func() {
			time.Sleep(100 * time.Millisecond)
			_ = conn.Close(websocket.StatusNormalClosure, "the test drops the connection")
		}()
		_ = hub.Serve(r.Context(), credential.NodeID, conn)
	})
	// The registration of the agent belongs to the control端 itself.
	mux.Handle("/", handler)

	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	control.server = server

	return control
}

// newControl builds the database and the handler of a control端.
func newControl(t *testing.T) (*sql.DB, http.Handler) {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "control.db")
	bootstrapPath := filepath.Join(dir, "bootstrap.token")
	if err := os.WriteFile(bootstrapPath, []byte(bootstrapCredential), 0o600); err != nil {
		t.Fatalf("write the bootstrap credential: %v", err)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	box, err := secret.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	handler, err := httpapi.NewServer(db, box, config.Control{
		DBPath:        dbPath,
		KeyFile:       filepath.Join(dir, "master.key"),
		BootstrapFile: bootstrapPath,
	})
	if err != nil {
		t.Fatalf("httpapi.NewServer: %v", err)
	}
	if closer, ok := handler.(io.Closer); ok {
		t.Cleanup(func() { closer.Close() })
	}
	return db, handler
}

// writeAuthority writes the certificate of the control端 into a file that
// the agent trusts.
func (c *testControl) writeAuthority(t *testing.T, path string) {
	t.Helper()

	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.server.Certificate().Raw})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write the certificate authority: %v", err)
	}
}

// login initializes the administrator of the control端 and logs it in. The
// answer holds the client of the session and its CSRF token.
func (c *testControl) login(t *testing.T) (*http.Client, string) {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := c.server.Client()
	client.Jar = jar

	initialize := c.request(t, client, http.MethodPost, "/api/v1/auth/initialize", map[string]string{
		"bootstrap_token": bootstrapCredential,
		"password":        adminPassword,
	})
	defer initialize.Body.Close()
	if initialize.StatusCode != http.StatusOK {
		t.Fatalf("the initialization returned %d, want %d", initialize.StatusCode, http.StatusOK)
	}

	login := c.request(t, client, http.MethodPost, "/api/v1/auth/login", map[string]string{
		"password": adminPassword,
	})
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("the login returned %d, want %d", login.StatusCode, http.StatusOK)
	}
	var payload struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(login.Body).Decode(&payload); err != nil {
		t.Fatalf("decode the login answer: %v", err)
	}
	if payload.CSRF == "" {
		t.Fatal("the login answer holds no CSRF token")
	}
	return client, payload.CSRF
}

// request sends a request to the control端 from its own origin.
func (c *testControl) request(t *testing.T, client *http.Client, method, path string, payload any) *http.Response {
	t.Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode the payload: %v", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, c.server.URL+path, body)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Origin", c.server.URL)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	return response
}

// nodes reads the nodes of the control端.
func (c *testControl) nodes(t *testing.T, client *http.Client) []map[string]any {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, c.server.URL+"/api/v1/nodes", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the node list returned %d, want %d", response.StatusCode, http.StatusOK)
	}
	var payload struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode the node list: %v", err)
	}
	return payload.Nodes
}

// agentConfig builds the configuration of an agent of the test.
func agentConfig(t *testing.T, control *testControl, dir string) config.Agent {
	t.Helper()

	control.writeAuthority(t, filepath.Join(dir, "ca.pem"))
	return config.Agent{
		ServerURL:      control.server.URL,
		CAFile:         filepath.Join(dir, "ca.pem"),
		CredentialFile: filepath.Join(dir, "credential.json"),
		EnrollmentFile: filepath.Join(dir, "enrollment.token"),
		DockerSocket:   "",
		WorkRoot:       filepath.Join(dir, "work"),
	}
}

// issueEnrollmentToken hands out an enrollment token and writes it into the
// file of the agent.
func issueEnrollmentToken(t *testing.T, control *testControl, path string) string {
	t.Helper()

	enrollment := agentlink.NewEnrollment(control.db, time.Now)
	token, err := enrollment.Issue(context.Background())
	if err != nil {
		t.Fatalf("issue the enrollment token: %v", err)
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write the enrollment token: %v", err)
	}
	return token
}

// waitFor waits until the condition holds.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout while waiting for %s", what)
}

// captureLog collects the output of the log while the test runs.
func captureLog(t *testing.T) *strings.Builder {
	t.Helper()

	builder := &strings.Builder{}
	previous := log.Writer()
	log.SetOutput(builder)
	t.Cleanup(func() { log.SetOutput(previous) })
	return builder
}

// foreignAuthority returns the certificate of an authority that signed
// nothing.
func foreignAuthority(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the key of the authority: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "foreign authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the certificate of the authority: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTheAgentRegistersItselfAndReportsItsNode(t *testing.T) {
	control := newTestControl(t)
	dir := t.TempDir()
	cfg := agentConfig(t, control, dir)
	issueEnrollmentToken(t, control, cfg.EnrollmentFile)

	identity := protocol.NodeIdentity{EngineID: "engine-a", Hostname: "worker-1", Role: protocol.RoleStandalone}
	engine := fakeEngine{
		identity: identity,
		inventory: protocol.Inventory{
			Identity:    identity,
			Containers:  []protocol.Container{{ID: "c1", Name: "web", Image: "nginx", State: "running"}},
			CollectedAt: time.Now().UTC(),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := run(ctx, cfg, engine); err != nil {
			t.Errorf("the agent stopped: %v", err)
		}
	}()

	client, _ := control.login(t)

	waitFor(t, "the node to be connected", func() bool {
		nodes := control.nodes(t, client)
		if len(nodes) != 1 {
			return false
		}
		return nodes[0]["connected"] == true
	})

	// The inventory of the node arrives at the control端.
	waitFor(t, "the inventory of the node", func() bool {
		nodes := control.nodes(t, client)
		if len(nodes) != 1 {
			return false
		}
		return nodes[0]["container_count"] == float64(1)
	})

	// The credential of the agent is stored in a file of its own.
	credential, err := LoadCredential(cfg.CredentialFile)
	if err != nil {
		t.Fatalf("read the credential of the agent: %v", err)
	}
	if credential.NodeID == "" || credential.Token == "" {
		t.Fatalf("the credential = %+v, want a node ID and a token", credential)
	}
}

func TestTheAgentRefusesACertificateOfAnotherAuthority(t *testing.T) {
	control := newTestControl(t)
	dir := t.TempDir()
	cfg := agentConfig(t, control, dir)
	issueEnrollmentToken(t, control, cfg.EnrollmentFile)

	// The agent trusts another certificate authority, so the certificate
	// of the control端 is not valid for it.
	if err := os.WriteFile(cfg.CAFile, foreignAuthority(t), 0o600); err != nil {
		t.Fatalf("write the certificate authority: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := run(ctx, cfg, fakeEngine{})
	if err == nil {
		t.Fatal("the agent talked to a control端 with a foreign certificate")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the agent reported %v, want a complaint about the certificate", err)
	}
}

func TestTheAgentRefusesAHostNameThatTheCertificateDoesNotCover(t *testing.T) {
	control := newTestControl(t)
	dir := t.TempDir()
	cfg := agentConfig(t, control, dir)
	issueEnrollmentToken(t, control, cfg.EnrollmentFile)

	// The certificate of the control端 names the address of the test, not
	// the host name that the agent uses.
	cfg.ServerURL = strings.Replace(control.server.URL, "127.0.0.1", "localhost", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err := run(ctx, cfg, fakeEngine{})
	if err == nil {
		t.Fatal("the agent talked to a control端 with another host name")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the agent reported %v, want a complaint about the certificate", err)
	}
}

func TestTheAgentNeverSkipsTheVerificationOfACertificate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, foreignAuthority(t), 0o600); err != nil {
		t.Fatalf("write the certificate authority: %v", err)
	}

	client, err := newHTTPClient(config.Agent{CAFile: path})
	if err != nil {
		t.Fatalf("build the client of the agent: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the transport of the agent = %T, want an *http.Transport", client.Transport)
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("the agent has no TLS configuration")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("the agent skips the verification of the certificate")
	}
	if transport.TLSClientConfig.ServerName != "" {
		t.Fatalf("the agent overrides the host name of the control端 with %q", transport.TLSClientConfig.ServerName)
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("the agent does not trust the certificate authority of the configuration")
	}
}

func TestTheAgentReconnectsAfterADroppedConnection(t *testing.T) {
	control := newDroppingControl(t)
	dir := t.TempDir()
	cfg := agentConfig(t, control, dir)
	issueEnrollmentToken(t, control, cfg.EnrollmentFile)

	engine := fakeEngine{identity: protocol.NodeIdentity{EngineID: "engine-a"}}

	// The agent reports the end of a connection in the log. The test
	// collects the log, so the output of the test stays clean.
	captureLog(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := run(ctx, cfg, engine); err != nil {
			t.Errorf("the agent stopped: %v", err)
		}
	}()

	waitFor(t, "the agent to connect", func() bool {
		return control.connections.Load() >= 1
	})
	waitFor(t, "the agent to connect again", func() bool {
		return control.connections.Load() >= 2
	})
}

func TestTheBackoffGrowsAndIsCapped(t *testing.T) {
	first := backoff(0)
	if first < 500*time.Millisecond || first > time.Second {
		t.Fatalf("the first delay = %s, want between 500ms and 1s", first)
	}

	second := backoff(1)
	if second < time.Second || second > 2*time.Second {
		t.Fatalf("the second delay = %s, want between 1s and 2s", second)
	}

	// The delay grows with every attempt and is capped at the maximum.
	for attempt := 0; attempt < 20; attempt++ {
		delay := backoff(attempt)
		if delay > reconnectMaxDelay {
			t.Fatalf("the delay of the attempt %d = %s, want at most %s", attempt, delay, reconnectMaxDelay)
		}
		if attempt >= 5 && delay < reconnectMaxDelay/2 {
			t.Fatalf("the delay of the attempt %d = %s, want at least %s", attempt, delay, reconnectMaxDelay/2)
		}
	}
}

func TestTheInventoryIsSplitIntoBatchesThatFitIntoAMessage(t *testing.T) {
	containers := make([]protocol.Container, 0, 20)
	for index := 0; index < 20; index++ {
		containers = append(containers, protocol.Container{
			ID:    fmt.Sprintf("container-%d", index),
			Name:  fmt.Sprintf("name-%d", index),
			Image: "registry.example.com/team/image:latest",
			State: "running",
			Labels: map[string]string{
				"com.example.description": strings.Repeat("x", 60_000),
			},
		})
	}
	inventory := protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-a"},
		Containers:  containers,
		CollectedAt: time.Now().UTC(),
	}

	batches, err := splitInventory(inventory)
	if err != nil {
		t.Fatalf("split the inventory: %v", err)
	}
	if len(batches) < 2 {
		t.Fatalf("batches = %d, want more than one", len(batches))
	}

	total := 0
	for index, batch := range batches {
		if batch.BatchIndex != index {
			t.Fatalf("the batch index = %d, want %d", batch.BatchIndex, index)
		}
		if batch.BatchCount != len(batches) {
			t.Fatalf("the batch count = %d, want %d", batch.BatchCount, len(batches))
		}
		if batch.Complete != (index == len(batches)-1) {
			t.Fatalf("the batch %d reports complete = %v", index, batch.Complete)
		}
		if batch.SnapshotID == "" {
			t.Fatal("the batch names no snapshot")
		}
		encoded, err := json.Marshal(batch)
		if err != nil {
			t.Fatalf("encode the batch: %v", err)
		}
		if len(encoded) > protocol.MaxPayloadSize {
			t.Fatalf("the batch holds %d bytes, want at most %d", len(encoded), protocol.MaxPayloadSize)
		}
		total += len(batch.Containers)
	}
	if total != len(containers) {
		t.Fatalf("the batches hold %d containers, want %d", total, len(containers))
	}
}

func TestTheInventoryOfAnEmptyNodeIsOneBatch(t *testing.T) {
	batches, err := splitInventory(protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-a"},
		Containers:  []protocol.Container{},
		CollectedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("split the inventory: %v", err)
	}
	if len(batches) != 1 {
		t.Fatalf("batches = %d, want 1", len(batches))
	}
	if !batches[0].Complete {
		t.Fatal("the only batch is not marked as complete")
	}
}

func TestTheCredentialFileIsNotReadableByOthers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")

	if err := SaveCredential(path, Credential{NodeID: "node-1", Token: "secret"}); err != nil {
		t.Fatalf("save the credential: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("inspect the credential file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("the permissions of the credential file = %o, want 600", info.Mode().Perm())
	}

	credential, err := LoadCredential(path)
	if err != nil {
		t.Fatalf("load the credential: %v", err)
	}
	if credential.NodeID != "node-1" || credential.Token != "secret" {
		t.Fatalf("the credential = %+v, want the saved one", credential)
	}
}

func TestTheCredentialFileIsReplacedAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")

	if err := SaveCredential(path, Credential{NodeID: "node-1", Token: "first"}); err != nil {
		t.Fatalf("save the credential: %v", err)
	}
	if err := SaveCredential(path, Credential{NodeID: "node-2", Token: "second"}); err != nil {
		t.Fatalf("replace the credential: %v", err)
	}

	credential, err := LoadCredential(path)
	if err != nil {
		t.Fatalf("load the credential: %v", err)
	}
	if credential.Token != "second" {
		t.Fatalf("the token = %q, want the second one", credential.Token)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the directory holds %d files, want only the credential", len(entries))
	}
}

func TestTheCredentialFileOfAMissingAgentIsReported(t *testing.T) {
	_, err := LoadCredential(filepath.Join(t.TempDir(), "credential.json"))
	if err == nil {
		t.Fatal("the agent found a credential that does not exist")
	}
}
