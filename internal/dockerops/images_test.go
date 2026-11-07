package dockerops

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	networkapi "github.com/docker/docker/api/types/network"
	swarmapi "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
)

// TestImagesOfProjectsTheImagesOfTheEngine proves that the images of the
// Docker SDK keep their references, their size and their build time.
func TestImagesOfProjectsTheImagesOfTheEngine(t *testing.T) {
	built := time.Unix(1800000000, 0)
	projected := imagesOf([]image.Summary{
		{
			ID:       "sha256:aaa111",
			RepoTags: []string{"nginx:alpine", "registry.example.com/nginx:alpine"},
			Size:     73400320,
			Created:  built.Unix(),
			Labels:   map[string]string{"maintainer": "nginx"},
		},
		{
			// An image that no reference names carries no tags.
			ID:       "sha256:bbb222",
			RepoTags: nil,
			Size:     1024,
			Created:  0,
		},
	})

	if len(projected) != 2 {
		t.Fatalf("images = %d, want 2", len(projected))
	}
	first := projected[0]
	if first.ID != "sha256:aaa111" {
		t.Fatalf("ID = %q, want the ID of the image", first.ID)
	}
	if len(first.Tags) != 2 || first.Tags[0] != "nginx:alpine" {
		t.Fatalf("tags = %v, want the references of the image", first.Tags)
	}
	if first.Size != 73400320 {
		t.Fatalf("size = %d, want the size of the image", first.Size)
	}
	if first.Created != built.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("created = %q, want the build time of the image", first.Created)
	}
	if first.Labels["maintainer"] != "nginx" {
		t.Fatalf("labels = %v, want the labels of the image", first.Labels)
	}

	second := projected[1]
	if len(second.Tags) != 0 {
		t.Fatalf("tags = %v, want none for an image without a reference", second.Tags)
	}
	if second.Created != "" {
		t.Fatalf("created = %q, want empty for an image without a build time", second.Created)
	}
}

// TestNetworkOfProjectsTheAddressManagement proves that the networks keep
// their labels and the subnets and gateways of both address families.
func TestNetworkOfProjectsTheAddressManagement(t *testing.T) {
	created := time.Unix(1800000000, 0)
	projected := networkOf(networkapi.Summary{
		ID:         "net-1",
		Name:       "demo-compose_default",
		Driver:     "bridge",
		Scope:      "local",
		Internal:   true,
		Attachable: true,
		Created:    created,
		Containers: map[string]network.EndpointResource{
			"ep-1": {Name: "web"},
			"ep-2": {Name: "db"},
		},
		Labels: map[string]string{"com.docker.compose.project": "demo-compose"},
		IPAM: networkapi.IPAM{
			Driver: "default",
			Config: []networkapi.IPAMConfig{
				{Subnet: "172.18.0.0/16", Gateway: "172.18.0.1"},
				{Subnet: "fd00:1::/64", Gateway: "fd00:1::1"},
			},
		},
	})

	if projected.Name != "demo-compose_default" {
		t.Fatalf("name = %q, want the name of the network", projected.Name)
	}
	if projected.Labels["com.docker.compose.project"] != "demo-compose" {
		t.Fatalf("labels = %v, want the labels of the network", projected.Labels)
	}
	if projected.IPAMDriver != "default" {
		t.Fatalf("IPAM driver = %q, want the driver of the address management", projected.IPAMDriver)
	}
	if projected.IPAMSubnet != "172.18.0.0/16" || projected.IPAMGateway != "172.18.0.1" {
		t.Fatalf("IPv4 = %s via %s, want the subnet and the gateway of the network", projected.IPAMSubnet, projected.IPAMGateway)
	}
	if projected.IPAMIPv6Subnet != "fd00:1::/64" || projected.IPAMIPv6Gateway != "fd00:1::1" {
		t.Fatalf("IPv6 = %s via %s, want the subnet and the gateway of the network", projected.IPAMIPv6Subnet, projected.IPAMIPv6Gateway)
	}

	// A network that Docker manages by itself carries no subnets.
	plain := networkOf(networkapi.Summary{ID: "net-2", Name: "host", Driver: "host"})
	if plain.IPAMSubnet != "" || plain.IPAMIPv6Subnet != "" {
		t.Fatalf("subnets = %s and %s, want none for a network without an address management", plain.IPAMSubnet, plain.IPAMIPv6Subnet)
	}
}

