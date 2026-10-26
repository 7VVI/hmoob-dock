package yamlspec_test

import (
	"reflect"
	"testing"

	"hmoob-dock/internal/yamlspec"
)

// TestTheHostPathsOfADocument proves that the paths of the host a document
// mounts are told apart from the named volumes.
//
// The paths belong to the machine that runs the containers, so the platform
// has to know them before it deploys.
func TestTheHostPathsOfADocument(t *testing.T) {
	document := []byte(`services:
  api:
    image: nginx:alpine
    volumes:
      - /srv/data:/data
      - data:/var/lib/data
      - type: bind
        source: /srv/logs
        target: /logs
      - type: volume
        source: cache
        target: /cache
`)
	paths := yamlspec.HostPaths(document)
	want := []string{"/srv/data", "/srv/logs"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("the paths = %q, want %q", paths, want)
	}
}

// TestTheHostPathsOfADocumentWithoutAMount proves that a document without a
// mount of the host names no path.
func TestTheHostPathsOfADocumentWithoutAMount(t *testing.T) {
	document := []byte(`services:
  api:
    image: nginx:alpine
    volumes:
      - data:/var/lib/data
`)
	if paths := yamlspec.HostPaths(document); len(paths) != 0 {
		t.Fatalf("the paths = %q, want none", paths)
	}
}

// TestTheHostPathsOfADocumentThatCannotBeRead proves that a document the
// parser refuses names no path, so the platform never invents one.
func TestTheHostPathsOfADocumentThatCannotBeRead(t *testing.T) {
	if paths := yamlspec.HostPaths([]byte("services: [")); len(paths) != 0 {
		t.Fatalf("the paths = %q, want none", paths)
	}
}
