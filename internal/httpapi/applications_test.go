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

// deploymentYAML is the configuration of the application of the tests.
const deploymentYAML = "services:\n  api:\n    image: nginx:alpine\n"

// connectAgent registers a node and connects its agent with an inventory.
//
// The inventory comes from a real connection of an agent, so the API reads
// the state of a target the same way it does in a deployment.
func (s *authServer) connectAgent(t *testing.T, csrf string, containers []protocol.Container) string {
	t.Helper()

	nodeID, _ := s.connectAgentWith(t, csrf, containers)
	return nodeID
}

// connectAgentWith registers a node and connects its agent with an
// inventory. It returns the node and the connection of its agent.
func (s *authServer) connectAgentWith(t *testing.T, csrf string, containers []protocol.Container) (string, *websocket.Conn) {
	t.Helper()

	token := s.createAgentToken(t, csrf)
	payload := s.enroll(t, token, nodeIdentity)
	nodeID, _ := payload["node_id"].(string)
	credential, _ := payload["credential"].(string)
	if nodeID == "" || credential == "" {
		t.Fatal("the enrollment answered without a node")
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+credential)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://"+s.host()+"/api/v1/agent/connect",
		&websocket.DialOptions{HTTPClient: s.client, HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial the control端: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })

	s.sendFrame(t, ctx, conn, protocol.TypeHello, protocol.Hello{Identity: nodeIdentity})
	s.sendFrame(t, ctx, conn, protocol.TypeInventory, protocol.InventoryBatch{
		SnapshotID:  "snapshot-1",
		BatchIndex:  0,
		BatchCount:  1,
		Complete:    true,
		Identity:    nodeIdentity,
		Containers:  containers,
		CollectedAt: time.Now(),
	})
	s.waitForInventory(t, nodeID, len(containers))
	return nodeID, conn
}

// sendFrame writes one frame of an agent to the control端.
func (s *authServer) sendFrame(t *testing.T, ctx context.Context, conn *websocket.Conn, messageType string, payload any) {
	t.Helper()

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	frame, err := protocol.Encode(protocol.Envelope{
		Version: protocol.WireVersion,
		Type:    messageType,
		Seq:     1,
		Payload: encoded,
	})
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatalf("write the frame: %v", err)
	}
}

