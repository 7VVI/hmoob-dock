package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/store"
	"hmoob-dock/internal/yamlspec"
)

// RemovalConfirmation confirms the removal of the services of a preview.
//
// The confirmation is bound to the configuration, to the exact set of the
// services and to the state of the target the preview was computed from.
// A confirmation of another preview therefore never fits: the digest of the
// configuration tells the previews apart, even when they remove the same
// services.
type RemovalConfirmation struct {
	// Digest is the digest of the configuration of the preview.
	Digest string `json:"digest"`
	// Services are the services the administrator confirmed the removal of.
	Services []string `json:"services"`
	// InventoryRevision is the revision of the target the preview was
	// computed from.
	InventoryRevision string `json:"inventory_revision"`
}

// SubmitRequest is a request to deploy a configuration.
type SubmitRequest struct {
	// AppID is the application the deployment belongs to.
	AppID string `json:"app_id"`
	// BaseVersionID is the version the administrator edited.
	BaseVersionID string `json:"base_version_id"`
	// IdempotencyKey identifies the request. A repeated request with the
	// same key and the same content answers with the task of the first
	// request.
	IdempotencyKey string `json:"idempotency_key"`
	// PreviewDigest is the digest of the preview the administrator saw.
	// The request has to name it: a deployment without a preview is
	// refused, because nobody looked at the impact of the configuration.
	PreviewDigest string `json:"preview_digest"`
	// InventoryRevision is the revision of the target the preview was
	// computed from.
	InventoryRevision string `json:"inventory_revision"`
	// Config is the configuration to deploy.
	Config model.Config `json:"config"`
	// ConfirmedRemoval confirms the removal of the services of the preview.
	ConfirmedRemoval RemovalConfirmation `json:"confirmed_removal"`
	// AcceptWarnings confirms the risky configuration.
	AcceptWarnings bool `json:"accept_warnings"`
}

