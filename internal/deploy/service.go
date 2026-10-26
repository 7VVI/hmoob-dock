// Package deploy owns the applications of the control端: their versions,
// their deployment tasks and the transactions that write them.
//
// The package never changes a target. It reads the state of a target
// through the InventoryReader and writes the versions and the tasks of the
// deployments, so a preview is a pure read and a submission changes only
// the database of the control端. The execution of a task belongs to the
// agents.
package deploy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"hmoob-dock/internal/config"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
	"hmoob-dock/internal/yamlspec"
)

// InventoryReader reads the state of a target.
//
// The hub of the agents implements it: the Compose mode reads the inventory
// of a node, the Swarm mode the inventory of a manager of the cluster. A
// target that does not answer is an error, because a deployment must not
// work with a state that nobody observes any more.
type InventoryReader interface {
	ReadInventory(ctx context.Context, mode, targetID string) (protocol.Inventory, error)
}

// The errors of the package. The HTTP layer turns them into status codes
// and messages.
var (
	// ErrBusy reports an application that has a task that is not finished.
	ErrBusy = errors.New("deploy: the application has a task that is not finished")
	// ErrConflict reports a request that does not match the current state
	// of the application, for example a page that is out of date.
	ErrConflict = errors.New("deploy: the request does not match the current state of the application")
	// ErrIdempotencyConflict reports an idempotency key that was used with
	// another request.
	ErrIdempotencyConflict = errors.New("deploy: the idempotency key was used with another request")
	// ErrConfirmationRequired reports a deployment that needs an explicit
	// confirmation of the administrator.
	ErrConfirmationRequired = errors.New("deploy: the deployment needs an explicit confirmation")
	// ErrUnknownApp reports an application that does not exist.
	ErrUnknownApp = errors.New("deploy: the application is unknown")
	// ErrUnknownVersion reports a version that does not exist.
	ErrUnknownVersion = errors.New("deploy: the version is unknown")
	// ErrUnknownTask reports a task that does not exist.
	ErrUnknownTask = errors.New("deploy: the task is unknown")
	// ErrUnknownTarget reports a node or a cluster that the control端 does
	// not know.
	ErrUnknownTarget = errors.New("deploy: the target is unknown")
	// ErrTargetUnavailable reports a target whose state cannot be read,
	// for example a node whose agent is not connected.
	ErrTargetUnavailable = errors.New("deploy: the state of the target is not available")
	// ErrNamespaceConflict reports a namespace that another application of
	// the same target uses already.
	ErrNamespaceConflict = errors.New("deploy: the namespace is used by another application")
	// ErrPlatformResource reports a namespace that holds a container of the
	// platform itself.
	ErrPlatformResource = errors.New("deploy: the namespace holds a resource of the platform itself")
	// ErrInvalidApp reports an application whose fields are not usable.
	ErrInvalidApp = errors.New("deploy: the application is not valid")
	// ErrInvalidRequest reports a request whose fields are not usable.
	ErrInvalidRequest = errors.New("deploy: the request is not valid")
)

// The limits of an application.
const (
	// maxNameLength is the longest name an application may have.
	maxNameLength = 128
	// maxNamespaceLength is the longest namespace an application may have.
	maxNamespaceLength = 63
)

// The limits of a deployment task.
const (
	// DefaultExecutionTimeout bounds the execution of the Docker command of
	// a task.
	DefaultExecutionTimeout = 10 * time.Minute
	// DefaultVerifyTimeout bounds the wait for the services of a task.
	DefaultVerifyTimeout = 10 * time.Minute
)

// The labels Docker writes on the resources of a Compose project and of a
// Swarm stack.
const (
	// composeProjectLabel names the Compose project of a container.
	composeProjectLabel = "com.docker.compose.project"
	// composeServiceLabel names the service of a container.
	composeServiceLabel = "com.docker.compose.service"
	// stackNamespaceLabel names the Swarm stack of a container.
	stackNamespaceLabel = "com.docker.stack.namespace"
	// swarmServiceLabel names the Swarm service of a container.
	swarmServiceLabel = "com.docker.swarm.service.name"
)

// Service owns the applications of the control端.
type Service struct {
	db  *sql.DB
	box *secret.Box
	// inventory reads the state of the targets.
	inventory InventoryReader
	// now provides the timestamps of the records.
	now func() time.Time
	// platformImages are the repositories of the images of the platform
	// itself.
	platformImages map[string]bool
}

