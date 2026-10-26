package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/yamlspec"
)

// Preview is the impact of a configuration on the target of an application.
type Preview struct {
	// Digest identifies the semantics of the configuration.
	Digest string `json:"digest"`
	// RemovedServices are the services that run on the target and are not
	// part of the configuration any more.
	RemovedServices []string `json:"removed_services"`
	// Warnings are the notices of the configuration.
	Warnings []string `json:"warnings"`
	// InventoryRevision identifies the state of the target the preview was
	// computed from.
	InventoryRevision string `json:"inventory_revision"`
}

// RemovalConfirmation returns the confirmation of the removal of the
// preview.
//
// The confirmation names the configuration of the preview, the services the
// deployment removes and the state of the target the preview was computed
// from. A deployment sends it back, so the server can tell whether the
// administrator confirmed this very impact.
func (p Preview) RemovalConfirmation() RemovalConfirmation {
	return RemovalConfirmation{
		Digest:            p.Digest,
		Services:          p.RemovedServices,
		InventoryRevision: p.InventoryRevision,
	}
}

// IsRiskWarning reports whether a warning describes a risk that the
// administrator has to confirm.
//
// The kind of a warning belongs to the platform that writes it, so the
// deploy package asks the yamlspec package instead of guessing by the text
// of the warning. The notice of a variable that was not provided is no
// risk: the value becomes an empty string, and the administrator confirms
// the risks of a configuration, not the values of its variables. A warning
// the platform does not know is a risk, so a risk that is not described
// yet asks for a confirmation as well.
func IsRiskWarning(warning string) bool {
	return yamlspec.WarningKindOf(warning) == yamlspec.WarningRisk
}

// RiskWarnings returns the warnings of a configuration that the
// administrator has to confirm.
func RiskWarnings(warnings []string) []string {
	risks := []string{}
	for _, warning := range warnings {
		if IsRiskWarning(warning) {
			risks = append(risks, warning)
		}
	}
	return risks
}

// Preview computes the impact of a configuration on the target of an
// application.
//
// The preview only reads: it neither writes to the database nor changes the
// target. The digest of the configuration and the revision of the target
// are the two values that bind a submission to the preview the
// administrator saw.
func (s *Service) Preview(ctx context.Context, appID, baseVersionID string, cfg model.Config) (Preview, error) {
	app, err := s.app(ctx, appID)
	if err != nil {
		return Preview{}, err
	}
	// The page of the administrator names the version it was opened with.
	// A page that is out of date may not decide what happens on the target.
	if baseVersionID != app.LatestVersionID {
		return Preview{}, fmt.Errorf(
			"%w: the base version %q is not the latest version of the application",
			ErrConflict, baseVersionID,
		)
	}

	checked, err := yamlspec.Check(app, cfg)
	if err != nil {
		return Preview{}, err
	}
	inventory, err := s.readInventory(ctx, app)
	if err != nil {
		return Preview{}, err
	}
	running := s.runningServices(inventory, app.Mode, app.Namespace)
	revision, err := inventoryRevision(inventory, app.Mode, app.Namespace)
	if err != nil {
		return Preview{}, err
	}

	return Preview{
		Digest:            checked.Digest,
		RemovedServices:   removedServices(running, checked.Services),
		Warnings:          checked.Warnings,
		InventoryRevision: revision,
	}, nil
}

// runningServices returns the names of the services of the namespace that
// run on the target.
//
// The containers of the platform itself are left out: they never belong to
// a managed application, so they are never removed by a deployment.
func (s *Service) runningServices(inventory protocol.Inventory, mode, namespace string) []string {
	seen := make(map[string]bool)
	var names []string
	for _, container := range namespaceContainers(inventory, mode, namespace) {
		if s.isPlatformContainer(container) {
			continue
		}
		service := containerService(container, mode, namespace)
		if service == "" || seen[service] {
			continue
		}
		seen[service] = true
		names = append(names, service)
	}
	return sortServices(names)
}

// removedServices returns the services that run on the target and are not
// part of the configuration any more.
func removedServices(running, configured []string) []string {
	kept := make(map[string]bool, len(configured))
	for _, name := range configured {
		kept[name] = true
	}
	var removed []string
	for _, name := range running {
		if !kept[name] {
			removed = append(removed, name)
		}
	}
	return sortServices(removed)
}

// inventoryRevision returns a digest of the state of the namespace.
//
// The revision names the state the preview was computed from. The
// confirmation of a removal is bound to it, so a confirmation of an older
// state cannot be reused.
func inventoryRevision(inventory protocol.Inventory, mode, namespace string) (string, error) {
	containers := namespaceContainers(inventory, mode, namespace)
	type containerState struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Image   string `json:"image"`
		Service string `json:"service"`
	}
	payload := struct {
		Namespace  string           `json:"namespace"`
		Containers []containerState `json:"containers"`
	}{Namespace: namespace}

	for _, container := range containers {
		payload.Containers = append(payload.Containers, containerState{
			ID:      container.ID,
			Name:    container.Name,
			Image:   container.Image,
			Service: containerService(container, mode, namespace),
		})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("deploy: encode the state of the target: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}
