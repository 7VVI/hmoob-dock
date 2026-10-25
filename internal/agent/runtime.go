// Package agent runs the agent of a node.
//
// The agent connects to the control端 over a verified TLS connection,
// registers itself with a one time enrollment token and reports the
// inventory of its node. The credential of the node lives in a file with
// restricted permissions; it never travels in an argument of a command or
// in the address of a request.
//
// The agent opens the connection to the control端, the control端 never
// connects to the agent. So the agent does not open a port of its own and
// the Docker socket of the node stays on the node.
package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/config"
	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/protocol"
)

// The intervals and limits of the agent.
const (
	// heartbeatInterval is the time the agent sends a heartbeat.
	heartbeatInterval = 10 * time.Second
	// inventoryInterval is the time the agent refreshes the inventory of
	// its node.
	inventoryInterval = 10 * time.Second
	// reconnectBaseDelay is the delay before the first connection attempt
	// after a failure.
	reconnectBaseDelay = time.Second
	// reconnectMaxDelay is the longest delay between two connection
	// attempts.
	reconnectMaxDelay = 30 * time.Second
	// outboundBuffer is the number of frames that wait for the write loop
	// of the connection.
	outboundBuffer = 64
	// dialTimeout bounds the handshake of a connection.
	dialTimeout = 30 * time.Second
	// enrollmentTimeout bounds the registration of the node.
	enrollmentTimeout = 30 * time.Second
)

// errRejected reports that the control端 refused the credential of the
// agent. Reconnecting does not help then, the node has to be registered
// again by the administrator.
var errRejected = errors.New("agent: the control端 refused the credential of the node")

// source reads the state of the local Docker engine.
type source interface {
	// Identity returns the identity of the engine.
	Identity(ctx context.Context) (protocol.NodeIdentity, error)
	// Inventory returns the containers of the engine.
	Inventory(ctx context.Context) (protocol.Inventory, error)
}

// Run starts the agent of a node and blocks until the context is done.
func Run(ctx context.Context, cfg config.Agent) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.WorkRoot, 0o700); err != nil {
		return fmt.Errorf("agent: create the work directory %s: %w", cfg.WorkRoot, err)
	}

	local, err := dockerops.NewLocal(cfg.DockerSocket)
	if err != nil {
		return err
	}
	defer func() {
		if err := local.Close(); err != nil {
			log.Printf("agent: close the Docker client: %v", err)
		}
	}()

	return run(ctx, cfg, local)
}

// run starts the agent with the given source of the Docker state.
func run(ctx context.Context, cfg config.Agent, engine source) error {
	client, err := newHTTPClient(cfg)
	if err != nil {
		return err
	}

	credential, err := resolveCredential(ctx, cfg, client, engine)
	if err != nil {
		return err
	}

	return connect(ctx, cfg, client, credential, engine)
}

// newHTTPClient builds the client of the agent.
//
// The certificate of the control端 is verified against the certificate
// authority of the configuration and its host name is verified as well. The
// agent never skips the verification of a certificate.
func newHTTPClient(cfg config.Agent) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		authority, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("agent: read the certificate authority %s: %w", cfg.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(authority) {
			return nil, fmt.Errorf("agent: the certificate authority %s holds no certificate", cfg.CAFile)
		}
		tlsConfig.RootCAs = pool
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
		Proxy:           http.ProxyFromEnvironment,
	}
	return &http.Client{Transport: transport, Timeout: enrollmentTimeout}, nil
}

