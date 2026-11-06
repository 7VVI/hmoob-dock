package httpapi_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"hmoob-dock/internal/protocol"
)

// containerOpPath is the path of the operations on one container.
func containerOpPath(nodeID, containerID string) string {
	return "/api/v1/nodes/" + nodeID + "/containers/" + containerID + "/op"
}

// validContainerID is a container ID the shape checks accept.
const validContainerID = "abcdef1234567890"

// withRawBody replaces the body of a request, so a test can send a body
// that is not valid JSON.
func withRawBody(body string) requestOption {
	return func(r *http.Request) {
		r.Body = io.NopCloser(bytes.NewReader([]byte(body)))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Type", "application/json")
	}
}

// TestTheContainerOperationNeedsTheSessionOfTheAdministrator proves that a
// request of another origin is refused, and a request of the origin of the
// control端 without a session is refused as well. The checks of the origin
// run before the check of the session, so a foreign page learns nothing
// about the session of the administrator.
func TestTheContainerOperationNeedsTheSessionOfTheAdministrator(t *testing.T) {
	server := newAuthServer(t)

	request, err := http.NewRequest(http.MethodPost, server.server.URL+containerOpPath("node-1", validContainerID),
		bytes.NewReader([]byte(`{"action":"stop"}`)))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Origin", "https://another.example")
	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("the operation of another origin returned %d, want %d", recorder.Code, http.StatusForbidden)
	}

	response := server.post(containerOpPath("node-1", validContainerID), nil, withRawBody(`{"action":"stop"}`))
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the operation without a session returned %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

// TestTheContainerOperationRefusesABrokenRequest proves that an unknown
// action and an invalid container identifier are refused before the node
// is asked.
func TestTheContainerOperationRefusesABrokenRequest(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	cases := []struct {
		name     string
		path     string
		rawBody  string
		wantCode int
	}{
		{"unknown action", containerOpPath("node-1", validContainerID), `{"action":"exec"}`, http.StatusBadRequest},
		{"empty action", containerOpPath("node-1", validContainerID), `{"action":""}`, http.StatusBadRequest},
		{"invalid container", containerOpPath("node-1", "short"), `{"action":"stop"}`, http.StatusBadRequest},
		{"non hexadecimal container", containerOpPath("node-1", "zzzzzz12345678"), `{"action":"stop"}`, http.StatusBadRequest},
		{"broken body", containerOpPath("node-1", validContainerID), `{`, http.StatusBadRequest},
	}
	for _, test := range cases {
		response := server.post(test.path, nil, withHeader("X-CSRF-Token", csrf), withRawBody(test.rawBody))
		response.Body.Close()
		if response.StatusCode != test.wantCode {
			t.Fatalf("%s: the operation returned %d, want %d", test.name, response.StatusCode, test.wantCode)
		}
	}
}

// TestTheContainerOperationRefusesAnUnknownNode proves that an operation on
// a node that was never enrolled is a not found.
func TestTheContainerOperationRefusesAnUnknownNode(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := server.post(containerOpPath("node-that-does-not-exist", validContainerID), map[string]any{"action": "stop"},
		withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("the operation on an unknown node returned %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}

// TestTheContainerOperationRefusesADisconnectedNode proves that an
// operation needs the live connection of its node: a node that is away
// cannot perform an operation, and the control端 says so instead of
// pretending the request was sent.
func TestTheContainerOperationRefusesADisconnectedNode(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	token := server.createAgentToken(t, csrf)
	payload := server.enroll(t, token, nodeIdentity)
	nodeID, _ := payload["node_id"].(string)

	response := server.post(containerOpPath(nodeID, validContainerID), map[string]any{"action": "stop"},
		withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("the operation on an away node returned %d, want %d", response.StatusCode, http.StatusConflict)
	}
	answer := server.decode(response)
	if answer["code"] != "target_unavailable" {
		t.Fatalf("the refusal names %v, want target_unavailable", answer["code"])
	}
}

// TestTheNodeDetailReportsTheRecord proves that the detail of a node holds
// the environment the enrollment stored.
func TestTheNodeDetailReportsTheRecord(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	token := server.createAgentToken(t, csrf)
	payload := server.enroll(t, token, nodeIdentity)
	nodeID, _ := payload["node_id"].(string)

	response := server.get("/api/v1/nodes/" + nodeID)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the node detail returned %d, want %d", response.StatusCode, http.StatusOK)
	}
	decoded := server.decode(response)
	node, _ := decoded["node"].(map[string]any)
	if node["id"] != nodeID || node["role"] != protocol.RoleStandalone {
		t.Fatalf("the node detail is wrong: %v", node)
	}
	if _, ok := node["docker_version"]; !ok {
		t.Fatal("the node detail holds no docker_version")
	}
}

// TestTheNodeDetailRefusesAnUnknownNode proves that the detail of a node
// that was never enrolled is a not found.
func TestTheNodeDetailRefusesAnUnknownNode(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	response := server.get("/api/v1/nodes/node-that-does-not-exist")
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("the detail of an unknown node returned %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}
