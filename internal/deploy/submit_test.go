package deploy_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hmoob-dock/internal/agentlink"
	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// fixedInventory is the state of a target.
//
// It replaces the Docker boundary of the tests only: the database of the
// tests is a real SQLite file and the identity of the node comes from a
// real enrollment.
type fixedInventory struct {
	value protocol.Inventory
	err   error
}

// ReadInventory returns the state of the target.
func (f fixedInventory) ReadInventory(ctx context.Context, mode, targetID string) (protocol.Inventory, error) {
	if f.err != nil {
		return protocol.Inventory{}, f.err
	}
	return f.value, nil
}

// switchableInventory is the state of a target that the test can take away.
type switchableInventory struct {
	mu    sync.Mutex
	value protocol.Inventory
	err   error
}

// ReadInventory returns the state of the target.
func (s *switchableInventory) ReadInventory(ctx context.Context, mode, targetID string) (protocol.Inventory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return protocol.Inventory{}, s.err
	}
	return s.value, nil
}

// unavailable makes the target answer with an error.
func (s *switchableInventory) unavailable(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// report replaces the state of the target.
func (s *switchableInventory) report(value protocol.Inventory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
}

// harness is a control端 with a node and a deployment service.
type harness struct {
	t       *testing.T
	db      *sql.DB
	service *deploy.Service
	nodeID  string
}

// newHarness starts a control端 with a node and a deployment service that
// reads the given state of the target.
func newHarness(t *testing.T, inventory fixedInventory) *harness {
	t.Helper()
	return newHarnessWith(t, inventory)
}

// newHarnessWith starts a control端 with a node and a deployment service
// that reads the given state of the target.
func newHarnessWith(t *testing.T, inventory deploy.InventoryReader) *harness {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "deploy.db"))
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	box, err := secret.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatalf("build the secret box: %v", err)
	}

	ctx := context.Background()
	enrollment := agentlink.NewEnrollment(db, time.Now)
	token, err := enrollment.Issue(ctx)
	if err != nil {
		t.Fatalf("issue the enrollment token: %v", err)
	}
	identity := protocol.NodeIdentity{EngineID: "engine-test"}
	credential, err := enrollment.Redeem(ctx, token, identity)
	if err != nil {
		t.Fatalf("redeem the enrollment token: %v", err)
	}

	return &harness{
		t:       t,
		db:      db,
		service: deploy.New(db, box, inventory),
		nodeID:  credential.NodeID,
	}
}

// createApp creates an application of the harness.
func (h *harness) createApp(namespace string) model.App {
	h.t.Helper()

	app, err := h.service.CreateApp(context.Background(), model.App{
		Name:      namespace,
		Mode:      "compose",
		TargetID:  h.nodeID,
		Namespace: namespace,
	}, false)
	if err != nil {
		h.t.Fatalf("create the application: %v", err)
	}
	return app
}

// createAdoptedApp creates an application whose namespace holds resources
// that the platform does not manage.
//
// The administrator confirmed the takeover of those resources.
func (h *harness) createAdoptedApp(namespace string) model.App {
	h.t.Helper()

	app, err := h.service.CreateApp(context.Background(), model.App{
		Name:      namespace,
		Mode:      "compose",
		TargetID:  h.nodeID,
		Namespace: namespace,
	}, true)
	if err != nil {
		h.t.Fatalf("create the application: %v", err)
	}
	return app
}

// preview asks the control端 for the impact of a configuration.
//
// Every deployment names the preview the administrator saw, so the tests
// ask for the preview of the configuration they deploy.
func (h *harness) preview(app model.App, baseVersionID string, cfg model.Config) deploy.Preview {
	h.t.Helper()

	preview, err := h.service.Preview(context.Background(), app.ID, baseVersionID, cfg)
	if err != nil {
		h.t.Fatalf("preview the deployment: %v", err)
	}
	return preview
}

// finishTask ends a task.
//
// The state machine of a task belongs to a later task of the plan, so the
// tests end a task with a direct write. An application holds at most one
// task that is not finished, so a test that deploys twice has to end the
// first deployment.
func (h *harness) finishTask(taskID string) {
	h.t.Helper()

	if _, err := h.db.Exec(
		`UPDATE tasks SET state = 'succeeded', finished_at = ? WHERE id = ?`,
		store.TimeToMillis(time.Now()), taskID,
	); err != nil {
		h.t.Fatalf("finish the task: %v", err)
	}
}

