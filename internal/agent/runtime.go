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
	"io"
	"log"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/config"
	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/yamlspec"
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
	// controlBuffer is the number of control frames that wait for the
	// write loop of the connection. A control frame answers the control端,
	// so it may not wait behind a flood of logs.
	controlBuffer = 16
	// logBuffer is the number of log frames that wait for the write loop
	// of the connection.
	logBuffer = 64
	// dialTimeout bounds the handshake of a connection.
	dialTimeout = 30 * time.Second
	// enrollmentTimeout bounds the registration of the node.
	enrollmentTimeout = 30 * time.Second
	// commandQueueSize is the number of commands that wait for their
	// execution.
	commandQueueSize = 16
	// ledgerFileName is the name of the ledger of the agent inside of the
	// work directory.
	ledgerFileName = "ledger.db"
)

// busyRetryInterval is the time the agent waits before it tries a command of
// the control端 again.
//
// A command of an application that holds another task is not a failure: the
// task may be released by hand later, so the command waits instead of ending
// the deployment with an error. The interval is a variable so that a test
// does not have to wait for it.
var busyRetryInterval = 5 * time.Second

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

	// The ledger of the agent holds the tasks of the node. It is a database
	// of its own, so the state of a node survives the state of the control端.
	ledger, err := OpenLedger(filepath.Join(cfg.WorkRoot, ledgerFileName))
	if err != nil {
		return err
	}
	defer func() {
		if err := ledger.Close(); err != nil {
			log.Printf("agent: close the ledger: %v", err)
		}
	}()

	// The executor of a deployment is chosen by the mode of its
	// application, so a client may not name the program of the node. The
	// execution runs under the context of the process of the agent, so a
	// connection that ends does not cancel a command that already runs.
	worker := NewWorker(ledger, &executors{
		compose: dockerops.NewCompose(cfg.WorkRoot, dockerops.ExecRunner{}, local),
		swarm:   dockerops.NewSwarm(cfg.WorkRoot, dockerops.ExecRunner{}, local),
	}).UseRootContext(ctx)

	handler := newDeployments(worker, ledger)
	handler.start(ctx)

	return run(ctx, cfg, local, handler)
}

// executors runs the deployments of the applications of a node.
//
// The executor is chosen by the mode of the application: a Compose
// application runs with Compose, a Swarm application as a stack. The mode
// belongs to the application of the control端, so neither a configuration nor
// an API may name the program of the node.
type executors struct {
	// compose runs the applications of the Compose mode.
	compose dockerops.Executor
	// swarm runs the applications of the Swarm mode.
	swarm dockerops.Executor
}

// Execute runs the command with the executor of the mode of the
// application.
func (e *executors) Execute(ctx context.Context, cmd model.Command, output io.Writer) (model.Result, error) {
	switch cmd.App.Mode {
	case yamlspec.ModeCompose:
		return e.compose.Execute(ctx, cmd, output)
	case yamlspec.ModeSwarm:
		return e.swarm.Execute(ctx, cmd, output)
	default:
		return model.Result{
			State:   model.Failed,
			Code:    dockerops.CodePreflightFailed,
			Message: fmt.Sprintf("应用模式 %q 不受支持", cmd.App.Mode),
		}, nil
	}
}

// sender hands the frames of the agent to the control端.
//
// The control frames answer the control端 and wait in the queue of the
// connection. The log frames carry the output of a task and are dropped
// when their queue is full.
type sender interface {
	// sendControl hands a control frame to the control端.
	sendControl(messageType string, payload any) error
	// sendLog hands a log frame to the control端.
	sendLog(messageType string, payload any) error
}

// deployments serves the deployment frames of the control端.
//
// Every command reaches the worker of the agent, so the intent of the
// execution is written into the ledger before the Docker command runs: a
// command that arrives twice runs the Docker command at most once.
//
// The questions about the ledger are answered from the ledger itself: they
// never run a command.
type deployments struct {
	// worker executes the commands of the control端.
	worker *Worker
	// ledger holds the tasks of the node.
	ledger *Ledger
	// commands holds the commands that wait for their execution.
	commands chan pendingCommand
}

// pendingCommand is a command of the control端 that waits for its
// execution.
type pendingCommand struct {
	// command is the command to execute.
	command model.Command
	// sender answers the frames of the command.
	sender sender
}

// newDeployments builds the handler of the deployment frames.
func newDeployments(worker *Worker, ledger *Ledger) *deployments {
	return &deployments{
		worker:   worker,
		ledger:   ledger,
		commands: make(chan pendingCommand, commandQueueSize),
	}
}