// New builds the deployment service.
//
// The database is the write pool of the control端: the versions and the
// tasks of a deployment are written in one transaction, so they need the
// same connection. The inventory reader is the source of the state of the
// targets; the hub of the agents implements it.
//
// The images of the platform itself are read from the configuration, so a
// rename of them does not turn the containers of the platform into managed
// resources.
func New(db *sql.DB, box *secret.Box, inventory InventoryReader) *Service {
	return &Service{
		db:             db,
		box:            box,
		inventory:      inventory,
		now:            time.Now,
		platformImages: platformImageSet(config.PlatformImages()),
	}
}

// platformImageSet returns the repositories of the given images.
//
// The images are normalized the same way the images of the containers are,
// so an image of the list may be written with a registry, with a tag or
// with a digest.
func platformImageSet(images []string) map[string]bool {
	set := make(map[string]bool, len(images))
	for _, image := range images {
		if repository := imageRepository(image); repository != "" {
			set[repository] = true
		}
	}
	return set
}

// CreateApp creates an application.
//
// The target of the application has to belong to the control端 and its state
// has to be readable: the platform never takes over a resource it cannot
// see. The adopt flag only records that the administrator confirmed the
// takeover of the resources of the namespace that the platform does not
// manage itself; it never skips the inventory of the target.
func (s *Service) CreateApp(ctx context.Context, app model.App, adopt bool) (model.App, error) {
	app.Name = strings.TrimSpace(app.Name)
	app.Mode = strings.ToLower(strings.TrimSpace(app.Mode))
	app.TargetID = strings.TrimSpace(app.TargetID)
	app.Namespace = strings.TrimSpace(app.Namespace)

	if app.Name == "" || len(app.Name) > maxNameLength {
		return model.App{}, fmt.Errorf("%w: the name of the application is missing or too long", ErrInvalidApp)
	}
	if app.Mode != yamlspec.ModeCompose && app.Mode != yamlspec.ModeSwarm {
		return model.App{}, fmt.Errorf("%w: the mode %q is not supported", ErrInvalidApp, app.Mode)
	}
	if app.TargetID == "" {
		return model.App{}, fmt.Errorf("%w: the application has no target", ErrInvalidApp)
	}
	if !validNamespace(app.Namespace) {
		return model.App{}, fmt.Errorf("%w: the namespace %q is not a valid name", ErrInvalidApp, app.Namespace)
	}

	// The target has to belong to the control端.
	if err := s.checkTarget(ctx, app); err != nil {
		return model.App{}, err
	}

	// The state of the target decides which resources of the namespace the
	// platform would take over.
	inventory, err := s.readInventory(ctx, app)
	if err != nil {
		return model.App{}, err
	}
	existing := namespaceContainers(inventory, app.Mode, app.Namespace)
	for _, container := range existing {
		if s.isPlatformContainer(container) {
			return model.App{}, fmt.Errorf("%w: the container %s", ErrPlatformResource, container.Name)
		}
	}
	if len(existing) > 0 && !adopt {
		return model.App{}, fmt.Errorf(
			"%w: the namespace %q holds %d resources that the platform does not manage",
			ErrConfirmationRequired, app.Namespace, len(existing),
		)
	}

	identifier, err := randomID()
	if err != nil {
		return model.App{}, err
	}
	app.ID = identifier
	app.LatestVersionID = ""
	app.SuccessfulVersionID = ""

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO apps (id, name, mode, target_id, namespace, latest_version_id, successful_version_id, created_at)
		 VALUES (?, ?, ?, ?, ?, '', '', ?)`,
		app.ID, app.Name, app.Mode, app.TargetID, app.Namespace, store.TimeToMillis(s.now())); err != nil {
		if uniqueViolation(err) {
			return model.App{}, fmt.Errorf("%w: the namespace %q", ErrNamespaceConflict, app.Namespace)
		}
		return model.App{}, fmt.Errorf("deploy: store the application: %w", err)
	}
	return app, nil
}

// ListApps returns the applications of the control端.
func (s *Service) ListApps(ctx context.Context) ([]model.App, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, mode, target_id, namespace, latest_version_id, successful_version_id
		 FROM apps ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("deploy: read the applications: %w", err)
	}
	defer rows.Close()

	apps := []model.App{}
	for rows.Next() {
		var app model.App
		if err := rows.Scan(&app.ID, &app.Name, &app.Mode, &app.TargetID, &app.Namespace,
			&app.LatestVersionID, &app.SuccessfulVersionID); err != nil {
			return nil, fmt.Errorf("deploy: scan the application: %w", err)
		}
		apps = append(apps, app)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deploy: read the applications: %w", err)
	}
	return apps, nil
}

// Task returns the deployment task of the given identifier.
func (s *Service) Task(ctx context.Context, taskID string) (model.Task, error) {
	record, err := s.task(ctx, taskID)
	if err != nil {
		return model.Task{}, err
	}
	return record.task, nil
}

// LoadCommand loads the command an agent receives for a task.
//
// The command holds the decrypted configuration of the version of the task:
// the agent writes it into the workspace of the application. The services
// the administrator confirmed the removal of are part of the command, so
// the agent removes only what was confirmed.
func (s *Service) LoadCommand(ctx context.Context, taskID string) (model.Command, error) {
	record, err := s.task(ctx, taskID)
	if err != nil {
		return model.Command{}, err
	}
	app, err := s.app(ctx, record.task.AppID)
	if err != nil {
		return model.Command{}, err
	}
	cfg, err := s.loadConfig(ctx, app.ID, record.task.VersionID)
	if err != nil {
		return model.Command{}, err
	}
	return model.Command{
		Task:             record.task,
		App:              app,
		Config:           cfg,
		RemovedServices:  record.removedServices,
		ExecutionTimeout: DefaultExecutionTimeout,
		VerifyTimeout:    DefaultVerifyTimeout,
	}, nil
}

// GetApp returns the application of the given identifier.
func (s *Service) GetApp(ctx context.Context, appID string) (model.App, error) {
	return s.app(ctx, appID)
}

// app reads the application of the given identifier.
func (s *Service) app(ctx context.Context, appID string) (model.App, error) {
	var app model.App
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, mode, target_id, namespace, latest_version_id, successful_version_id
		 FROM apps WHERE id = ?`, appID).
		Scan(&app.ID, &app.Name, &app.Mode, &app.TargetID, &app.Namespace,
			&app.LatestVersionID, &app.SuccessfulVersionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.App{}, fmt.Errorf("%w: %s", ErrUnknownApp, appID)
	case err != nil:
		return model.App{}, fmt.Errorf("deploy: read the application: %w", err)
	}
	return app, nil
}