// resolveCredential reads the credential of the agent.
//
// An agent without a credential registers itself with the enrollment token
// of the configuration and stores the credential it receives.
func resolveCredential(ctx context.Context, cfg config.Agent, client *http.Client, engine source) (Credential, error) {
	credential, err := LoadCredential(cfg.CredentialFile)
	if err == nil {
		return credential, nil
	}
	if !errors.Is(err, ErrNoCredential) {
		return Credential{}, err
	}

	token, err := readEnrollmentToken(cfg.EnrollmentFile)
	if err != nil {
		return Credential{}, err
	}
	identity, err := engine.Identity(ctx)
	if err != nil {
		return Credential{}, err
	}
	credential, err = enroll(ctx, cfg, client, token, identity)
	if err != nil {
		return Credential{}, err
	}
	if err := SaveCredential(cfg.CredentialFile, credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// readEnrollmentToken reads the one time enrollment token from the file of
// the configuration. The token is only read here, it never reaches the
// log.
func readEnrollmentToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("agent: the agent has no credential and no enrollment file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("agent: read the enrollment file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("agent: the enrollment file %s is empty", path)
	}
	return token, nil
}

// enroll registers the node at the control端 and returns the credential of
// the node.
func enroll(ctx context.Context, cfg config.Agent, client *http.Client, token string, identity protocol.NodeIdentity) (Credential, error) {
	body, err := json.Marshal(map[string]any{"token": token, "identity": identity})
	if err != nil {
		return Credential{}, fmt.Errorf("agent: encode the registration: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.ServerURL, "/")+"/api/v1/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return Credential{}, fmt.Errorf("agent: build the registration: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		return Credential{}, fmt.Errorf("agent: register the node: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		return Credential{}, fmt.Errorf("agent: the control端 refused the registration: %s", response.Status)
	}

	var answer struct {
		NodeID     string `json:"node_id"`
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return Credential{}, fmt.Errorf("agent: read the answer of the registration: %w", err)
	}
	if answer.NodeID == "" || answer.Credential == "" {
		return Credential{}, errors.New("agent: the control端 answered the registration without a credential")
	}
	return Credential{NodeID: answer.NodeID, Token: answer.Credential}, nil
}

// connect keeps the connection to the control端 alive.
//
// A connection that ends is opened again after a delay that grows with
// every attempt and that carries a jitter, so the agents of a fleet do not
// reconnect at the same moment.
func connect(ctx context.Context, cfg config.Agent, client *http.Client, credential Credential, engine source) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		established, err := session(ctx, cfg, client, credential, engine)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errRejected) {
			// The credential is not accepted any more. Reconnecting would
			// not help, the administrator has to register the node again.
			return err
		}
		if established {
			attempt = 0
		}
		delay := backoff(attempt)
		attempt++

		log.Printf("agent: the connection to the control端 ended: %v", err)
		log.Printf("agent: the next attempt is in %s", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// session opens one connection to the control端 and serves it until the
// connection ends. The second return value reports whether the connection
// was established.
func session(ctx context.Context, cfg config.Agent, client *http.Client, credential Credential, engine source) (bool, error) {
	conn, err := dialControl(ctx, cfg, client, credential)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()

	done := make(chan struct{})
	outbound := make(chan []byte, outboundBuffer)
	frames := &framer{}

	// The conversation starts with the hello of the agent. The frames wait
	// in one buffer, so the hello is the first frame the control端 reads.
	identity, err := engine.Identity(ctx)
	if err != nil {
		return true, err
	}
	hello, err := frames.envelope(protocol.TypeHello, protocol.Hello{Identity: identity})
	if err != nil {
		return true, err
	}
	if err := send(ctx, done, outbound, hello); err != nil {
		return true, err
	}

	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		writeFrames(ctx, conn, outbound, done)
	}()
	workers.Add(2)
	go func() {
		defer workers.Done()
		reportHeartbeats(ctx, frames, outbound, done)
	}()
	go func() {
		defer workers.Done()
		reportInventory(ctx, frames, outbound, done, engine)
	}()

	err = readControl(ctx, conn)

	// The workers stop before the connection is closed, so no frame is
	// written to a closed connection.
	close(done)
	workers.Wait()
	return true, err
}

// readControl reads the frames of the control端 until the connection ends.
func readControl(ctx context.Context, conn *websocket.Conn) error {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return fmt.Errorf("agent: the connection ended: %w", err)
		}
	}
}

// dialControl opens the connection to the control端.
//
// The credential of the node travels in a header, never in the address of
// the request, so it does not reach the log of a proxy.
func dialControl(ctx context.Context, cfg config.Agent, client *http.Client, credential Credential) (*websocket.Conn, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+credential.Token)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, response, err := websocket.Dial(dialCtx, connectURL(cfg.ServerURL), &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: header,
	})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return nil, errRejected
		}
		return nil, fmt.Errorf("agent: connect to the control端: %w", err)
	}
	return conn, nil
}

// connectURL turns the address of the control端 into the address of the
// connection of the agents.
func connectURL(serverURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	switch {
	case strings.HasPrefix(trimmed, "https://"):
		return "wss://" + strings.TrimPrefix(trimmed, "https://") + "/api/v1/agent/connect"
	case strings.HasPrefix(trimmed, "http://"):
		return "ws://" + strings.TrimPrefix(trimmed, "http://") + "/api/v1/agent/connect"
	default:
		return trimmed + "/api/v1/agent/connect"
	}
}

// framer builds the frames of the agent. Every frame carries a sequence
// number that grows by one per frame.
type framer struct {
	seq atomic.Uint64
}

// envelope builds the frame of a message.
func (f *framer) envelope(messageType string, payload any) (protocol.Envelope, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return protocol.Envelope{}, fmt.Errorf("agent: encode the %s: %w", messageType, err)
	}
	return protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    messageType,
		Seq:     f.seq.Add(1),
		Payload: encoded,
	}, nil
}