// start runs the worker of the commands.
//
// The worker runs under the context of the process of the agent, so a
// connection that ends does not cancel a command that already runs.
func (d *deployments) start(ctx context.Context) {
	go d.run(ctx)
}

// run executes the commands of the control端 one after the other.
//
// A node deploys one application at a time, and the ledger holds the
// application of a task anyway.
func (d *deployments) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pending, ok := <-d.commands:
			if !ok {
				return
			}
			d.execute(ctx, pending)
		}
	}
}

// handle serves a frame of the control端.
//
// The frames of the deployments are answered by the agent: a command runs
// the deployment, a question is answered from the ledger and a resolution
// ends a task by hand.
func (d *deployments) handle(ctx context.Context, envelope protocol.Envelope, out sender) {
	if d == nil {
		return
	}
	switch envelope.Type {
	case protocol.TypeDeployCommand:
		d.enqueue(ctx, envelope, out)
	case protocol.TypeDeployQuery:
		d.answer(ctx, envelope, out)
	case protocol.TypeDeployResolve:
		d.resolve(ctx, envelope, out)
	default:
		// The control端 sends nothing else to an agent.
	}
}

// enqueue hands a command of the control端 to the worker of the commands.
//
// The command waits in the queue: the read loop of the connection has to
// stay open, because the control端 asks the node about its tasks while a
// deployment runs.
func (d *deployments) enqueue(ctx context.Context, envelope protocol.Envelope, out sender) {
	message, err := protocol.DecodeDeployCommand(envelope.Payload)
	if err != nil {
		log.Printf("agent: the command of the control端 is not valid: %v", err)
		return
	}
	select {
	case d.commands <- pendingCommand{command: message, sender: out}:
	default:
		// The queue is full. The command is dropped and the control端
		// learns about it when it asks the node about the task again.
		log.Printf("agent: the queue of the commands is full, a command was dropped")
	}
}

// execute runs a command of the control端 and reports its outcome.
//
// A command that the ledger refuses because the application holds another
// task is not a failure: the application is still busy, so the command waits
// and is tried again. That is the case of a deployment that follows a
// manual release: the release is only complete when the node confirms it.
func (d *deployments) execute(ctx context.Context, pending pendingCommand) {
	frames := &outputFrames{sender: pending.sender, taskID: pending.command.Task.ID}
	for {
		receipt, err := d.worker.Handle(ctx, pending.command, frames)
		switch {
		case errors.Is(err, ErrAppBusy):
			select {
			case <-ctx.Done():
				return
			case <-time.After(busyRetryInterval):
			}
			continue
		case err != nil:
			// The ledger refused the command. The control端 learns the real
			// state of the task from the ledger itself.
			d.reportState(ctx, pending.command.Task.ID, pending.sender)
			return
		}
		frames.close()
		d.report(ctx, receipt, pending.sender)
		return
	}
}

// answer answers a question about the ledger of the node.
//
// The answer only reads the ledger: a question never runs a command.
func (d *deployments) answer(ctx context.Context, envelope protocol.Envelope, out sender) {
	query, err := protocol.DecodeDeployQuery(envelope.Payload)
	if err != nil {
		log.Printf("agent: the question of the control端 is not valid: %v", err)
		return
	}
	d.reportState(ctx, query.TaskID, out)
}

// resolve ends a task whose outcome could not be proven.
//
// The resolution is refused when the ledger holds another sequence number:
// the control端 learns the real state of the task from the answer of the
// node instead of forcing its own outcome onto the node.
func (d *deployments) resolve(ctx context.Context, envelope protocol.Envelope, out sender) {
	message, err := protocol.DecodeDeployResolve(envelope.Payload)
	if err != nil {
		log.Printf("agent: the resolution of the control端 is not valid: %v", err)
		return
	}
	receipt, err := d.ledger.Resolve(ctx, message.TaskID, message.ExpectedSeq, model.Result{
		State:   model.Failed,
		Message: message.Reason,
	})
	switch {
	case err == nil:
		d.report(ctx, receipt, out)
	case errors.Is(err, ErrSeqConflict), errors.Is(err, ErrNotResolvable):
		// The node knows another state of the task. The control端 learns it
		// from the answer of the node.
		d.reportState(ctx, message.TaskID, out)
	default:
		log.Printf("agent: resolve the task %s: %v", message.TaskID, err)
	}
}

