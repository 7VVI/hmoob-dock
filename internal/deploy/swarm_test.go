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

// swarmStackInventory is the state of a cluster whose stack demo holds two
// services: the service api and the service cache.
//
// The tasks of the services run on the worker, so the manager holds no
// container of the stack at all.
func swarmStackInventory() fixedInventory {
	inventory := swarmInventory()
	inventory.value.SwarmServices = []protocol.SwarmService{
		{
			ID:   "service-api",
			Name: "demo_api",
			Mode: "replicated",
			Labels: map[string]string{
				"com.docker.stack.namespace": "demo",
			},
		},
		{
			ID:   "service-cache",
			Name: "demo_cache",
			Mode: "replicated",
			Labels: map[string]string{
				"com.docker.stack.namespace": "demo",
			},
		},
		{
			ID:   "service-other",
			Name: "other_api",
			Mode: "replicated",
			Labels: map[string]string{
				"com.docker.stack.namespace": "other",
			},
		},
	}
	return inventory
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

// TestThePreviewNamesTheServicesOfTheStackThatTheConfigurationDoesNotHold
// proves that the preview of a Swarm deployment names the services of the
// cluster that the configuration does not hold any more.
//
// The services of a stack are the services of the cluster and not the
// containers of a node: a service whose tasks run on the workers has no
// container on the manager, so the containers of the manager never tell the
// whole stack.
func TestThePreviewNamesTheServicesOfTheStackThatTheConfigurationDoesNotHold(t *testing.T) {
	env := newSwarmHarness(t, swarmStackInventory())
	app := env.createApp("demo")
	cfg := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n"}

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.RemovedServices) != 1 || preview.RemovedServices[0] != "cache" {
		t.Fatalf("the removed services = %v, want the service cache", preview.RemovedServices)
	}
}

// TestThePreviewNamesTheServicesOfTheStackWithTheNameOfTheExecutor proves
// that the control端 and the executor of the stack name a service of a stack
// the same way.
//
// The name of a service of a stack is the name of the stack, an underscore
// and the name of the service, so the name of the service is what follows
// the namespace. A service of another stack is never part of the answer,
// because the executor only reads the services of the stack of the
// application.
func TestThePreviewNamesTheServicesOfTheStackWithTheNameOfTheExecutor(t *testing.T) {
	env := newSwarmHarness(t, swarmStackInventory())
	app := env.createApp("demo")
	cfg := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n"}

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.RemovedServices) != 1 || preview.RemovedServices[0] != "cache" {
		t.Fatalf("the removed services = %v, want the service cache of the stack demo", preview.RemovedServices)
	}
	for _, name := range preview.RemovedServices {
		if strings.Contains(name, "demo_") {
			t.Fatalf("the removed service %q holds the name of the stack", name)
		}
	}
}

// TestASwarmDeploymentRemovesTheServiceTheAdministratorConfirmed proves that
// the confirmation of a removal of a Swarm stack can be produced at all.
//
// The set of the services the administrator confirms has to be the set of
// the services of the cluster the configuration does not hold, or the
// executor refuses the deployment of every removal.
func TestASwarmDeploymentRemovesTheServiceTheAdministratorConfirmed(t *testing.T) {
	env := newSwarmHarness(t, swarmStackInventory())
	app := env.createApp("demo")
	cfg := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n"}

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.RemovedServices) != 1 || preview.RemovedServices[0] != "cache" {
		t.Fatalf("the removed services = %v, want the service cache", preview.RemovedServices)
	}
	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:             app.ID,
		IdempotencyKey:    "request-1",
		Config:            cfg,
		PreviewDigest:     preview.Digest,
		InventoryRevision: preview.InventoryRevision,
		ConfirmedRemoval:  preview.RemovalConfirmation(),
	}); err != nil {
		t.Fatalf("submit the confirmed deployment: %v", err)
	}
}

// TestTheRevisionOfTheTargetHoldsTheServicesOfTheCluster proves that the
// revision of the target names the services of the cluster.
//
// The confirmation of a removal is bound to the revision, so a confirmation
// of a state that does not hold the services of the cluster any more may
// not be reused.
func TestTheRevisionOfTheTargetHoldsTheServicesOfTheCluster(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n"}

	first := newSwarmHarness(t, swarmStackInventory())
	app := first.createApp("demo")
	before, err := first.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}

	changed := swarmStackInventory()
	changed.value.SwarmServices = append(changed.value.SwarmServices, protocol.SwarmService{
		ID:   "service-extra",
		Name: "demo_extra",
		Mode: "replicated",
		Labels: map[string]string{
			"com.docker.stack.namespace": "demo",
		},
	})
	second := newSwarmHarness(t, changed)
	other := second.createApp("demo")
	after, err := second.service.Preview(context.Background(), other.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}

	if before.InventoryRevision == after.InventoryRevision {
		t.Fatalf("the revision = %q, want the revision of the state of the cluster", after.InventoryRevision)
	}
}
