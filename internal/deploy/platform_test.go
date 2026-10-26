package deploy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hmoob-dock/internal/config"
	"hmoob-dock/internal/deploy"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// inventoryWithImage holds the state of a target with a single container
// that runs the given image in the namespace demo.
func inventoryWithImage(image string) fixedInventory {
	return fixedInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
		Containers: []protocol.Container{{
			ID:    "container-platform",
			Name:  "demo-platform-1",
			Image: image,
			Labels: map[string]string{
				"com.docker.compose.project": "demo",
				"com.docker.compose.service": "platform",
			},
		}},
	}}
}

func TestTheContainersOfThePlatformAreRecognized(t *testing.T) {
	images := []string{
		"hmoob-dock-control:local",
		"hmoob-dock-agent:local",
		"registry.example.com/hmoob-dock-control:1.0",
		"registry.example.com/hmoob-dock-agent:1.0",
		"registry.example.com/hmoob-dock-control@sha256:0123456789abcdef0123456789abcdef",
		"localhost:5000/hmoob-dock-control:1.0",
		"docker.io/library/hmoob-dock-control:1.0",
		"index.docker.io/library/hmoob-dock-agent:1.0",
	}
	for _, image := range images {
		env := newHarness(t, inventoryWithImage(image))

		// The confirmation of the administrator does not help: the platform
		// never takes over its own containers.
		_, err := env.service.CreateApp(context.Background(), model.App{
			Name:      "demo",
			Mode:      "compose",
			TargetID:  env.nodeID,
			Namespace: "demo",
		}, true)
		if !errors.Is(err, deploy.ErrPlatformResource) {
			t.Fatalf("the image %q was not taken for a container of the platform: %v", image, err)
		}
	}
}

func TestTheImageOfTheUserIsNoContainerOfThePlatform(t *testing.T) {
	images := []string{
		"myapp:latest",
		"hmoob-dock-foo:local",
		"registry.example.com/hmoob-dock-tools:1.0",
		"registry.example.com/team/myapp:1.0",
	}
	for _, image := range images {
		env := newHarness(t, inventoryWithImage(image))

		// The image of the user is not a container of the platform, so the
		// application may take it over.
		if _, err := env.service.CreateApp(context.Background(), model.App{
			Name:      "demo",
			Mode:      "compose",
			TargetID:  env.nodeID,
			Namespace: "demo",
		}, true); err != nil {
			t.Fatalf("the image %q was taken for a container of the platform: %v", image, err)
		}
	}
}

func TestTheListOfTheImagesOfThePlatformIsConfigurable(t *testing.T) {
	t.Setenv(config.PlatformImagesEnv, "my-control,my-agent")

	env := newHarness(t, inventoryWithImage("my-control:1.0"))
	_, err := env.service.CreateApp(context.Background(), model.App{
		Name:      "demo",
		Mode:      "compose",
		TargetID:  env.nodeID,
		Namespace: "demo",
	}, true)
	if !errors.Is(err, deploy.ErrPlatformResource) {
		t.Fatalf("the image of the configured list was not taken for a container of the platform: %v", err)
	}

	// The list of the environment adds to the images the platform ships
	// with, so a renamed image never loses the protection of the default
	// one: a name left out by mistake cannot turn the container of the
	// platform into a managed resource.
	other := newHarness(t, inventoryWithImage("hmoob-dock-control:local"))
	if _, err := other.service.CreateApp(context.Background(), model.App{
		Name:      "demo",
		Mode:      "compose",
		TargetID:  other.nodeID,
		Namespace: "demo",
	}, true); !errors.Is(err, deploy.ErrPlatformResource) {
		t.Fatalf("the image of the default list was not taken for a container of the platform: %v", err)
	}
}

func TestTheContainerOfThePlatformInARegistryIsNeverRemoved(t *testing.T) {
	source := &switchableInventory{value: protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
	}}
	env := newHarnessWith(t, source)
	app := env.createApp("demo")

	// The agent of the node joins the namespace of the application with an
	// image of the platform.
	source.report(protocol.Inventory{
		Identity:    protocol.NodeIdentity{EngineID: "engine-test"},
		CollectedAt: time.Now(),
		Containers: []protocol.Container{{
			ID:    "container-platform",
			Name:  "demo-platform-1",
			Image: "registry.example.com/hmoob-dock-control:1.0",
			Labels: map[string]string{
				"com.docker.compose.project": "demo",
				"com.docker.compose.service": "platform",
			},
		}},
	})

	preview, err := env.service.Preview(context.Background(), app.ID, "", composeConfig("nginx:alpine"))
	if err != nil {
		t.Fatalf("preview the deployment: %v", err)
	}
	if len(preview.RemovedServices) != 0 {
		t.Fatalf("removed services = %v, want none", preview.RemovedServices)
	}
}
