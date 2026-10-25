package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"hmoob-dock/internal/protocol"
)

// nodeIdentity is the identity a node reports when it enrolls.
var nodeIdentity = protocol.NodeIdentity{
	EngineID: "engine-a",
	Hostname: "worker-1",
	Role:     protocol.RoleStandalone,
}

// createAgentToken hands out an enrollment token as the administrator.
func (s *authServer) createAgentToken(t *testing.T, csrf string) string {
	t.Helper()

	response := s.post("/api/v1/agent-tokens", nil, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("the token request returned %d, want %d", response.StatusCode, http.StatusCreated)
	}
	payload := s.decode(response)
	token, _ := payload["token"].(string)
	if token == "" {
		t.Fatal("the answer of the token request holds no token")
	}
	return token
}

// enroll registers a node with the given enrollment token.
func (s *authServer) enroll(t *testing.T, token string, identity protocol.NodeIdentity) map[string]any {
	t.Helper()

	response := s.do(http.MethodPost, "/api/v1/agent/enroll", map[string]any{
		"token":    token,
		"identity": identity,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("the enrollment returned %d, want %d", response.StatusCode, http.StatusCreated)
	}
	return s.decode(response)
}

// nodeList reads the nodes of the control端.
func (s *authServer) nodeList(t *testing.T) []map[string]any {
	t.Helper()

	response := s.get("/api/v1/nodes")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the node list returned %d, want %d", response.StatusCode, http.StatusOK)
	}
	payload := s.decode(response)
	raw, _ := payload["nodes"].([]any)
	nodes := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		node, _ := entry.(map[string]any)
		nodes = append(nodes, node)
	}
	return nodes
}

func TestTheAgentTokenNeedsTheSessionOfTheAdministrator(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.post("/api/v1/agent-tokens", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	payload := server.decode(response)
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("the error payload = %v, want a code and a message", payload)
	}
}

func TestTheAgentTokenNeedsTheCSRFToken(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	response := server.post("/api/v1/agent-tokens", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func TestTheEnrollmentTokenRegistersANode(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	token := server.createAgentToken(t, csrf)

	// The machine endpoint does not use the session of the administrator:
	// it verifies the enrollment token instead.
	payload := server.enroll(t, token, nodeIdentity)
	nodeID, _ := payload["node_id"].(string)
	credential, _ := payload["credential"].(string)
	if nodeID == "" || credential == "" {
		t.Fatalf("the enrollment answered %v, want a node ID and a credential", payload)
	}

	nodes := server.nodeList(t)
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if nodes[0]["id"] != nodeID {
		t.Fatalf("the node ID = %v, want %q", nodes[0]["id"], nodeID)
	}
	if nodes[0]["hostname"] != "worker-1" {
		t.Fatalf("the hostname = %v, want worker-1", nodes[0]["hostname"])
	}
	if nodes[0]["connected"] != false {
		t.Fatalf("the node is connected = %v, want false", nodes[0]["connected"])
	}
	if nodes[0]["container_count"] != float64(0) {
		t.Fatalf("the container count = %v, want 0", nodes[0]["container_count"])
	}
	if nodes[0]["last_seen"] != nil {
		t.Fatalf("the node was seen = %v, want nil", nodes[0]["last_seen"])
	}
}

func TestTheEnrollmentTokenWorksOnlyOnce(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	token := server.createAgentToken(t, csrf)
	server.enroll(t, token, nodeIdentity)

	response := server.do(http.MethodPost, "/api/v1/agent/enroll", map[string]any{
		"token":    token,
		"identity": protocol.NodeIdentity{EngineID: "engine-b"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	payload := server.decode(response)
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("the error payload = %v, want a code and a message", payload)
	}
}

func TestTheEnrollmentRefusesAKnownEngine(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	first := server.createAgentToken(t, csrf)
	server.enroll(t, first, nodeIdentity)

	second := server.createAgentToken(t, csrf)
	response := server.do(http.MethodPost, "/api/v1/agent/enroll", map[string]any{
		"token":    second,
		"identity": nodeIdentity,
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
}

func TestTheNodeListNeedsTheSessionOfTheAdministrator(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.get("/api/v1/nodes")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestTheConnectEndpointRefusesAnUnknownCredential(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	request, err := http.NewRequest(http.MethodGet, server.server.URL+"/api/v1/agent/connect", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer not-a-credential")
	response, err := server.client.Do(request)
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestRevokingANodeClosesItsConnection(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	token := server.createAgentToken(t, csrf)
	payload := server.enroll(t, token, nodeIdentity)
	nodeID, _ := payload["node_id"].(string)
	credential, _ := payload["credential"].(string)

	// The agent opens its connection.
	header := http.Header{}
	header.Set("Authorization", "Bearer "+credential)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://"+server.host()+"/api/v1/agent/connect",
		&websocket.DialOptions{HTTPClient: server.client, HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial the control端: %v", err)
	}
	defer conn.CloseNow()

	hello, err := json.Marshal(protocol.Hello{Identity: nodeIdentity})
	if err != nil {
		t.Fatalf("encode the hello: %v", err)
	}
	frame, err := protocol.Encode(protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    protocol.TypeHello,
		Seq:     1,
		Payload: hello,
	})
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write the hello: %v", err)
	}

	// The administrator revokes the node.
	response := server.post("/api/v1/nodes/"+nodeID+"/revoke", nil, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("the revocation returned %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	// The connection of the node ends.
	readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readCancel()
	for {
		if _, _, err := conn.Read(readCtx); err != nil {
			break
		}
	}

	// The credential cannot be used any more.
	revoked, err := server.client.Do(request(http.MethodGet, server.server.URL+"/api/v1/agent/connect", header))
	if err != nil {
		t.Fatalf("send the request: %v", err)
	}
	defer revoked.Body.Close()
	if revoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", revoked.StatusCode, http.StatusUnauthorized)
	}
}

func TestRevokingAnUnknownNodeFails(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := server.post("/api/v1/nodes/node-that-does-not-exist/revoke", nil,
		withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}

func TestTheMachineEndpointsDoNotUseTheSessionOfTheAdministrator(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	// The administrator has a session, but the enrollment only needs the
	// enrollment token.
	token := server.createAgentToken(t, csrf)
	payload := server.enroll(t, token, nodeIdentity)
	if payload["node_id"] == "" {
		t.Fatalf("the enrollment answered %v, want a node", payload)
	}

	// The connect endpoint answers a request without a credential with the
	// common error format.
	response := server.get("/api/v1/agent/connect")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	errorPayload := server.decode(response)
	if errorPayload["code"] == "" || errorPayload["message"] == "" {
		t.Fatalf("the error payload = %v, want a code and a message", errorPayload)
	}
	if strings.Contains(server.server.URL, errorPayload["code"].(string)) {
		t.Fatal("the error message holds the address of the control端")
	}
}

// request builds a request with the given headers.
func request(method, url string, header http.Header) *http.Request {
	built, err := http.NewRequest(method, url, nil)
	if err != nil {
		panic(err)
	}
	for name, values := range header {
		for _, value := range values {
			built.Header.Add(name, value)
		}
	}
	return built
}
