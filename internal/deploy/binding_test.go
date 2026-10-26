package deploy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// competingInventory is the state of a target that changes while the
// control端 reads it.
//
// The change stands for a deployment of another administrator that finished
// while the request was reading the target.
type competingInventory struct {
	value protocol.Inventory
	// before changes the state of the control端 before the state of the
	// target is answered.
	before func()
}

// ReadInventory answers with the state of the target.
func (c *competingInventory) ReadInventory(ctx context.Context, mode, targetID string) (protocol.Inventory, error) {
	if c.before != nil {
		c.before()
	}
	return c.value, nil
}

// removalInventory holds the state of a target with a service the
// configuration does not hold any more.
func removalInventory() fixedInventory {
	return fixedInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
		Containers: []protocol.Container{{
			ID:    "container-db",
			Name:  "demo-db-1",
			Image: "postgres:16",
			Labels: map[string]string{
				"com.docker.compose.project": "demo",
				"com.docker.compose.service": "db",
			},
		}},
	}}
}

func TestTheSubmissionOfAnOutdatedPageChangesNothing(t *testing.T) {
	source := &competingInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
	}}
	env := newHarnessWith(t, source)
	app := env.createApp("demo")
	cfg := composeConfig("nginx:alpine")

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}

	// Another deployment finishes while this request reads the target: the
	// application points at another version now.
	source.before = func() {
		if _, err := env.db.Exec(
			`UPDATE apps SET latest_version_id = 'version-of-another-deployment' WHERE id = ?`,
			app.ID,
		); err != nil {
			t.Fatalf("move the application to another version: %v", err)
		}
	}

	_, err = env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:             app.ID,
		IdempotencyKey:    "request-1",
		Config:            cfg,
		PreviewDigest:     preview.Digest,
		InventoryRevision: preview.InventoryRevision,
	})
	if !errors.Is(err, deploy.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}

	// The version of the other deployment is still the latest one.
	var latest string
	if err := env.db.QueryRow(`SELECT latest_version_id FROM apps WHERE id = ?`, app.ID).Scan(&latest); err != nil {
		t.Fatalf("read the application: %v", err)
	}
	if latest != "version-of-another-deployment" {
		t.Fatalf("the latest version = %q, want the version of the other deployment", latest)
	}
	if env.countRows("versions") != 0 {
		t.Fatalf("versions = %d, want 0", env.countRows("versions"))
	}
	if env.countRows("tasks") != 0 {
		t.Fatalf("tasks = %d, want 0", env.countRows("tasks"))
	}
}

func TestTheSubmissionWithoutAPreviewIsRefused(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")

	// The request names no preview, so it is refused before the
	// configuration is looked at: nobody saw what the deployment does.
	_, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         model.Config{YAML: "services:\n  api:\n    build: .\n"},
	})
	if !errors.Is(err, deploy.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}
	if env.countRows("tasks") != 0 {
		t.Fatalf("tasks = %d, want 0", env.countRows("tasks"))
	}
	if env.countRows("versions") != 0 {
		t.Fatalf("versions = %d, want 0", env.countRows("versions"))
	}
}

func TestTheSubmissionWithAnotherPreviewIsRefused(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := composeConfig("nginx:alpine")

	if _, err := env.service.Preview(context.Background(), app.ID, "", cfg); err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}

	_, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  "the-digest-of-another-configuration",
	})
	if !errors.Is(err, deploy.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}
	if env.countRows("tasks") != 0 {
		t.Fatalf("tasks = %d, want 0", env.countRows("tasks"))
	}
}

func TestTheConfirmationOfAnotherConfigurationIsRefused(t *testing.T) {
	env := newHarness(t, removalInventory())
	app := env.createAdoptedApp("demo")
	first := composeConfig("nginx:alpine")
	second := composeConfig("nginx:1.27")

	preview, err := env.service.Preview(context.Background(), app.ID, "", first)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.RemovedServices) != 1 {
		t.Fatalf("removed services = %v, want the service db", preview.RemovedServices)
	}

	// The administrator changes the configuration and looks at the impact
	// of the new one.
	other, err := env.service.Preview(context.Background(), app.ID, "", second)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}

	// The confirmation of the first preview is used for the second
	// configuration. The removed service is the same, so only the digest
	// of the configuration tells the two previews apart.
	_, err = env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:             app.ID,
		IdempotencyKey:    "request-1",
		Config:            second,
		PreviewDigest:     other.Digest,
		InventoryRevision: preview.InventoryRevision,
		ConfirmedRemoval: deploy.RemovalConfirmation{
			Digest:            preview.Digest,
			Services:          preview.RemovedServices,
			InventoryRevision: preview.InventoryRevision,
		},
	})
	if !errors.Is(err, deploy.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}
	if env.countRows("tasks") != 0 {
		t.Fatalf("tasks = %d, want 0", env.countRows("tasks"))
	}
}

func TestTheConfirmationOfTheConfigurationIsAccepted(t *testing.T) {
	env := newHarness(t, removalInventory())
	app := env.createAdoptedApp("demo")
	cfg := composeConfig("nginx:alpine")

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}

	task, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:             app.ID,
		IdempotencyKey:    "request-1",
		Config:            cfg,
		PreviewDigest:     preview.Digest,
		InventoryRevision: preview.InventoryRevision,
		ConfirmedRemoval:  preview.RemovalConfirmation(),
	})
	if err != nil {
		t.Fatalf("submit the confirmed deployment: %v", err)
	}
	if task.ID == "" {
		t.Fatal("the task has no identifier")
	}
}

func TestTheWarningsAreClassifiedByThePlatform(t *testing.T) {
	if deploy.IsRiskWarning("变量 TAG 没有提供，将按空字符串处理") {
		t.Fatal("the notice of a variable is a risk")
	}
	if deploy.IsRiskWarning("服务 X 引用了未声明的 Y") {
		t.Fatal("the message of a service that names a missing resource is a risk")
	}
	if !deploy.IsRiskWarning("服务 api 以特权模式运行") {
		t.Fatal("the risk of a service is no risk")
	}
}