// report sends the report of a task to the control端.
func (d *deployments) report(ctx context.Context, receipt model.Receipt, out sender) {
	if err := out.sendControl(protocol.TypeDeployReceipt, protocol.DeployReceipt{Receipt: receipt}); err != nil {
		log.Printf("agent: send the report of the task %s: %v", receipt.TaskID, err)
	}
}

// reportState sends the state of a task in the ledger to the control端.
//
// The ledger is the truth about the task, so the control端 learns it even
// when the command itself was refused.
func (d *deployments) reportState(ctx context.Context, taskID string, out sender) {
	answer := protocol.DeployQueryResult{TaskID: taskID}
	receipt, err := d.ledger.Lookup(ctx, taskID)
	switch {
	case err == nil:
		answer.Found = true
		answer.Receipt = receipt
	case errors.Is(err, ErrTaskNotFound):
		// The node does not know the task. The answer says so.
	default:
		log.Printf("agent: read the task %s: %v", taskID, err)
		return
	}
	if err := out.sendControl(protocol.TypeDeployQueryResult, answer); err != nil {
		log.Printf("agent: send the state of the task %s: %v", taskID, err)
	}
}

// outputFrames streams the output of a task to the control端.
//
// The fragments carry their offset, so the control端 stores every fragment
// once and a reader skips the fragments it has already seen.
//
// The queue of the logs is bounded: a fragment that does not fit is
// dropped. The ledger of the agent holds the cursor of the output and not
// the output itself, so a dropped fragment cannot be fetched again. The
// next fragment marks the output as truncated, so the administrator learns
// that the output is incomplete.
type outputFrames struct {
	// sender hands the fragments to the control端.
	sender sender
	// taskID is the task the output belongs to.
	taskID string

	// mu guards the offset and the flag of the truncation.
	mu sync.Mutex
	// offset is the position of the next fragment.
	offset int64
	// dropped reports that a fragment was lost.
	dropped bool
}

// Write forwards a piece of the output to the control端.
func (w *outputFrames) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	total := len(p)
	for len(p) > 0 {
		size := len(p)
		if size > protocol.MaxOutputPayloadSize {
			size = protocol.MaxOutputPayloadSize
		}
		fragment := protocol.DeployOutput{
			TaskID:    w.taskID,
			Offset:    w.offset,
			Data:      p[:size],
			Truncated: w.dropped,
		}
		w.offset += int64(size)
		if err := w.sender.sendLog(protocol.TypeDeployOutput, fragment); err != nil {
			// The fragment is lost. The next one tells the control端 that
			// the output is incomplete.
			w.dropped = true
		}
		p = p[size:]
	}
	return total, nil
}

// close tells the control端 that the output of the task is incomplete.
//
// A fragment that did not fit into the queue of the logs is lost, and the
// ledger of the agent holds the cursor of the output and not the output
// itself, so the fragment cannot be fetched again. The empty fragment marks
// the end of the output, so the administrator learns that the output of the
// task is incomplete instead of believing that the command was quiet.
func (w *outputFrames) close() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.dropped {
		return
	}
	fragment := protocol.DeployOutput{
		TaskID:    w.taskID,
		Offset:    w.offset,
		Truncated: true,
	}
	if err := w.sender.sendLog(protocol.TypeDeployOutput, fragment); err != nil {
		log.Printf("agent: mark the output of the task %s as truncated: %v", w.taskID, err)
	}
}