// taskRecord is a task of the database with the fields the control端 needs
// beside the task itself.
type taskRecord struct {
	// task is the task of the deployment.
	task model.Task
	// idempotencyKey identifies the request that created the task.
	idempotencyKey string
	// requestDigest identifies the content of the request that created the
	// task.
	requestDigest string
	// removedServices are the services the administrator confirmed the
	// removal of.
	removedServices []string
}

// task reads the task of the given identifier.
func (s *Service) task(ctx context.Context, taskID string) (*taskRecord, error) {
	var record taskRecord
	var removed string
	var lastSeq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, app_id, version_id, agent_id, state, last_seq, digest, idempotency_key, request_digest, removed_services
		 FROM tasks WHERE id = ?`, taskID).
		Scan(&record.task.ID, &record.task.AppID, &record.task.VersionID, &record.task.AgentID,
			&record.task.State, &lastSeq, &record.task.Digest, &record.idempotencyKey,
			&record.requestDigest, &removed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s", ErrUnknownTask, taskID)
	case err != nil:
		return nil, fmt.Errorf("deploy: read the task: %w", err)
	}
	record.task.LastSeq = uint64(lastSeq)
	services, err := decodeServices(removed)
	if err != nil {
		return nil, fmt.Errorf("deploy: read the task: %w", err)
	}
	record.removedServices = services
	return &record, nil
}

// checkTarget makes sure that the target of an application belongs to the
// control端.
//
// A Compose application runs on a node, a Swarm application on a cluster.
// The cluster needs an enrolled manager, because only a manager can answer
// for the cluster.
func (s *Service) checkTarget(ctx context.Context, app model.App) error {
	var count int
	var err error
	switch app.Mode {
	case yamlspec.ModeSwarm:
		err = s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM nodes WHERE swarm_id = ? AND role = ? AND revoked_at IS NULL`,
			app.TargetID, protocol.RoleManager).Scan(&count)
	default:
		err = s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM nodes WHERE id = ? AND revoked_at IS NULL`, app.TargetID).Scan(&count)
	}
	if err != nil {
		return fmt.Errorf("deploy: read the target: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: %s", ErrUnknownTarget, app.TargetID)
	}
	return nil
}

// readInventory reads the state of the target of an application.
func (s *Service) readInventory(ctx context.Context, app model.App) (protocol.Inventory, error) {
	if s.inventory == nil {
		return protocol.Inventory{}, fmt.Errorf("%w: the control端 has no source of the state of the targets", ErrTargetUnavailable)
	}
	inventory, err := s.inventory.ReadInventory(ctx, app.Mode, app.TargetID)
	if err != nil {
		return protocol.Inventory{}, fmt.Errorf("%w: %w", ErrTargetUnavailable, err)
	}
	return inventory, nil
}

// namespaceContainers returns the containers of the inventory that belong to
// the namespace of an application.
func namespaceContainers(inventory protocol.Inventory, mode, namespace string) []protocol.Container {
	var found []protocol.Container
	for _, container := range inventory.Containers {
		if containerNamespace(container, mode) == namespace {
			found = append(found, container)
		}
	}
	return found
}

// containerNamespace returns the namespace a container belongs to.
func containerNamespace(container protocol.Container, mode string) string {
	if mode == yamlspec.ModeSwarm {
		return container.Labels[stackNamespaceLabel]
	}
	return container.Labels[composeProjectLabel]
}

// containerService returns the name of the service a container belongs to.
//
// A container of a Swarm service carries the name of the stack and of the
// service, so the name of the service is what follows the namespace.
func containerService(container protocol.Container, mode, namespace string) string {
	if mode == yamlspec.ModeSwarm {
		return strings.TrimPrefix(container.Labels[swarmServiceLabel], namespace+"_")
	}
	return container.Labels[composeServiceLabel]
}

// isPlatformContainer reports whether a container belongs to the platform
// itself.
//
// The container is told by the repository of its image, so the image of the
// platform is recognized with a registry, with a tag and with a digest. The
// comparison is exact: an image of the user that starts with the name of
// the platform is no container of the platform.
func (s *Service) isPlatformContainer(container protocol.Container) bool {
	return s.platformImages[imageRepository(container.Image)]
}

// imageRepository returns the repository of an image reference.
//
// A reference may name a registry, a tag and a digest. The registry is
// recognized by the rule of Docker: the first part of the path names a
// registry when it holds a dot or a colon or when it is "localhost". The
// result is the name of the repository alone, so "hmoob-dock-control",
// "hmoob-dock-control:local" and
// "registry.example.com/hmoob-dock-control@sha256:..." name the same
// repository.
func imageRepository(image string) string {
	reference := strings.TrimSpace(image)
	if reference == "" {
		return ""
	}
	// The digest comes last.
	if at := strings.Index(reference, "@"); at >= 0 {
		reference = reference[:at]
	}
	parts := strings.Split(reference, "/")
	// The tag belongs to the last part of the path.
	last := len(parts) - 1
	if colon := strings.LastIndex(parts[last], ":"); colon >= 0 {
		parts[last] = parts[last][:colon]
	}
	if len(parts) > 1 && registryHost(parts[0]) {
		host := parts[0]
		parts = parts[1:]
		// The "library" namespace only holds the default namespace of the
		// Docker Hub, so "docker.io/library/nginx" names the same
		// repository as "nginx". Another registry uses "library" as an
		// ordinary namespace.
		if len(parts) > 1 && parts[0] == "library" && dockerHubHost(host) {
			parts = parts[1:]
		}
	}
	return strings.Join(parts, "/")
}

// dockerHubHost reports whether a registry host names the Docker Hub.
func dockerHubHost(host string) bool {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	}
	return false
}

// registryHost reports whether the first part of the path of an image
// reference names a registry.
func registryHost(part string) bool {
	return part == "localhost" || strings.ContainsAny(part, ".:")
}

// validNamespace reports whether a namespace may be used as the name of a
// Compose project and of a Swarm stack.
func validNamespace(namespace string) bool {
	if namespace == "" || len(namespace) > maxNamespaceLength {
		return false
	}
	for index, char := range namespace {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		case char == '-' || char == '_':
			// A name may not start with a separator.
			if index == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// decodeServices reads a list of service names of the database.
func decodeServices(value string) ([]string, error) {
	var services []string
	if value == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(value), &services); err != nil {
		return nil, fmt.Errorf("decode the services: %w", err)
	}
	return services, nil
}

// encodeServices writes a list of service names for the database.
func encodeServices(services []string) (string, error) {
	if services == nil {
		services = []string{}
	}
	encoded, err := json.Marshal(services)
	if err != nil {
		return "", fmt.Errorf("encode the services: %w", err)
	}
	return string(encoded), nil
}

// versionAAD returns the additional data of the encryption of a version.
//
// The additional data binds the ciphertext to the application and to the
// version, so a ciphertext cannot be moved to another application or to
// another version.
func versionAAD(appID, versionID string) []byte {
	return []byte("app/" + appID + "/version/" + versionID)
}

// randomID returns a random identifier.
func randomID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("deploy: generate an identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// digestOf returns the hex encoded SHA-256 digest of the given value.
func digestOf(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// uniqueViolation reports whether the database refused a write because a
// unique constraint was violated.
//
// The database names the constraint in the message of the refusal, so a
// submission that lost a race can be told apart from a submission that
// failed for another reason.
func uniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// sortServices returns the names of the services in a stable order.
func sortServices(services []string) []string {
	sorted := make([]string, len(services))
	copy(sorted, services)
	sort.Strings(sorted)
	return sorted
}