// Submit writes the version and the task of a deployment.
//
// The version and the task are written in one transaction and the
// application points at the new version afterwards, so a task never refers
// to a version that does not exist.
//
// The request has to name the preview the administrator saw: a deployment
// without a preview is refused. The version the administrator edited is
// compared inside the transaction, so a page that is out of date changes
// nothing.
//
// The idempotency key of the request is unique per application. A request
// with a known key and the same content answers with the task of the first
// request, a request with a known key and another content is a conflict.
// The check of the key comes before the check of the busy application, so
// a repeated click of the administrator gets the task of the first click
// and not a refusal.
func (s *Service) Submit(ctx context.Context, req SubmitRequest) (model.Task, error) {
	if strings.TrimSpace(req.AppID) == "" {
		return model.Task{}, fmt.Errorf("%w: the request names no application", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return model.Task{}, fmt.Errorf("%w: the request names no idempotency key", ErrInvalidRequest)
	}

	app, err := s.app(ctx, req.AppID)
	if err != nil {
		return model.Task{}, err
	}
	digest := requestDigest(req)

	// The idempotency key comes first: a repeated request has to answer
	// with the task of the first one, even while the application is busy.
	existing, err := s.taskByIdempotencyKey(ctx, app.ID, req.IdempotencyKey)
	if err != nil {
		return model.Task{}, err
	}
	if existing != nil {
		return answeredTask(existing, req, digest)
	}

	// An application has at most one task that is not finished.
	open, err := s.openTask(ctx, app.ID)
	if err != nil {
		return model.Task{}, err
	}
	if open != nil {
		// The open task may be the task of the request that is repeated
		// right now: it was written after the read above.
		if open.idempotencyKey == req.IdempotencyKey {
			return answeredTask(open, req, digest)
		}
		return model.Task{}, fmt.Errorf("%w: the task %s", ErrBusy, open.task.ID)
	}

	// The administrator looked at the impact of the configuration before it
	// was deployed, so the request has to name that preview.
	if strings.TrimSpace(req.PreviewDigest) == "" {
		return model.Task{}, fmt.Errorf("%w: the request names no preview", ErrConflict)
	}

	// The version the administrator edited is not compared here: the
	// comparison happens inside the transaction that writes the deployment,
	// so a deployment that lost the race against another one changes
	// nothing at all.
	checked, err := yamlspec.Check(app, req.Config)
	if err != nil {
		return model.Task{}, err
	}
	// The configuration of the submission is the configuration of the
	// preview the administrator saw.
	if req.PreviewDigest != checked.Digest {
		return model.Task{}, fmt.Errorf("%w: the configuration differs from the preview", ErrConflict)
	}

	inventory, err := s.readInventory(ctx, app)
	if err != nil {
		return model.Task{}, err
	}
	running := s.runningServices(inventory, app.Mode, app.Namespace)
	removed := removedServices(running, checked.Services)
	revision, err := inventoryRevision(inventory, app.Mode, app.Namespace)
	if err != nil {
		return model.Task{}, err
	}

	// The state of the target has to be the state of the preview, so a
	// confirmation of an older state cannot be reused.
	if req.InventoryRevision != "" && req.InventoryRevision != revision {
		return model.Task{}, fmt.Errorf("%w: the state of the target changed since the preview", ErrConflict)
	}
	// The confirmation of a removal is bound to the configuration, to the
	// exact set of the services and to the state of the target, so a
	// confirmation of another preview cannot be reused.
	if len(removed) > 0 {
		if err := checkRemoval(req.ConfirmedRemoval, checked.Digest, removed, revision); err != nil {
			return model.Task{}, err
		}
	}

	// The risks of the configuration need an explicit confirmation, the
	// notice of a variable that was not provided does not. The notices of
	// the platform about the target belong to the risks as well: a Swarm
	// deployment names the nodes whose paths of the host it could not
	// check.
	warnings := make([]string, 0, len(checked.Warnings))
	warnings = append(warnings, checked.Warnings...)
	warnings = append(warnings, swarmWarnings(inventory, app, checked)...)
	if len(RiskWarnings(warnings)) > 0 && !req.AcceptWarnings {
		return model.Task{}, fmt.Errorf("%w: the configuration is risky", ErrConfirmationRequired)
	}

	return s.store(ctx, app, req, checked, removed, digest)
}

// checkRemoval makes sure that the administrator confirmed the removal of
// the given services.
//
// The confirmation has to name the configuration of the deployment and the
// state of the target the preview was computed from. A confirmation of
// another configuration is a conflict: the administrator confirmed another
// impact, so the request has to be previewed again.
func checkRemoval(confirmation RemovalConfirmation, digest string, removed []string, revision string) error {
	if strings.TrimSpace(confirmation.Digest) == "" ||
		strings.TrimSpace(confirmation.InventoryRevision) == "" ||
		len(confirmation.Services) == 0 {
		return fmt.Errorf("%w: the removal of %s is not confirmed", ErrConfirmationRequired, strings.Join(removed, ", "))
	}
	if confirmation.Digest != digest {
		return fmt.Errorf("%w: the confirmation belongs to another configuration", ErrConflict)
	}
	if confirmation.InventoryRevision != revision {
		return fmt.Errorf("%w: the state of the target changed since the preview", ErrConflict)
	}
	if !sameServices(confirmation.Services, removed) {
		return fmt.Errorf("%w: the removal of %s is not confirmed", ErrConfirmationRequired, strings.Join(removed, ", "))
	}
	return nil
}

// store writes the version and the task of a deployment in one transaction.
//
// The transaction holds the version, the task and the reference of the
// application together: a task that refers to a version that does not
// exist can never come to life.
//
// The application points at the new version only if it still points at the
// version the administrator edited. The comparison happens inside the
// transaction, so a deployment that lost the race against another one
// changes nothing at all.
func (s *Service) store(
	ctx context.Context,
	app model.App,
	req SubmitRequest,
	checked model.CheckedConfig,
	removed []string,
	digest string,
) (model.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Task{}, fmt.Errorf("deploy: start the submission: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = tx.Rollback()
		}
	}()

	versionID, err := randomID()
	if err != nil {
		return model.Task{}, err
	}
	if err := s.insertVersion(ctx, tx, app, versionID, checked, req.Config); err != nil {
		return model.Task{}, err
	}

	taskID, err := randomID()
	if err != nil {
		return model.Task{}, err
	}
	task := model.Task{
		ID:        taskID,
		AppID:     app.ID,
		VersionID: versionID,
		State:     model.Queued,
		Digest:    checked.Digest,
	}
	if err := s.insertTask(ctx, tx, task, req, removed, digest); err != nil {
		if !uniqueViolation(err) {
			return model.Task{}, err
		}
		// Another submission won the race. The transaction has to be
		// closed before the database is read again: the write pool of the
		// control端 holds a single connection.
		_ = tx.Rollback()
		closed = true
		return s.resolveRace(ctx, app.ID, req, digest)
	}

	// The application points at the new version only while it points at the
	// version the administrator edited. A deployment whose base version is
	// out of date changes nothing: the version and the task of the
	// transaction disappear with it.
	result, err := tx.ExecContext(ctx,
		`UPDATE apps SET latest_version_id = ? WHERE id = ? AND latest_version_id = ?`,
		versionID, app.ID, req.BaseVersionID)
	if err != nil {
		return model.Task{}, fmt.Errorf("deploy: point the application at the new version: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return model.Task{}, fmt.Errorf("deploy: point the application at the new version: %w", err)
	}
	if affected == 0 {
		return model.Task{}, fmt.Errorf(
			"%w: the base version %q is not the latest version of the application",
			ErrConflict, req.BaseVersionID,
		)
	}
	if err := tx.Commit(); err != nil {
		return model.Task{}, fmt.Errorf("deploy: commit the submission: %w", err)
	}
	closed = true
	return task, nil
}

// resolveRace answers a submission that lost the race with another one.
//
// The other submission either wrote a task with the same idempotency key
// or it holds the only open task of the application.
func (s *Service) resolveRace(ctx context.Context, appID string, req SubmitRequest, digest string) (model.Task, error) {
	existing, err := s.taskByIdempotencyKey(ctx, appID, req.IdempotencyKey)
	if err != nil {
		return model.Task{}, err
	}
	if existing != nil {
		return answeredTask(existing, req, digest)
	}
	return model.Task{}, fmt.Errorf("%w: the application has a task that is not finished", ErrBusy)
}

// answeredTask returns the task of a request that was answered already.
//
// A request with the same idempotency key and the same content gets the
// task of the first request, a request with the same key and another
// content is a conflict.
func answeredTask(record *taskRecord, req SubmitRequest, digest string) (model.Task, error) {
	if record.requestDigest != digest {
		return model.Task{}, fmt.Errorf("%w: the key %q", ErrIdempotencyConflict, req.IdempotencyKey)
	}
	return record.task, nil
}

// insertVersion stores the encrypted configuration of a version.
func (s *Service) insertVersion(
	ctx context.Context,
	tx *sql.Tx,
	app model.App,
	versionID string,
	checked model.CheckedConfig,
	cfg model.Config,
) error {
	aad := versionAAD(app.ID, versionID)
	document, err := s.box.Seal([]byte(cfg.YAML), aad)
	if err != nil {
		return fmt.Errorf("deploy: encrypt the configuration: %w", err)
	}
	variables, err := json.Marshal(cfg.Variables)
	if err != nil {
		return fmt.Errorf("deploy: encode the variables: %w", err)
	}
	sealed, err := s.box.Seal(variables, aad)
	if err != nil {
		return fmt.Errorf("deploy: encrypt the variables: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO versions (id, app_id, digest, yaml_ciphertext, variables_ciphertext, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		versionID, app.ID, checked.Digest, document, sealed,
		store.TimeToMillis(s.now())); err != nil {
		return fmt.Errorf("deploy: store the version: %w", err)
	}
	return nil
}

// insertTask stores the task of a deployment.
func (s *Service) insertTask(
	ctx context.Context,
	tx *sql.Tx,
	task model.Task,
	req SubmitRequest,
	removed []string,
	digest string,
) error {
	services, err := encodeServices(removed)
	if err != nil {
		return fmt.Errorf("deploy: encode the removed services: %w", err)
	}
	now := store.TimeToMillis(s.now())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tasks (id, app_id, version_id, agent_id, state, last_seq, digest,
		                    idempotency_key, request_digest, base_version_id, preview_digest,
		                    inventory_revision, removed_services, created_at, updated_at)
		 VALUES (?, ?, ?, '', ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.AppID, task.VersionID, string(model.Queued), task.Digest,
		req.IdempotencyKey, digest, req.BaseVersionID, req.PreviewDigest,
		req.InventoryRevision, services, now, now); err != nil {
		return fmt.Errorf("deploy: store the task: %w", err)
	}
	return nil
}

// taskByIdempotencyKey returns the task of the given idempotency key.
//
// It returns nil when no task of the application uses the key.
func (s *Service) taskByIdempotencyKey(ctx context.Context, appID, key string) (*taskRecord, error) {
	var record taskRecord
	var removed string
	var lastSeq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, app_id, version_id, agent_id, state, last_seq, digest, idempotency_key, request_digest, removed_services
		 FROM tasks WHERE app_id = ? AND idempotency_key = ?`, appID, key).
		Scan(&record.task.ID, &record.task.AppID, &record.task.VersionID, &record.task.AgentID,
			&record.task.State, &lastSeq, &record.task.Digest, &record.idempotencyKey,
			&record.requestDigest, &removed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
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

// openTask returns the task of the application that is not finished.
//
// It returns nil when the application has no such task.
func (s *Service) openTask(ctx context.Context, appID string) (*taskRecord, error) {
	var identifier string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM tasks
		 WHERE app_id = ? AND state IN ('queued','preflight','running','verifying','uncertain')`,
		appID).Scan(&identifier)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("deploy: read the open task: %w", err)
	}
	return s.task(ctx, identifier)
}