// send hands a frame to the write loop of the connection.
//
// The frame waits for room in the buffer. The wait ends when the
// connection ends, so a connection that does not read any more cannot hold
// the agent forever.
func send(ctx context.Context, done <-chan struct{}, outbound chan<- []byte, envelope protocol.Envelope) error {
	frame, err := protocol.Encode(envelope)
	if err != nil {
		return err
	}
	select {
	case outbound <- frame:
		return nil
	case <-done:
		return errors.New("agent: the connection ended")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// writeFrames writes the frames of the agent to the control端. The
// connection has exactly one writer, so the frames never interleave.
func writeFrames(ctx context.Context, conn *websocket.Conn, outbound <-chan []byte, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case frame := <-outbound:
			if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
				return
			}
		}
	}
}

// reportHeartbeats sends a heartbeat right away and then in every interval.
// The control端 judges a lost node by the time it received the last
// heartbeat.
func reportHeartbeats(ctx context.Context, frames *framer, outbound chan<- []byte, done <-chan struct{}) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		envelope, err := frames.envelope(protocol.TypeHeartbeat, protocol.Heartbeat{SentAt: time.Now().UTC()})
		if err != nil {
			log.Printf("agent: %v", err)
			return
		}
		if err := send(ctx, done, outbound, envelope); err != nil {
			return
		}

		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reportInventory reads the inventory of the node right away and then in
// every interval.
//
// A snapshot that does not fit into one message is sent in several
// batches, so the control端 never receives a truncated inventory that
// looks like a complete one.
func reportInventory(ctx context.Context, frames *framer, outbound chan<- []byte, done <-chan struct{}, engine source) {
	ticker := time.NewTicker(inventoryInterval)
	defer ticker.Stop()

	for {
		inventory, err := engine.Inventory(ctx)
		if err != nil {
			log.Printf("agent: read the inventory of the node: %v", err)
		} else {
			batches, err := splitInventory(inventory)
			if err != nil {
				log.Printf("agent: split the inventory of the node: %v", err)
			} else {
				for _, batch := range batches {
					envelope, err := frames.envelope(protocol.TypeInventory, batch)
					if err != nil {
						log.Printf("agent: %v", err)
						continue
					}
					if err := send(ctx, done, outbound, envelope); err != nil {
						return
					}
				}
			}
		}

		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// splitInventory splits the containers of an inventory into batches whose
// encoded size stays below the limit of a message.
//
// The batches carry the ID of the snapshot, the number of the batch, the
// number of the batches and the flag of the last batch, so the control端
// holds the snapshot as incomplete until every batch of it arrived.
func splitInventory(inventory protocol.Inventory) ([]protocol.InventoryBatch, error) {
	snapshotID, err := randomID()
	if err != nil {
		return nil, err
	}

	encoded := make([][]byte, 0, len(inventory.Containers))
	for _, item := range inventory.Containers {
		data, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("agent: encode the container %s: %w", item.ID, err)
		}
		encoded = append(encoded, data)
	}

	// Every batch carries the same fields around its containers, so the
	// size of that part is known before the split.
	template, err := json.Marshal(protocol.InventoryBatch{
		SnapshotID:  snapshotID,
		BatchIndex:  0,
		BatchCount:  1,
		Complete:    true,
		Identity:    inventory.Identity,
		CollectedAt: inventory.CollectedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: encode the inventory batch: %w", err)
	}
	budget := protocol.MaxPayloadSize - len(template)

	var batches []protocol.InventoryBatch
	current := make([]protocol.Container, 0, 16)
	size := 0
	flush := func() {
		batches = append(batches, protocol.InventoryBatch{
			SnapshotID:  snapshotID,
			BatchIndex:  len(batches),
			Identity:    inventory.Identity,
			Containers:  current,
			CollectedAt: inventory.CollectedAt,
		})
		current = make([]protocol.Container, 0, 16)
		size = 0
	}
	for index, item := range inventory.Containers {
		if len(current) > 0 && size+len(encoded[index])+1 > budget {
			flush()
		}
		current = append(current, item)
		size += len(encoded[index]) + 1
	}
	if len(current) > 0 || len(batches) == 0 {
		flush()
	}

	for index := range batches {
		batches[index].BatchCount = len(batches)
		batches[index].Complete = index == len(batches)-1
	}
	for index, batch := range batches {
		data, err := json.Marshal(batch)
		if err != nil {
			return nil, fmt.Errorf("agent: encode the inventory batch: %w", err)
		}
		if len(data) > protocol.MaxPayloadSize {
			return nil, fmt.Errorf(
				"agent: the batch %d of the inventory is too large: %d bytes", index, len(data))
		}
	}
	return batches, nil
}

// backoff returns the delay before the next connection attempt.
//
// The delay grows exponentially and is capped at the maximum delay. Half of
// the delay is random, so the agents of a fleet do not reconnect at the
// same moment.
func backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 6 {
		attempt = 6
	}
	delay := reconnectBaseDelay << uint(attempt)
	if delay > reconnectMaxDelay {
		delay = reconnectMaxDelay
	}
	half := delay / 2
	return half + time.Duration(mathrand.Int64N(int64(half)+1))
}

// randomID returns a random identifier of a snapshot.
func randomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("agent: generate an identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
