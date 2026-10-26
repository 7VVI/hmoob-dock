package yamlspec

import (
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// HostPaths returns the paths of the host the services of a document mount.
//
// A named volume has no path, so only the sources of the binds are paths of
// the host. The paths belong to the machine that runs the containers: a
// Swarm cluster runs them on several machines, so the platform cannot claim
// that a path exists on every node of a cluster.
//
// The function reads the rendered document of a deployment, so the paths hold
// the values the variables of the application resolved to.
func HostPaths(payload []byte) []string {
	document := map[string]any{}
	if err := yaml.Unmarshal(payload, &document); err != nil {
		return nil
	}
	services, _ := document["services"].(map[string]any)

	seen := map[string]bool{}
	var paths []string
	for _, name := range sortedKeys(services) {
		service, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		for _, path := range hostPathsOf(service) {
			if seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// hostPathsOf returns the paths of the host one service mounts.
func hostPathsOf(service map[string]any) []string {
	volumes, ok := service["volumes"].([]any)
	if !ok {
		return nil
	}
	var paths []string
	for _, volume := range volumes {
		switch typed := volume.(type) {
		case string:
			// The short syntax of a volume is "source:target:mode".
			source, _, _ := strings.Cut(typed, ":")
			if isHostPath(source) {
				paths = append(paths, source)
			}
		case map[string]any:
			kind, _ := typed["type"].(string)
			if kind == "volume" {
				// The source of a named volume is a name, not a path.
				continue
			}
			source, _ := typed["source"].(string)
			if isHostPath(source) {
				paths = append(paths, source)
			}
		}
	}
	return paths
}

// isHostPath reports whether a source of a volume is a path of the host.
//
// The rule of the platform the control端 runs on is not enough: a document
// is written on one machine and read on another one, so a path of Windows is
// absolute as well.
func isHostPath(source string) bool {
	if source == "" {
		return false
	}
	if strings.HasPrefix(source, "/") {
		return true
	}
	if len(source) >= 2 && source[1] == ':' {
		// A drive of Windows, for example "C:\data".
		return true
	}
	return filepath.IsAbs(source)
}