// TestVolumesOfProjectsTheVolumesOfTheEngine proves that the named volumes
// keep their driver, their mountpoint and their creation time.
func TestVolumesOfProjectsTheVolumesOfTheEngine(t *testing.T) {
	created := time.Unix(1800000000, 0)
	projected := volumesOf([]*volume.Volume{
		{
			Name:       "demo-compose_web-data",
			Driver:     "local",
			Mountpoint: "/var/lib/docker/volumes/demo-compose_web-data/_data",
			Scope:      "local",
			CreatedAt:  created.Format(time.RFC3339Nano),
			Labels:     map[string]string{"com.docker.compose.project": "demo-compose"},
		},
		{
			Name:       "dangling",
			Driver:     "local",
			Mountpoint: "/var/lib/docker/volumes/dangling/_data",
			Scope:      "local",
			CreatedAt:  "not-a-time",
		},
		nil,
	})

	if len(projected) != 2 {
		t.Fatalf("volumes = %d, want 2 without the empty entry", len(projected))
	}
	first := projected[0]
	if first.Name != "demo-compose_web-data" || first.Driver != "local" {
		t.Fatalf("volume = %+v, want the projected volume", first)
	}
	if first.Mountpoint != "/var/lib/docker/volumes/demo-compose_web-data/_data" {
		t.Fatalf("mountpoint = %q, want the path of the volume", first.Mountpoint)
	}
	if first.Created != created.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("created = %q, want the creation time of the volume", first.Created)
	}
	if first.Labels["com.docker.compose.project"] != "demo-compose" {
		t.Fatalf("labels = %v, want the labels of the volume", first.Labels)
	}

	second := projected[1]
	if second.Created != "not-a-time" {
		t.Fatalf("created = %q, want the text the engine sent", second.Created)
	}
	if second.Labels != nil {
		t.Fatalf("labels = %v, want none for a volume without labels", second.Labels)
	}
}

// TestSwarmNodesOfProjectsTheClusterFacts proves that the nodes of a
// cluster keep their role, their reachability and the resources they offer.
func TestSwarmNodesOfProjectsTheClusterFacts(t *testing.T) {
	managerRole := swarmapi.NodeRole("manager")
	workerRole := swarmapi.NodeRole("worker")
	active := swarmapi.NodeAvailability("active")
	ready := swarmapi.NodeState("ready")
	projected := swarmNodesOf([]swarmapi.Node{
		{
			ID:     "node-manager",
			Spec:   swarmapi.NodeSpec{Role: managerRole, Availability: active},
			Status: swarmapi.NodeStatus{State: ready, Addr: "172.16.36.48"},
			Description: swarmapi.NodeDescription{
				Hostname: "manager-1",
				Resources: swarmapi.Resources{
					NanoCPUs:    28000000000,
					MemoryBytes: 26948403200,
				},
				Engine: swarmapi.EngineDescription{EngineVersion: "20.10.21"},
			},
		},
		{
			ID:     "node-worker",
			Spec:   swarmapi.NodeSpec{Role: workerRole, Availability: active},
			Status: swarmapi.NodeStatus{State: ready, Addr: "172.16.36.49"},
			Description: swarmapi.NodeDescription{
				Hostname: "worker-1",
				Resources: swarmapi.Resources{
					NanoCPUs:    28000000000,
					MemoryBytes: 26948403200,
				},
				Engine: swarmapi.EngineDescription{EngineVersion: "20.10.21"},
			},
		},
	})

	if len(projected) != 2 {
		t.Fatalf("nodes = %d, want 2", len(projected))
	}
	manager := projected[0]
	if manager.Hostname != "manager-1" || manager.Role != "manager" {
		t.Fatalf("node = %+v, want the manager of the cluster", manager)
	}
	if manager.Addr != "172.16.36.48" {
		t.Fatalf("addr = %q, want the address of the node", manager.Addr)
	}
	if manager.CPUs != 28 {
		t.Fatalf("cpus = %g, want 28 cores", manager.CPUs)
	}
	if manager.MemoryBytes != 26948403200 {
		t.Fatalf("memory = %d, want the memory of the node", manager.MemoryBytes)
	}
	if manager.EngineVersion != "20.10.21" {
		t.Fatalf("engine = %q, want the version of the engine", manager.EngineVersion)
	}
	if manager.Status != "ready" || manager.Availability != "active" {
		t.Fatalf("state = %s/%s, want ready and active", manager.Status, manager.Availability)
	}
	worker := projected[1]
	if worker.Role != "worker" || worker.Hostname != "worker-1" {
		t.Fatalf("node = %+v, want the worker of the cluster", worker)
	}
}
