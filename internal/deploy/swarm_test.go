package deploy_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// swarmHarness is a control端 with a manager of a Swarm cluster.
type swarmHarness struct {
	t       *testing.T
	service *deploy.Service
	swarmID string
}

// newSwarmHarness starts a control端 with a manager of a Swarm cluster.
//
// The manager is enrolled with the identity of the Docker engine of a node,
// so the control端 knows the cluster the node belongs to and the role of the
// node inside of it.
func newSwarmHarness(t *testing.T, inventory fixedInventory) *swarmHarness {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "deploy.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	box, err := secret.New(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatalf("build the secret box: %v", err)
	}

	ctx := context.Background()
	enrollment := agentlink.NewEnrollment(db, time.Now)
	token, err := enrollment.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the enrollment token: %v", err)
	}
	identity := protocol.NodeIdentity{
		EngineID:    "engine-manager",
		SwarmID:     "swarm-1",
		SwarmNodeID: "node-1",
		Role:        protocol.RoleManager,
		Hostname:    "manager-1",
	}
	if _, err := enrollment.Redeem(ctx, token, identity); err != nil {
		t.Fatalf("redeem the enrollment token: %v", err)
	}

	return &swarmHarness{t: t, service: deploy.New(db, box, inventory), swarmID: "swarm-1"}
}

// createApp creates a Swarm application of the harness.
func (h *swarmHarness) createApp(namespace string) model.App {
	h.t.Helper()

	app, err := h.service.CreateApp(context.Background(), model.App{
		Name:      namespace,
		Mode:      "swarm",
		TargetID:  h.swarmID,
		Namespace: namespace,
	}, false)
	if err != nil {
		h.t.Fatalf("create the application: %v", err)
	}
	return app
}

// swarmInventory is the state of a cluster with a manager and a worker.
func swarmInventory() fixedInventory {
	return fixedInventory{value: protocol.Inventory{
		Identity: protocol.NodeIdentity{
			EngineID:    "engine-manager",
			SwarmID:     "swarm-1",
			SwarmNodeID: "node-1",
			Role:        protocol.RoleManager,
			Hostname:    "manager-1",
		},
		SwarmNodes: []protocol.SwarmNode{
			{ID: "node-1", Hostname: "manager-1", Role: protocol.RoleManager, Status: "ready"},
			{ID: "node-2", Hostname: "worker-1", Role: protocol.RoleWorker, Status: "ready"},
		},
	}}
}

// TestThePreviewNamesTheNodesThePlatformCouldNotCheck proves that the
// preview of a Swarm deployment names the nodes whose paths of the host no
// agent of the platform has verified.
//
// The manager checks the paths of its own node, so the existence of a path
// on the manager says nothing about the workers.
func TestThePreviewNamesTheNodesThePlatformCouldNotCheck(t *testing.T) {
	env := newSwarmHarness(t, swarmInventory())
	app := env.createApp("demo")
	cfg := model.Config{
		YAML: "services:\n  api:\n    image: nginx:alpine\n    volumes:\n      - /srv/data:/data\n",
	}

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	found := false
	for _, warning := range preview.Warnings {
		if strings.Contains(warning, "worker-1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the warnings = %q, want the node the platform could not check", preview.Warnings)
	}
}

// TestASwarmDeploymentNeedsTheConfirmationOfTheUnverifiedNodes proves that a
// Swarm deployment with a path of the host needs the confirmation of the
// administrator before it runs.
func TestASwarmDeploymentNeedsTheConfirmationOfTheUnverifiedNodes(t *testing.T) {
	env := newSwarmHarness(t, swarmInventory())
	app := env.createApp("demo")
	cfg := model.Config{
		YAML: "services:\n  api:\n    image: nginx:alpine\n    volumes:\n      - /srv/data:/data\n",
	}

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	_, err = env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  preview.Digest,
	})
	if !errors.Is(err, deploy.ErrConfirmationRequired) {
		t.Fatalf("error = %v, want a confirmation", err)
	}

	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-2",
		Config:         cfg,
		PreviewDigest:  preview.Digest,
		AcceptWarnings: true,
	}); err != nil {
		t.Fatalf("submit the confirmed deployment: %v", err)
	}
}

// TestASwarmDeploymentWithoutAPathNeedsNoConfirmation proves that a stack
// that mounts no path of the host needs no confirmation: there is nothing
// the manager of the cluster could not prove.
func TestASwarmDeploymentWithoutAPathNeedsNoConfirmation(t *testing.T) {
	env := newSwarmHarness(t, swarmInventory())
	app := env.createApp("demo")
	cfg := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n"}

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	for _, warning := range preview.Warnings {
		if deploy.IsRiskWarning(warning) {
			t.Fatalf("the warning %q is a risk, want no risk", warning)
		}
	}

	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  preview.Digest,
	}); err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}
}