// run starts the agent with the given source of the Docker state.
func run(ctx context.Context, cfg config.Agent, engine source, handler *deployments) error {
	client, err := newHTTPClient(cfg)
	if err != nil {
		return err
	}

	credential, err := resolveCredential(ctx, cfg, client, engine)
	if err != nil {
		return err
	}

	return connect(ctx, cfg, client, credential, engine, handler)
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
func connect(ctx context.Context, cfg config.Agent, client *http.Client, credential Credential, engine source, handler *deployments) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		established, err := session(ctx, cfg, client, credential, engine, handler)
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
func session(ctx context.Context, cfg config.Agent, client *http.Client, credential Credential, engine source, handler *deployments) (bool, error) {
	conn, err := dialControl(ctx, cfg, client, credential)
	if err != nil {
		return false, err
	}
	defer conn.CloseNow()
	// The limit of a frame is the limit of the protocol. A command of the
	// control端 carries a document of an application, so it does not fit
	// into the small default limit of the library.
	conn.SetReadLimit(int64(protocol.MaxMessageSize))

	done := make(chan struct{})
	control := make(chan []byte, controlBuffer)
	logs := make(chan []byte, logBuffer)
	frames := &framer{}

	// The conversation starts with the hello of the agent. The hello is a
	// control frame, so it is the first frame the control端 reads.
	identity, err := engine.Identity(ctx)
	if err != nil {
		return true, err
	}
	hello, err := frames.envelope(protocol.TypeHello, protocol.Hello{Identity: identity})
	if err != nil {
		return true, err
	}
	if err := send(ctx, done, control, hello); err != nil {
		return true, err
	}

	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		writeFrames(ctx, conn, control, logs, done)
	}()
	workers.Add(2)
	go func() {
		defer workers.Done()
		reportHeartbeats(ctx, frames, logs, done)
	}()
	go func() {
		defer workers.Done()
		reportInventory(ctx, frames, logs, done, engine)
	}()

	out := &sessionSender{ctx: ctx, frames: frames, control: control, logs: logs, done: done}
	err = readControl(ctx, conn, func(ctx context.Context, envelope protocol.Envelope) {
		handler.handle(ctx, envelope, out)
	})

	// The workers stop before the connection is closed, so no frame is
	// written to a closed connection.
	close(done)
	workers.Wait()
	return true, err
}

// readControl reads the frames of the control端 until the connection ends.
func readControl(ctx context.Context, conn *websocket.Conn, handle func(context.Context, protocol.Envelope)) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("agent: the connection ended: %w", err)
		}
		envelope, err := protocol.Decode(data)
		if err != nil {
			return fmt.Errorf("agent: the frame of the control端 is not valid: %w", err)
		}
		if err := envelope.Validate(); err != nil {
			return fmt.Errorf("agent: the frame of the control端 is not accepted: %w", err)
		}
		handle(ctx, envelope)
	}
}

// sessionSender hands the frames of the agent to the control端.
//
// The frames of the control端 wait in the queue of the connection. The
// frames of the logs are dropped when their queue is full: the output of a
// task may be long, and the deployment of the node may not wait for it.
type sessionSender struct {
	// ctx is the context of the connection.
	ctx context.Context
	// frames builds the frames of the agent.
	frames *framer
	// control carries the frames that answer the control端.
	control chan []byte
	// logs carries the fragments of the output.
	logs chan []byte
	// done closes when the connection ends.
	done chan struct{}
}

// sendControl hands a control frame to the control端.
func (s *sessionSender) sendControl(messageType string, payload any) error {
	envelope, err := s.frames.envelope(messageType, payload)
	if err != nil {
		return err
	}
	return send(s.ctx, s.done, s.control, envelope)
}

// sendLog hands a log frame to the control端.
//
// The frame waits for room in the queue of the logs. A queue that is full
// drops the frame: the output of a task is not the report of its outcome,
// and the outcome of the task is never lost.
func (s *sessionSender) sendLog(messageType string, payload any) error {
	envelope, err := s.frames.envelope(messageType, payload)
	if err != nil {
		return err
	}
	frame, err := protocol.Encode(envelope)
	if err != nil {
		return err
	}
	select {
	case s.logs <- frame:
		return nil
	case <-s.done:
		return errors.New("agent: the connection ended")
	default:
		return errors.New("agent: the queue of the logs is full")
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

// writeFrames writes the frames of the agent to the control端.
//
// The connection has exactly one writer, so the frames never interleave. A
// control frame wins over a log frame: the answer of the control端 must not
// wait behind a flood of logs.
func writeFrames(ctx context.Context, conn *websocket.Conn, control, logs <-chan []byte, done <-chan struct{}) {
	for {
		frame, ok := nextFrame(ctx, control, logs, done)
		if !ok {
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			return
		}
	}
}

// nextFrame waits for the next frame of the agent.
//
// The control frames are served first, so a receipt of a task is never
// delayed by the fragments of a log.
func nextFrame(ctx context.Context, control, logs <-chan []byte, done <-chan struct{}) ([]byte, bool) {
	select {
	case frame := <-control:
		return frame, true
	default:
	}
	select {
	case <-done:
		return nil, false
	case <-ctx.Done():
		return nil, false
	case frame := <-control:
		return frame, true
	case frame := <-logs:
		return frame, true
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
	budget := protocol.MaxInventoryPayloadSize - len(template)

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
		if len(data) > protocol.MaxInventoryPayloadSize {
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
