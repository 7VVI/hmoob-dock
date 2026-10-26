package deploy_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

func TestTheHistoryHoldsNoConfiguration(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: nginx:alpine\n",
		Variables: map[string]string{"TOKEN": "the-secret-value"},
	}

	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
	}); err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}

	versions, err := env.service.ListVersions(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(versions))
	}
	if versions[0].ID == "" {
		t.Fatal("the version has no identifier")
	}
	if versions[0].CreatedAt.IsZero() {
		t.Fatal("the version has no creation time")
	}
}

func TestTheHistoryVersionHoldsTheConfiguration(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: nginx:alpine\n",
		Variables: map[string]string{"TOKEN": "the-secret-value"},
	}

	task, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}

	version, err := env.service.GetVersion(context.Background(), app.ID, task.VersionID)
	if err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if version.Config.YAML != cfg.YAML {
		t.Fatalf("the version holds %q, want the submitted document", version.Config.YAML)
	}
	if version.Config.Variables["TOKEN"] != "the-secret-value" {
		t.Fatalf("the variables of the version = %v, want the submitted variables", version.Config.Variables)
	}
}

func TestTheVersionOfAnotherApplicationIsUnknown(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	other := env.createApp("other")

	task, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         composeConfig("nginx:alpine"),
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}

	if _, err := env.service.GetVersion(context.Background(), other.ID, task.VersionID); !errors.Is(err, deploy.ErrUnknownVersion) {
		t.Fatalf("error = %v, want an unknown version", err)
	}
}

func TestTheHistoryKeepsTheContentOfAVersion(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")

	first, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         composeConfig("nginx:alpine"),
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}
	env.finishTask(first.ID)

	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-2",
		BaseVersionID:  first.VersionID,
		Config:         composeConfig("nginx:1.27"),
	}); err != nil {
		t.Fatalf("submit the second deployment: %v", err)
	}

	// The old version still holds the document of the first deployment.
	version, err := env.service.GetVersion(context.Background(), app.ID, first.VersionID)
	if err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if !strings.Contains(version.Config.YAML, "nginx:alpine") {
		t.Fatalf("the version holds %q, want the document of the first deployment", version.Config.YAML)
	}
	if strings.Contains(version.Config.YAML, "nginx:1.27") {
		t.Fatal("the old version holds the document of the second deployment")
	}
}

func TestTheHistoryRedeployCreatesANewVersionAndANewTask(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")

	first, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         composeConfig("nginx:alpine"),
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}
	env.finishTask(first.ID)

	// The redeployment loads the old configuration and deploys it again as
	// a new version.
	old, err := env.service.GetVersion(context.Background(), app.ID, first.VersionID)
	if err != nil {
		t.Fatalf("read the version: %v", err)
	}
	preview, err := env.service.Preview(context.Background(), app.ID, first.VersionID, old.Config)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	second, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:             app.ID,
		IdempotencyKey:    "request-2",
		BaseVersionID:     first.VersionID,
		Config:            old.Config,
		PreviewDigest:     preview.Digest,
		InventoryRevision: preview.InventoryRevision,
	})
	if err != nil {
		t.Fatalf("submit the redeployment: %v", err)
	}
	if second.VersionID == first.VersionID {
		t.Fatal("the redeployment reused the version of the first deployment")
	}
	if second.ID == first.ID {
		t.Fatal("the redeployment reused the task of the first deployment")
	}

	versions, err := env.service.ListVersions(context.Background(), app.ID)
	if err != nil {
		t.Fatalf("read the versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(versions))
	}
}

func TestTheContainerOfThePlatformIsNeverRemoved(t *testing.T) {
	source := &switchableInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
	}}
	env := newHarnessWith(t, source)
	app := env.createApp("demo")

	// The agent of the node reports a container of the platform itself and
	// a service of the application.
	source.report(protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
		Containers: []protocol.Container{
			{
				ID:    "container-agent",
				Name:  "demo-agent-1",
				Image: "hmoob-dock-agent:local",
				Labels: map[string]string{
					"com.docker.compose.project": "demo",
					"com.docker.compose.service": "agent",
				},
			},
			{
				ID:    "container-cache",
				Name:  "demo-cache-1",
				Image: "redis:7",
				Labels: map[string]string{
					"com.docker.compose.project": "demo",
					"com.docker.compose.service": "cache",
				},
			},
		},
	})

	preview, err := env.service.Preview(context.Background(), app.ID, "", composeConfig("nginx:alpine"))
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.RemovedServices) != 1 || preview.RemovedServices[0] != "cache" {
		t.Fatalf("removed services = %v, want the service cache", preview.RemovedServices)
	}
}

func TestTheCommandOfATaskHoldsTheConfiguration(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: nginx:alpine\n",
		Variables: map[string]string{"TOKEN": "the-secret-value"},
	}

	task, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}

	command, err := env.service.LoadCommand(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("load the command: %v", err)
	}
	if command.Task.ID != task.ID {
		t.Fatalf("the command holds the task %q, want %q", command.Task.ID, task.ID)
	}
	if command.App.ID != app.ID {
		t.Fatalf("the command holds the application %q, want %q", command.App.ID, app.ID)
	}
	if command.Config.YAML != cfg.YAML {
		t.Fatalf("the command holds %q, want the submitted document", command.Config.YAML)
	}
	if command.Config.Variables["TOKEN"] != "the-secret-value" {
		t.Fatalf("the variables of the command = %v, want the submitted variables", command.Config.Variables)
	}
	if command.ExecutionTimeout <= 0 || command.VerifyTimeout <= 0 {
		t.Fatalf("the command holds the timeouts %v and %v, want two limits",
			command.ExecutionTimeout, command.VerifyTimeout)
	}
}

func TestTheTaskOfAnUnknownIdentifierIsUnknown(t *testing.T) {
	env := newHarness(t, fixedInventory{})

	if _, err := env.service.Task(context.Background(), "a-task-that-does-not-exist"); !errors.Is(err, deploy.ErrUnknownTask) {
		t.Fatalf("error = %v, want an unknown task", err)
	}
}

func TestTheInventoryOfTheTargetDecidesTheRemovedServices(t *testing.T) {
	inventory := fixedInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
		Containers: []protocol.Container{
			{
				ID:    "container-api",
				Name:  "demo-api-1",
				Image: "nginx:alpine",
				Labels: map[string]string{
					"com.docker.compose.project": "demo",
					"com.docker.compose.service": "api",
				},
			},
			{
				ID:    "container-cache",
				Name:  "demo-cache-1",
				Image: "redis:7",
				Labels: map[string]string{
					"com.docker.compose.project": "demo",
					"com.docker.compose.service": "cache",
				},
			},
			{
				ID:    "container-other",
				Name:  "other-api-1",
				Image: "nginx:alpine",
				Labels: map[string]string{
					"com.docker.compose.project": "other",
					"com.docker.compose.service": "api",
				},
			},
		},
	}}
	env := newHarness(t, inventory)
	app := env.createAdoptedApp("demo")

	preview, err := env.service.Preview(context.Background(), app.ID, "", composeConfig("nginx:alpine"))
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.RemovedServices) != 1 || preview.RemovedServices[0] != "cache" {
		t.Fatalf("removed services = %v, want the service cache", preview.RemovedServices)
	}
	if preview.InventoryRevision == "" {
		t.Fatal("the preview holds no revision of the target")
	}
}