// waitForInventory waits until the control端 holds the inventory of the
// node.
func (s *authServer) waitForInventory(t *testing.T, nodeID string, containers int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response := s.get("/api/v1/nodes")
		payload := s.decode(response)
		nodes, _ := payload["nodes"].([]any)
		for _, entry := range nodes {
			node, _ := entry.(map[string]any)
			if node["id"] != nodeID {
				continue
			}
			if count, _ := node["container_count"].(float64); int(count) == containers {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the control端 did not hold the inventory of the node %s", nodeID)
}

// createApplication creates an application of the tests.
func (s *authServer) createApplication(t *testing.T, csrf, nodeID string) map[string]any {
	t.Helper()
	return s.createApplicationWith(t, csrf, nodeID, false)
}

// createAdoptedApplication creates an application whose namespace holds
// resources that the platform does not manage.
//
// The administrator confirmed the takeover of those resources.
func (s *authServer) createAdoptedApplication(t *testing.T, csrf, nodeID string) map[string]any {
	t.Helper()
	return s.createApplicationWith(t, csrf, nodeID, true)
}

// createApplicationWith creates an application of the tests.
func (s *authServer) createApplicationWith(t *testing.T, csrf, nodeID string, adopt bool) map[string]any {
	t.Helper()

	response := s.post("/api/v1/applications", map[string]any{
		"name":      "demo",
		"mode":      "compose",
		"target_id": nodeID,
		"namespace": "demo",
		"adopt":     adopt,
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("the creation returned %d, want %d", response.StatusCode, http.StatusCreated)
	}
	return s.decode(response)
}

// finishDeployment ends a deployment task.
//
// The state machine of a task belongs to a later task of the plan, so the
// test ends a task with a direct write: an application holds at most one
// task that is not finished.
func (s *authServer) finishDeployment(t *testing.T, taskID string) {
	t.Helper()

	if _, err := s.db.Exec(
		`UPDATE tasks SET state = 'succeeded', finished_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), taskID,
	); err != nil {
		t.Fatalf("finish the deployment: %v", err)
	}
}

// previewDeployment asks the control端 for the impact of a configuration.
func (s *authServer) previewDeployment(t *testing.T, csrf, appID, baseVersionID string) map[string]any {
	t.Helper()

	return s.previewDocument(t, csrf, appID, baseVersionID, deploymentYAML)
}

// previewDocument asks the control端 for the impact of a document.
func (s *authServer) previewDocument(t *testing.T, csrf, appID, baseVersionID, document string) map[string]any {
	t.Helper()

	response := s.post("/api/v1/applications/"+appID+"/preview", map[string]any{
		"base_version_id": baseVersionID,
		"config": map[string]any{
			"yaml":      document,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the preview returned %d, want %d", response.StatusCode, http.StatusOK)
	}
	return s.decode(response)
}

func TestTheApplicationsNeedTheSessionOfTheAdministrator(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()

	response := server.get("/api/v1/applications")
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	payload := server.decode(response)
	if payload["code"] == "" || payload["message"] == "" {
		t.Fatalf("the error payload = %v, want a code and a message", payload)
	}
}

func TestTheModifyingRequestsOfTheApplicationsNeedTheCSRFToken(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	server.login()

	response := server.post("/api/v1/applications", map[string]any{
		"name":      "demo",
		"mode":      "compose",
		"target_id": "a-node",
		"namespace": "demo",
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func TestTheCreationRefusesATargetThatTheControlDoesNotKnow(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()

	response := server.post("/api/v1/applications", map[string]any{
		"name":      "demo",
		"mode":      "compose",
		"target_id": "a-node-that-does-not-exist",
		"namespace": "demo",
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	payload := server.decode(response)
	if payload["code"] != "target_not_found" {
		t.Fatalf("error code = %v, want target_not_found", payload["code"])
	}
}

func TestTheDeploymentFlowFromTheApplicationToTheVersion(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)

	application := server.createApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)
	if appID == "" {
		t.Fatalf("the application = %v, want an identifier", application)
	}
	if application["namespace"] != "demo" {
		t.Fatalf("the namespace = %v, want demo", application["namespace"])
	}

	preview := server.previewDeployment(t, csrf, appID, "")
	digest, _ := preview["digest"].(string)
	revision, _ := preview["inventory_revision"].(string)
	if digest == "" || revision == "" {
		t.Fatalf("the preview = %v, want a digest and a revision", preview)
	}

	response := server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key":    "request-1",
		"base_version_id":    "",
		"preview_digest":     digest,
		"inventory_revision": revision,
		"config": map[string]any{
			"yaml":      deploymentYAML,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("the deployment returned %d, want %d", response.StatusCode, http.StatusCreated)
	}
	task := server.decode(response)
	taskID, _ := task["id"].(string)
	versionID, _ := task["version_id"].(string)
	if taskID == "" || versionID == "" {
		t.Fatalf("the task = %v, want a task and a version", task)
	}
	if task["state"] != "queued" {
		t.Fatalf("the state = %v, want queued", task["state"])
	}

	// The list of the versions holds no configuration.
	response = server.get("/api/v1/applications/" + appID + "/versions")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the version list returned %d, want %d", response.StatusCode, http.StatusOK)
	}
	versions := server.decode(response)
	list, _ := versions["versions"].([]any)
	if len(list) != 1 {
		t.Fatalf("versions = %v, want one version", versions)
	}
	entry, _ := list[0].(map[string]any)
	if entry["id"] != versionID {
		t.Fatalf("the version = %v, want %q", entry["id"], versionID)
	}
	if _, ok := entry["config"]; ok {
		t.Fatal("the list of the versions holds a configuration")
	}

	// The single version holds the configuration.
	response = server.get("/api/v1/applications/" + appID + "/versions/" + versionID)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the version returned %d, want %d", response.StatusCode, http.StatusOK)
	}
	version := server.decode(response)
	config, _ := version["config"].(map[string]any)
	if yaml, _ := config["yaml"].(string); !strings.Contains(yaml, "nginx:alpine") {
		t.Fatalf("the version = %v, want the submitted document", version)
	}
}

func TestTheRepeatedDeploymentAnswersTheSameTask(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)

	application := server.createApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)
	preview := server.previewDeployment(t, csrf, appID, "")
	digest, _ := preview["digest"].(string)
	revision, _ := preview["inventory_revision"].(string)

	request := map[string]any{
		"idempotency_key":    "request-1",
		"preview_digest":     digest,
		"inventory_revision": revision,
		"config": map[string]any{
			"yaml":      deploymentYAML,
			"variables": map[string]string{},
		},
	}
	first := server.post("/api/v1/applications/"+appID+"/deployments", request,
		withHeader("X-CSRF-Token", csrf))
	firstBody := server.decode(first)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("the deployment returned %d, want %d", first.StatusCode, http.StatusCreated)
	}

	second := server.post("/api/v1/applications/"+appID+"/deployments", request,
		withHeader("X-CSRF-Token", csrf))
	secondBody := server.decode(second)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("the repeated deployment returned %d, want %d", second.StatusCode, http.StatusCreated)
	}
	if firstBody["id"] != secondBody["id"] {
		t.Fatalf("the tasks are %v and %v, want the same one", firstBody["id"], secondBody["id"])
	}
}

func TestTheBusyApplicationAnswersAConflict(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)

	application := server.createApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)
	preview := server.previewDeployment(t, csrf, appID, "")
	digest, _ := preview["digest"].(string)
	revision, _ := preview["inventory_revision"].(string)

	config := map[string]any{
		"yaml":      deploymentYAML,
		"variables": map[string]string{},
	}
	response := server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key":    "request-1",
		"preview_digest":     digest,
		"inventory_revision": revision,
		"config":             config,
	}, withHeader("X-CSRF-Token", csrf))
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("the deployment returned %d, want %d", response.StatusCode, http.StatusCreated)
	}

	// Another key of the busy application is refused.
	response = server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key": "request-2",
		"preview_digest":  digest,
		"config":          config,
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
	payload := server.decode(response)
	if payload["code"] != "app_busy" {
		t.Fatalf("error code = %v, want app_busy", payload["code"])
	}
}

func TestAnOutdatedPageAnswersAConflict(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)

	application := server.createApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)
	preview := server.previewDeployment(t, csrf, appID, "")
	digest, _ := preview["digest"].(string)
	revision, _ := preview["inventory_revision"].(string)

	response := server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key":    "request-1",
		"preview_digest":     digest,
		"inventory_revision": revision,
		"config": map[string]any{
			"yaml":      deploymentYAML,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	task := server.decode(response)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("the deployment returned %d, want %d", response.StatusCode, http.StatusCreated)
	}
	taskID, _ := task["id"].(string)
	versionID, _ := task["version_id"].(string)
	server.finishDeployment(t, taskID)

	// The page was opened before the deployment, so it names no version.
	// The administrator looks at the impact of the new configuration on the
	// version the application has now.
	changed := "services:\n  api:\n    image: nginx:1.27\n"
	current := server.previewDocument(t, csrf, appID, versionID, changed)

	response = server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key":    "request-2",
		"base_version_id":    "",
		"preview_digest":     current["digest"],
		"inventory_revision": current["inventory_revision"],
		"config": map[string]any{
			"yaml":      changed,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
	payload := server.decode(response)
	if payload["code"] != "conflict" {
		t.Fatalf("error code = %v, want conflict", payload["code"])
	}
}

func TestADeploymentWithoutAPreviewAnswersAConflict(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)

	application := server.createApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)

	// Nobody looked at the impact of the configuration, so the deployment
	// is refused.
	response := server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key": "request-1",
		"config": map[string]any{
			"yaml":      deploymentYAML,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
	payload := server.decode(response)
	if payload["code"] != "conflict" {
		t.Fatalf("error code = %v, want conflict", payload["code"])
	}
}

func TestTheRemovalOfAServiceNeedsAConfirmation(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, []protocol.Container{{
		ID:    "container-db",
		Name:  "demo-db-1",
		Image: "postgres:16",
		Labels: map[string]string{
			"com.docker.compose.project": "demo",
			"com.docker.compose.service": "db",
		},
	}})

	application := server.createAdoptedApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)
	preview := server.previewDeployment(t, csrf, appID, "")
	removed, _ := preview["removed_services"].([]any)
	if len(removed) != 1 || removed[0] != "db" {
		t.Fatalf("removed services = %v, want the service db", preview["removed_services"])
	}
	digest, _ := preview["digest"].(string)
	revision, _ := preview["inventory_revision"].(string)

	response := server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key":    "request-1",
		"preview_digest":     digest,
		"inventory_revision": revision,
		"config": map[string]any{
			"yaml":      deploymentYAML,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnprocessableEntity)
	}
	payload := server.decode(response)
	if payload["code"] != "confirmation_required" {
		t.Fatalf("error code = %v, want confirmation_required", payload["code"])
	}

	// The confirmation names the configuration, the services and the state
	// of the target, so the deployment goes through.
	response = server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key":    "request-2",
		"preview_digest":     digest,
		"inventory_revision": revision,
		"confirmed_removal": map[string]any{
			"digest":             digest,
			"services":           removed,
			"inventory_revision": revision,
		},
		"config": map[string]any{
			"yaml":      deploymentYAML,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusCreated)
	}
}

func TestTheConfirmationOfAnotherConfigurationAnswersAConflict(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, []protocol.Container{{
		ID:    "container-db",
		Name:  "demo-db-1",
		Image: "postgres:16",
		Labels: map[string]string{
			"com.docker.compose.project": "demo",
			"com.docker.compose.service": "db",
		},
	}})

	application := server.createAdoptedApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)

	// The administrator looks at the impact of the first configuration.
	first := server.previewDeployment(t, csrf, appID, "")
	removed, _ := first["removed_services"].([]any)
	if len(removed) != 1 {
		t.Fatalf("removed services = %v, want the service db", first["removed_services"])
	}

	// The configuration changes, but the removed service is the same.
	changed := "services:\n  api:\n    image: nginx:1.27\n"
	second := server.previewDocument(t, csrf, appID, "", changed)

	// The confirmation of the first preview does not fit the second
	// configuration.
	response := server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key":    "request-1",
		"preview_digest":     second["digest"],
		"inventory_revision": second["inventory_revision"],
		"confirmed_removal": map[string]any{
			"digest":             first["digest"],
			"services":           removed,
			"inventory_revision": first["inventory_revision"],
		},
		"config": map[string]any{
			"yaml":      changed,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusConflict)
	}
	payload := server.decode(response)
	if payload["code"] != "conflict" {
		t.Fatalf("error code = %v, want conflict", payload["code"])
	}
}

func TestTheRiskyConfigurationNeedsAConfirmation(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)

	application := server.createApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)

	risky := "services:\n  api:\n    image: nginx:alpine\n    privileged: true\n"
	response := server.post("/api/v1/applications/"+appID+"/preview", map[string]any{
		"config": map[string]any{
			"yaml":      risky,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the preview returned %d, want %d", response.StatusCode, http.StatusOK)
	}
	preview := server.decode(response)
	if preview["requires_confirmation"] != true {
		t.Fatalf("the preview = %v, want a confirmation", preview)
	}

	response = server.post("/api/v1/applications/"+appID+"/deployments", map[string]any{
		"idempotency_key": "request-1",
		"preview_digest":  preview["digest"],
		"config": map[string]any{
			"yaml":      risky,
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnprocessableEntity)
	}
}

func TestTheInvalidConfigurationIsReportedWithItsFields(t *testing.T) {
	server := newAuthServer(t)
	server.initialize()
	csrf := server.login()
	nodeID := server.connectAgent(t, csrf, nil)

	application := server.createApplication(t, csrf, nodeID)
	appID, _ := application["id"].(string)

	response := server.post("/api/v1/applications/"+appID+"/preview", map[string]any{
		"config": map[string]any{
			"yaml":      "services:\n  api:\n    build: .\n",
			"variables": map[string]string{},
		},
	}, withHeader("X-CSRF-Token", csrf))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
	payload := server.decode(response)
	if payload["code"] != "unsupported_field" {
		t.Fatalf("error code = %v, want unsupported_field", payload["code"])
	}
	fields, _ := payload["fields"].([]any)
	if len(fields) == 0 {
		t.Fatalf("the error payload = %v, want the fields of the configuration", payload)
	}
	field, _ := fields[0].(map[string]any)
	if field["path"] != "services.api.build" {
		t.Fatalf("the field = %v, want the path of the build field", field)
	}
}