// requestDigest returns the digest of a submission request.
//
// The digest covers the whole request, so a repeated request with the same
// key and another content is told apart from a repeated request with the
// same content. The digest is a hash, so it never holds the value of a
// variable.
func requestDigest(req SubmitRequest) string {
	confirmation := req.ConfirmedRemoval
	confirmation.Services = sortServices(req.ConfirmedRemoval.Services)
	payload := struct {
		AppID             string              `json:"app_id"`
		BaseVersionID     string              `json:"base_version_id"`
		PreviewDigest     string              `json:"preview_digest"`
		InventoryRevision string              `json:"inventory_revision"`
		YAML              string              `json:"yaml"`
		Variables         map[string]string   `json:"variables"`
		ConfirmedRemoval  RemovalConfirmation `json:"confirmed_removal"`
		AcceptWarnings    bool                `json:"accept_warnings"`
	}{
		AppID:             req.AppID,
		BaseVersionID:     req.BaseVersionID,
		PreviewDigest:     req.PreviewDigest,
		InventoryRevision: req.InventoryRevision,
		YAML:              req.Config.YAML,
		Variables:         req.Config.Variables,
		ConfirmedRemoval:  confirmation,
		AcceptWarnings:    req.AcceptWarnings,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// The payload holds nothing that the encoder refuses, so the
		// digest of the empty payload identifies the request.
		return digestOf(req.AppID + req.IdempotencyKey)
	}
	return digestOf(string(encoded))
}

// sameServices reports whether two lists of service names hold the same
// names.
func sameServices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	sorted := sortServices(left)
	other := sortServices(right)
	for index := range sorted {
		if sorted[index] != other[index] {
			return false
		}
	}
	return true
}
