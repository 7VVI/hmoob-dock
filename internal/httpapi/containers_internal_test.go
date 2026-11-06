package httpapi

import (
	"testing"

	"hmoob-dock/internal/protocol"
)

// TestComposeServicesGroupsTheContainers proves that the services of a
// standalone node are derived from the Compose labels of its containers,
// with the ports of the containers merged into the service.
func TestComposeServicesGroupsTheContainers(t *testing.T) {
	containers := []protocol.Container{
		{
			ID:      "c1",
			Name:    "demo-web-1",
			Image:   "nginx:1.27",
			State:   "running",
			Service: "web",
			Labels:  map[string]string{composeProjectLabel: "demo"},
			Ports:   []protocol.PortMapping{{PrivatePort: 80, PublicPort: 8080, Type: "tcp"}},
		},
		{
			ID:      "c2",
			Name:    "demo-web-2",
			Image:   "nginx:1.27",
			State:   "exited",
			Service: "web",
			Labels:  map[string]string{composeProjectLabel: "demo"},
			Ports:   []protocol.PortMapping{{PrivatePort: 80, PublicPort: 8080, Type: "tcp"}},
		},
		{
			ID:      "c3",
			Name:    "shop-api-1",
			Image:   "demo/api:1.4.2",
			State:   "running",
			Service: "api",
			Labels:  map[string]string{composeProjectLabel: "shop"},
		},
		{
			ID:    "c4",
			Name:  "watchtower",
			State: "running",
		},
	}

	services := composeServices(containers)
	if len(services) != 2 {
		t.Fatalf("services = %d, want 2", len(services))
	}

	byID := map[string]serviceSummary{}
	for _, service := range services {
		byID[service.ID] = service
	}

	web, ok := byID["demo/web"]
	if !ok {
		t.Fatalf("the demo/web service is missing: %+v", services)
	}
	if web.Desired != 2 || web.Running != 1 || web.Containers != 2 {
		t.Fatalf("demo/web counts = %d/%d containers=%d, want 2/1 containers=2",
			web.Running, web.Desired, web.Containers)
	}
	if web.Image != "nginx:1.27" {
		t.Fatalf("demo/web image = %q", web.Image)
	}
	if len(web.Ports) != 1 || web.Ports[0].PrivatePort != 80 || web.Ports[0].PublicPort != 8080 {
		t.Fatalf("demo/web ports = %+v", web.Ports)
	}

	api, ok := byID["shop/api"]
	if !ok {
		t.Fatalf("the shop/api service is missing: %+v", services)
	}
	if api.Desired != 1 || api.Running != 1 {
		t.Fatalf("shop/api counts = %d/%d, want 1/1", api.Running, api.Desired)
	}
}
