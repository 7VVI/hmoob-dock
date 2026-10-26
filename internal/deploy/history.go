package deploy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/store"
)

// VersionSummary describes a version of an application without its
// configuration.
type VersionSummary struct {
	// ID identifies the version.
	ID string `json:"id"`
	// CreatedAt is the time the version was created.
	CreatedAt time.Time `json:"created_at"`
}

// Version is a version of an application with its configuration.
type Version struct {
	// ID identifies the version.
	ID string `json:"id"`
	// CreatedAt is the time the version was created.
	CreatedAt time.Time `json:"created_at"`
	// Config is the configuration of the version.
	Config model.Config `json:"config"`
}

// ListVersions returns the versions of an application.
//
// The list holds no configuration: the history is read often and the
// configuration of a version is decrypted only when the administrator opens
// it.
func (s *Service) ListVersions(ctx context.Context, appID string) ([]VersionSummary, error) {
	if _, err := s.app(ctx, appID); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at FROM versions WHERE app_id = ? ORDER BY created_at DESC, id DESC`, appID)
	if err != nil {
		return nil, fmt.Errorf("deploy: read the versions: %w", err)
	}
	defer rows.Close()

	summaries := []VersionSummary{}
	for rows.Next() {
		var summary VersionSummary
		var createdAt int64
		if err := rows.Scan(&summary.ID, &createdAt); err != nil {
			return nil, fmt.Errorf("deploy: scan the version: %w", err)
		}
		summary.CreatedAt = store.TimeFromMillis(createdAt)
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("deploy: read the versions: %w", err)
	}
	return summaries, nil
}

// GetVersion returns the version of an application with its
// configuration.
//
// The configuration is decrypted here and nowhere else: the list of the
// versions and the answers of the API hold no plaintext.
func (s *Service) GetVersion(ctx context.Context, appID, versionID string) (Version, error) {
	if _, err := s.app(ctx, appID); err != nil {
		return Version{}, err
	}
	cfg, createdAt, err := s.loadVersion(ctx, appID, versionID)
	if err != nil {
		return Version{}, err
	}
	return Version{ID: versionID, CreatedAt: createdAt, Config: cfg}, nil
}

// loadConfig reads the decrypted configuration of a version.
func (s *Service) loadConfig(ctx context.Context, appID, versionID string) (model.Config, error) {
	cfg, _, err := s.loadVersion(ctx, appID, versionID)
	return cfg, err
}

// loadVersion reads the decrypted configuration and the creation time of a
// version.
//
// The ciphertext is bound to the application and to the version, so a
// version of another application cannot be read here.
func (s *Service) loadVersion(ctx context.Context, appID, versionID string) (model.Config, time.Time, error) {
	var document, variables []byte
	var createdAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT yaml_ciphertext, variables_ciphertext, created_at
		 FROM versions WHERE id = ? AND app_id = ?`, versionID, appID).
		Scan(&document, &variables, &createdAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return model.Config{}, time.Time{}, fmt.Errorf("%w: %s", ErrUnknownVersion, versionID)
	case err != nil:
		return model.Config{}, time.Time{}, fmt.Errorf("deploy: read the version: %w", err)
	}

	aad := versionAAD(appID, versionID)
	plain, err := s.box.Open(document, aad)
	if err != nil {
		return model.Config{}, time.Time{}, fmt.Errorf("deploy: decrypt the configuration: %w", err)
	}
	plainVariables, err := s.box.Open(variables, aad)
	if err != nil {
		return model.Config{}, time.Time{}, fmt.Errorf("deploy: decrypt the variables: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(plainVariables, &values); err != nil {
		return model.Config{}, time.Time{}, fmt.Errorf("deploy: decode the variables: %w", err)
	}

	return model.Config{YAML: string(plain), Variables: values}, store.TimeFromMillis(createdAt), nil
}
