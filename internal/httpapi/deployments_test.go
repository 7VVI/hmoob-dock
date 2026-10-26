package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// deployApplication creates an application and deploys a configuration of it.
//
// The deployment returns the task of the deployment, so the test may look at
// the state of the deployment.
func (s *authServer) deployApplication(t *testing.T, csrf, nodeID string) (string, string) {
	t.Helper()

	application := s.createApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)
	if appID == "" {
		t.Fatal("the application has no identifier")
	}
	preview := s.previewDeployment(t, csrf, appID, "")
	digest, _ := preview["digest"].(string)

	response := s.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key": "request-1",
		"preview_digest":  digest,
		"config": map[string]any{
			"yaml":      deploymentYAML,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("the deployment returned %d, want %d", response.StatusCode, http.StatusCreated)
	}
	payload := s.decode(response)
	taskID, _ := payload["id"].(string)
	if taskID == "" {
		t.Fatal("the deployment has no identifier")
	}
	return appID, taskID
}

// taskDigest reads the digest of a deployment task.
//
// The digest belongs to the configuration of the task, so the test reads it
// from the database: a report of another configuration is refused.
func (s *authServer) taskDigest(taskID string) string {
	s.t.Helper()

	var digest string
	if err := s.db.QueryRow(`SELECT digest FROM tasks WHERE id = ?`, taskID).Scan(&digest); err != nil {
		s.t.Fatalf("read the task: %v", err)
	}
	return digest
}

// decodeEvent reads the data of an event of the stream.
func decodeEvent(raw []byte) map[string]any {
	text := string(raw)
	start := strings.Index(text, "data: ")
	if start < 0 {
		return map[string]any{}
	}
	rest := text[start+len("data: "):]
	end := strings.Index(rest, "\n")
	if end < 0 {
		end = len(rest)
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(rest[:end]), &event); err != nil {
		return map[string]any{}
	}
	return event
}

func TestTheDeploymentsNeedTheSessionOfTheAdministrator(t *testing.T) {
	server := newAuthServer(t)

	response := server.get("/api/v1/deployments/task-1")
	defer response.Body.Close()

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestTheDeploymentIsReported(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)
	_, taskID := server.deployApplication(t, csrf, nodeID)

	response := server.get("/api/v1/deployments/" + taskID)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	payload := server.decode(response)
	if payload["id"] != taskID {
		t.Fatalf("the deployment holds the task %v, want %s", payload["id"], taskID)
	}
	if payload["state"] == "" {
		t.Fatalf("the deployment holds no state: %v", payload)
	}
	// The deployment holds no configuration.
	if _, ok := payload["config"]; ok {
		t.Fatalf("the deployment holds a configuration: %v", payload)
	}
}

func TestTheUnknownDeploymentIsReported(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	response := server.get("/api/v1/deployments/task-that-does-not-exist")
	defer response.Body.Close()

	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
}

func TestTheOutputOfTheDeploymentIsReported(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID, conn := server.connectAgentWith(t, csrf, nil)
	_, taskID := server.deployApplication(t, csrf, nodeID)

	// The agent sends the output of the task.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server.sendFrame(t, ctx, conn, protocol.TypeDeployOutput, protocol.DeployOutput{
		TaskID: taskID,
		Offset: 0,
		Data:   []byte("the output of the task"),
	})

	deadline := time.Now().Add(10 * time.Second)
	var payload map[string]any
	for time.Now().Before(deadline) {
		response := server.get("/api/v1/deployments/" + taskID + "/output")
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
		}
		payload = server.decode(response)
		fragments, _ := payload["fragments"].([]any)
		if len(fragments) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	fragments, _ := payload["fragments"].([]any)
	if len(fragments) != 1 {
		t.Fatalf("fragments = %v, want one", payload["fragments"])
	}
	fragment, _ := fragments[0].(map[string]any)
	if fragment["offset"].(float64) != 0 {
		t.Fatalf("the fragment holds the offset %v, want 0", fragment["offset"])
	}
}

func TestTheReleaseNeedsAConfirmationAndAReason(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)
	_, taskID := server.deployApplication(t, csrf, nodeID)

	response := server.post("/api/v1/deployments/"+taskID+"/resolve", map[string]any{
		"expected_seq":     0,
		"executor_stopped": false,
		"reason":           "the executor of the task was stopped by hand",
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	response = server.post("/api/v1/deployments/"+taskID+"/resolve", map[string]any{
		"expected_seq":     0,
		"executor_stopped": true,
		"reason":           "   ",
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
}

func TestTheReleaseWaitsForTheConfirmationOfTheNode(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)
	_, taskID := server.deployApplication(t, csrf, nodeID)

	response := server.post("/api/v1/deployments/"+taskID+"/resolve", map[string]any{
		"expected_seq":     0,
		"executor_stopped": true,
		"reason":           "the executor of the task was stopped by hand",
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusAccepted)
	}
	payload := server.decode(response)
	if payload["state"] == "failed" {
		t.Fatalf("the deployment ended before the node confirmed the release: %v", payload)
	}
}

func TestTheEventsNeedTheSessionOfTheAdministrator(t *testing.T) {
	server := newAuthServer(t)

	response := server.get("/api/v1/events")
	defer response.Body.Close()

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestTheEventsCarryTheTypeTheObjectAndTheSequenceNumber(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID, conn := server.connectAgentWith(t, csrf, nil)
	_, taskID := server.deployApplication(t, csrf, nodeID)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	response, err := server.client.Do(request)
	if err != nil {
		t.Fatalf("open the stream of the events: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content type = %q, want text/event-stream", contentType)
	}

	// The agent reports the outcome of the task, so the browser learns about
	// it.
	server.sendFrame(t, ctx, conn, protocol.TypeDeployReceipt, protocol.DeployReceipt{
		Receipt: model.Receipt{
			TaskID: taskID,
			Digest: server.taskDigest(taskID),
			Seq:    3,
			Result: model.Result{State: model.Succeeded, Code: "compose_ready"},
		},
	})

	reader := make(chan map[string]any, 1)
	go func() {
		buffer := make([]byte, 4096)
		read := 0
		for read < len(buffer) {
			count, err := response.Body.Read(buffer[read:])
			if err != nil {
				break
			}
			read += count
			if strings.Contains(string(buffer[:read]), "data: ") {
				break
			}
		}
		reader <- decodeEvent(buffer[:read])
	}()

	select {
	case event := <-reader:
		if event["type"] == "" {
			t.Fatalf("the event holds no type: %v", event)
		}
		if event["object_id"] != taskID {
			t.Fatalf("the event holds the object %v, want %s", event["object_id"], taskID)
		}
		if event["seq"].(float64) <= 0 {
			t.Fatalf("the event holds the sequence number %v, want a positive one", event["seq"])
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the stream of the events held no event")
	}
}