// countRows returns the number of rows of the given table.
func (h *harness) countRows(table string) int {
	h.t.Helper()

	var count int
	if err := h.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		h.t.Fatalf("count the rows of %s: %v", table, err)
	}
	return count
}

// composeConfig is a configuration with a single service.
func composeConfig(image string) model.Config {
	return model.Config{YAML: fmt.Sprintf("services:\n  api:\n    image: %s\n", image)}
}

func TestSubmitRetryReturnsSameTask(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "deploy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	box, err := secret.New(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	enrollment := agentlink.NewEnrollment(db, time.Now)
	token, err := enrollment.Issue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	identity := protocol.NodeIdentity{EngineID: "engine-test"}
	credential, err := enrollment.Redeem(ctx, token, identity)
	if err != nil {
		t.Fatal(err)
	}
	source := fixedInventory{value: protocol.Inventory{Identity: identity, CollectedAt: time.Now()}}
	svc := deploy.New(db, box, source)
	app, err := svc.CreateApp(ctx, model.App{Name: "demo", Mode: "compose", TargetID: credential.NodeID, Namespace: "demo"}, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n"}
	preview, err := svc.Preview(ctx, app.ID, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := deploy.SubmitRequest{AppID: app.ID, IdempotencyKey: "request-1", Config: cfg, PreviewDigest: preview.Digest, InventoryRevision: preview.InventoryRevision}
	first, err := svc.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Submit(ctx, req)
	if err != nil || first.ID != second.ID {
		t.Fatalf("duplicate task: %v", err)
	}
}

func TestSubmitWithTheSameKeyAndAnotherRequestIsAConflict(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := composeConfig("nginx:alpine")
	preview := env.preview(app, "", cfg)

	first, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  preview.Digest,
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}

	_, err = env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         composeConfig("nginx:1.27"),
	})
	if !errors.Is(err, deploy.ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want an idempotency conflict", err)
	}
	if env.countRows("tasks") != 1 {
		t.Fatalf("tasks = %d, want 1", env.countRows("tasks"))
	}
	if first.ID == "" {
		t.Fatal("the task has no identifier")
	}
}

func TestSubmitRepeatedlyAnswersTheTaskOfTheFirstRequest(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := composeConfig("nginx:alpine")
	preview := env.preview(app, "", cfg)
	request := deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  preview.Digest,
	}

	first, err := env.service.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}

	// The application is busy now, but the repeated request has to answer
	// with the task of the first request.
	second, err := env.service.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("repeat the request: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("task = %q, want %q", second.ID, first.ID)
	}

	// Another key of the busy application is refused.
	_, err = env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-2",
		Config:         cfg,
		PreviewDigest:  preview.Digest,
	})
	if !errors.Is(err, deploy.ErrBusy) {
		t.Fatalf("error = %v, want a busy application", err)
	}
}

func TestConcurrentSubmissionsWithOneKeyCreateOneTask(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := composeConfig("nginx:alpine")
	preview := env.preview(app, "", cfg)
	request := deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  preview.Digest,
	}

	const submissions = 20
	tasks := make([]model.Task, submissions)
	errs := make([]error, submissions)
	var wg sync.WaitGroup
	for i := 0; i < submissions; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			tasks[index], errs[index] = env.service.Submit(context.Background(), request)
		}(i)
	}
	wg.Wait()

	identifier := ""
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submission %d failed: %v", i, err)
		}
		if identifier == "" {
			identifier = tasks[i].ID
			continue
		}
		if tasks[i].ID != identifier {
			t.Fatalf("submission %d created the task %q, want %q", i, tasks[i].ID, identifier)
		}
	}
	if env.countRows("tasks") != 1 {
		t.Fatalf("tasks = %d, want 1", env.countRows("tasks"))
	}
	if env.countRows("versions") != 1 {
		t.Fatalf("versions = %d, want 1", env.countRows("versions"))
	}
}

func TestSubmitRollsTheVersionBackWithTheTask(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")

	// The database refuses the task, so the version of the same
	// transaction has to disappear as well.
	if _, err := env.db.Exec(`CREATE TRIGGER refuse_task BEFORE INSERT ON tasks
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatalf("create the trigger: %v", err)
	}

	_, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         composeConfig("nginx:alpine"),
		PreviewDigest:  env.preview(app, "", composeConfig("nginx:alpine")).Digest,
	})
	if err == nil {
		t.Fatal("the deployment was accepted although the database refused the task")
	}
	if env.countRows("versions") != 0 {
		t.Fatalf("versions = %d, want 0", env.countRows("versions"))
	}
	if env.countRows("tasks") != 0 {
		t.Fatalf("tasks = %d, want 0", env.countRows("tasks"))
	}

	var latest string
	if err := env.db.QueryRow(`SELECT latest_version_id FROM apps WHERE id = ?`, app.ID).Scan(&latest); err != nil {
		t.Fatalf("read the application: %v", err)
	}
	if latest != "" {
		t.Fatalf("the latest version = %q, want none", latest)
	}
}

func TestSubmitStoresTheConfigurationEncrypted(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: nginx:alpine\n    environment:\n      TOKEN: $TOKEN\n",
		Variables: map[string]string{"TOKEN": "the-secret-value"},
	}

	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  env.preview(app, "", cfg).Digest,
	}); err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}

	var yamlCiphertext, variableCiphertext []byte
	if err := env.db.QueryRow(
		`SELECT yaml_ciphertext, variables_ciphertext FROM versions WHERE app_id = ?`, app.ID,
	).Scan(&yamlCiphertext, &variableCiphertext); err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if bytes.Contains(yamlCiphertext, []byte(cfg.YAML)) {
		t.Fatal("the stored document holds the plain configuration")
	}
	if bytes.Contains(variableCiphertext, []byte("the-secret-value")) {
		t.Fatal("the stored variables hold the plain value")
	}
	if bytes.Contains(yamlCiphertext, []byte("nginx:alpine")) {
		t.Fatal("the stored document holds the plain image")
	}
}

func TestSubmitNeedsTheConfirmationOfARemovedService(t *testing.T) {
	inventory := fixedInventory{value: protocol.Inventory{
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
	env := newHarness(t, inventory)
	app := env.createAdoptedApp("demo")
	cfg := composeConfig("nginx:alpine")

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.RemovedServices) != 1 || preview.RemovedServices[0] != "db" {
		t.Fatalf("removed services = %v, want the service db", preview.RemovedServices)
	}

	request := deploy.SubmitRequest{
		AppID:             app.ID,
		IdempotencyKey:    "request-1",
		Config:            cfg,
		PreviewDigest:     preview.Digest,
		InventoryRevision: preview.InventoryRevision,
	}
	_, err = env.service.Submit(context.Background(), request)
	if !errors.Is(err, deploy.ErrConfirmationRequired) {
		t.Fatalf("error = %v, want a confirmation", err)
	}
	if env.countRows("tasks") != 0 {
		t.Fatalf("tasks = %d, want 0", env.countRows("tasks"))
	}

	request.ConfirmedRemoval = deploy.RemovalConfirmation{
		Digest:            preview.Digest,
		Services:          preview.RemovedServices,
		InventoryRevision: preview.InventoryRevision,
	}
	if _, err := env.service.Submit(context.Background(), request); err != nil {
		t.Fatalf("submit the confirmed deployment: %v", err)
	}
}

func TestSubmitRefusesTheConfirmationOfAnotherService(t *testing.T) {
	inventory := fixedInventory{value: protocol.Inventory{
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
	env := newHarness(t, inventory)
	app := env.createAdoptedApp("demo")
	cfg := composeConfig("nginx:alpine")

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}

	_, err = env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:             app.ID,
		IdempotencyKey:    "request-1",
		Config:            cfg,
		PreviewDigest:     preview.Digest,
		InventoryRevision: preview.InventoryRevision,
		ConfirmedRemoval: deploy.RemovalConfirmation{
			Digest:            preview.Digest,
			Services:          []string{"cache"},
			InventoryRevision: preview.InventoryRevision,
		},
	})
	if !errors.Is(err, deploy.ErrConfirmationRequired) {
		t.Fatalf("error = %v, want a confirmation", err)
	}
}

func TestSubmitRefusesTheConfirmationOfAnOlderTarget(t *testing.T) {
	inventory := fixedInventory{value: protocol.Inventory{
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
	env := newHarness(t, inventory)
	app := env.createAdoptedApp("demo")
	cfg := composeConfig("nginx:alpine")

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}

	// The confirmation of an older state of the target cannot be reused.
	_, err = env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:             app.ID,
		IdempotencyKey:    "request-1",
		Config:            cfg,
		PreviewDigest:     preview.Digest,
		InventoryRevision: "a-revision-of-an-older-state",
		ConfirmedRemoval: deploy.RemovalConfirmation{
			Digest:            preview.Digest,
			Services:          preview.RemovedServices,
			InventoryRevision: preview.InventoryRevision,
		},
	})
	if !errors.Is(err, deploy.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}
}

func TestSubmitNeedsTheConfirmationOfARiskyConfiguration(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n    privileged: true\n"}

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.Warnings) == 0 {
		t.Fatal("the preview holds no warning")
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
		t.Fatalf("submit the accepted deployment: %v", err)
	}
}

func TestSubmitDoesNotNeedTheConfirmationOfAMissingVariable(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := model.Config{YAML: "services:\n  api:\n    image: nginx:alpine\n    command: [\"echo\", \"$TAG\"]\n"}

	preview, err := env.service.Preview(context.Background(), app.ID, "", cfg)
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.Warnings) == 0 {
		t.Fatal("the preview holds no warning")
	}
	for _, warning := range preview.Warnings {
		if deploy.IsRiskWarning(warning) {
			t.Fatalf("the warning %q is a risk, want a notice about a variable", warning)
		}
	}

	// The notice of a variable does not stop the deployment.
	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  preview.Digest,
	}); err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}
}

func TestSubmitOfAnOutdatedPageIsRefused(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")
	cfg := composeConfig("nginx:alpine")

	first, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  env.preview(app, "", cfg).Digest,
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}
	env.finishTask(first.ID)

	// The page was opened before the first deployment, so it names no
	// version while the application has one now.
	changed := composeConfig("nginx:1.27")
	preview := env.preview(app, first.VersionID, changed)
	_, err = env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-2",
		BaseVersionID:  "",
		Config:         changed,
		PreviewDigest:  preview.Digest,
	})
	if !errors.Is(err, deploy.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}

	// The version of the first deployment is the base of the next one.
	second, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-3",
		BaseVersionID:  first.VersionID,
		Config:         changed,
		PreviewDigest:  preview.Digest,
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}
	if second.VersionID == first.VersionID {
		t.Fatal("the deployment reused the version of the first one")
	}
}

func TestThePreviewChangesNothing(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	app := env.createApp("demo")

	if _, err := env.service.Preview(context.Background(), app.ID, "", composeConfig("nginx:alpine")); err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if env.countRows("versions") != 0 {
		t.Fatalf("versions = %d, want 0", env.countRows("versions"))
	}
	if env.countRows("tasks") != 0 {
		t.Fatalf("tasks = %d, want 0", env.countRows("tasks"))
	}

	var latest string
	if err := env.db.QueryRow(`SELECT latest_version_id FROM apps WHERE id = ?`, app.ID).Scan(&latest); err != nil {
		t.Fatalf("read the application: %v", err)
	}
	if latest != "" {
		t.Fatalf("the latest version = %q, want none", latest)
	}
}

func TestTheCreationRefusesAnUnknownTarget(t *testing.T) {
	env := newHarness(t, fixedInventory{})

	_, err := env.service.CreateApp(context.Background(), model.App{
		Name:      "demo",
		Mode:      "compose",
		TargetID:  "a-node-that-does-not-exist",
		Namespace: "demo",
	}, false)
	if !errors.Is(err, deploy.ErrUnknownTarget) {
		t.Fatalf("error = %v, want an unknown target", err)
	}
}

func TestTheCreationRefusesANamespaceThatIsUsedAlready(t *testing.T) {
	env := newHarness(t, fixedInventory{})
	env.createApp("demo")

	_, err := env.service.CreateApp(context.Background(), model.App{
		Name:      "other",
		Mode:      "compose",
		TargetID:  env.nodeID,
		Namespace: "demo",
	}, false)
	if !errors.Is(err, deploy.ErrNamespaceConflict) {
		t.Fatalf("error = %v, want a namespace conflict", err)
	}
}

func TestTheCreationNeedsTheConfirmationOfForeignResources(t *testing.T) {
	inventory := fixedInventory{value: protocol.Inventory{
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
	env := newHarness(t, inventory)

	// The namespace holds a resource that the platform does not manage, so
	// the administrator has to confirm the takeover.
	_, err := env.service.CreateApp(context.Background(), model.App{
		Name:      "demo",
		Mode:      "compose",
		TargetID:  env.nodeID,
		Namespace: "demo",
	}, false)
	if !errors.Is(err, deploy.ErrConfirmationRequired) {
		t.Fatalf("error = %v, want a confirmation", err)
	}
	if env.countRows("apps") != 0 {
		t.Fatalf("apps = %d, want 0", env.countRows("apps"))
	}

	// The confirmation creates the application.
	if _, err := env.service.CreateApp(context.Background(), model.App{
		Name:      "demo",
		Mode:      "compose",
		TargetID:  env.nodeID,
		Namespace: "demo",
	}, true); err != nil {
		t.Fatalf("create the adopted application: %v", err)
	}
}

func TestTheContainerOfThePlatformIsNeverAdopted(t *testing.T) {
	inventory := fixedInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
		Containers: []protocol.Container{{
			ID:    "container-agent",
			Name:  "hmoob-agent-1",
			Image: "hmoob-dock-agent:local",
			Labels: map[string]string{
				"com.docker.compose.project": "demo",
				"com.docker.compose.service": "agent",
			},
		}},
	}}
	env := newHarness(t, inventory)

	// The confirmation of the administrator does not help: the platform
	// never takes over its own containers.
	_, err := env.service.CreateApp(context.Background(), model.App{
		Name:      "demo",
		Mode:      "compose",
		TargetID:  env.nodeID,
		Namespace: "demo",
	}, true)
	if err == nil {
		t.Fatal("the platform took over its own container")
	}
}

func TestSubmitOfAnOfflineTargetIsRefused(t *testing.T) {
	source := &switchableInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
	}}
	env := newHarnessWith(t, source)
	app := env.createApp("demo")
	cfg := composeConfig("nginx:alpine")

	// The deployment of the application succeeds while the target answers.
	task, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-1",
		Config:         cfg,
		PreviewDigest:  env.preview(app, "", cfg).Digest,
	})
	if err != nil {
		t.Fatalf("submit the deployment: %v", err)
	}
	env.finishTask(task.ID)

	// The administrator looks at the impact of the next deployment before
	// the agent of the node is gone.
	changed := composeConfig("nginx:1.27")
	preview := env.preview(app, task.VersionID, changed)

	// The agent of the node is gone.
	source.unavailable(errors.New("the node is not connected"))

	if _, err := env.service.Preview(context.Background(), app.ID, task.VersionID, cfg); !errors.Is(err, deploy.ErrTargetUnavailable) {
		t.Fatalf("error = %v, want an unavailable target", err)
	}
	if _, err := env.service.Submit(context.Background(), deploy.SubmitRequest{
		AppID:          app.ID,
		IdempotencyKey: "request-2",
		BaseVersionID:  task.VersionID,
		Config:         changed,
		PreviewDigest:  preview.Digest,
	}); !errors.Is(err, deploy.ErrTargetUnavailable) {
		t.Fatalf("error = %v, want an unavailable target", err)
	}

	// The stored configuration stays readable while the target is away.
	version, err := env.service.GetVersion(context.Background(), app.ID, task.VersionID)
	if err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if !strings.Contains(version.Config.YAML, "nginx:alpine") {
		t.Fatalf("the version holds %q, want the stored configuration", version.Config.YAML)
	}
}
