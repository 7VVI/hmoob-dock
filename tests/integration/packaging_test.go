//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The samples of the delivery and the names of the images of the delivery.
const (
	controlSample = "deploy/compose.control.yaml"
	agentSample   = "deploy/compose.agent.yaml"
	controlImage  = "hmoob-dock-control:local"
	agentImage    = "hmoob-dock-agent:local"
)

// The variables the control端 is configured with. The list is fixed by the
// design of the deployment: a sample that adds another variable changes
// the configuration of the control端.
var controlVariables = []string{
	"HMOOB_DB_PATH",
	"HMOOB_KEY_FILE",
	"HMOOB_BOOTSTRAP_FILE",
	"HMOOB_PUBLIC_URL",
	"HMOOB_TLS_CERT",
	"HMOOB_TLS_KEY",
	"HMOOB_WEB_DIR",
}

// The variables the agent is configured with.
var agentVariables = []string{
	"HMOOB_SERVER_URL",
	"HMOOB_CA_FILE",
	"HMOOB_CREDENTIAL_FILE",
	"HMOOB_ENROLLMENT_FILE",
	"HMOOB_DOCKER_SOCKET",
	"HMOOB_WORK_ROOT",
}

// composeVariable matches the variable of an interpolation of Compose.
var composeVariable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)

// composeConfig is the answer of "docker compose config --format json".
type composeConfig struct {
	Services map[string]composeService `json:"services"`
}

// composeService is one service of the answer of Compose.
type composeService struct {
	Image       string            `json:"image"`
	Environment map[string]string `json:"environment"`
	Command     []string          `json:"command"`
	Ports       []composePort     `json:"ports"`
	Privileged  bool              `json:"privileged"`
	Volumes     []composeVolume   `json:"volumes"`
}

// composePort is one published port of a service.
type composePort struct {
	Target    int    `json:"target"`
	Published string `json:"published"`
	Protocol  string `json:"protocol"`
}

// composeVolume is one mount of a service.
type composeVolume struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// TestPackagingComposeSamplesRestrictTheDeployment proves that the samples
// of the delivery keep the promises of the packaging.
//
// The control端 gets no socket of Docker and publishes only the port of
// its HTTPS service. The agent opens no port, needs no privileged mode
// and mounts only the socket of Docker and its work directory. Every
// bind mount of the agent uses the same path on the host and in the
// container, so the Docker engine of the node resolves the relative bind
// mounts of an application against the right directory.
//
// The samples have to fail without their variables, so a deployment never
// starts with a default credential.
func TestPackagingComposeSamplesRestrictTheDeployment(t *testing.T) {
	control := composeConfigOf(t, controlSample, map[string]string{
		"HMOOB_PUBLIC_URL": "https://hmoob.example.com",
	})
	agent := composeConfigOf(t, agentSample, map[string]string{
		"HMOOB_SERVER_URL":      "https://hmoob.example.com",
		"HMOOB_CA_FILE":         "/etc/hmoob-dock/ca.pem",
		"HMOOB_ENROLLMENT_FILE": "/etc/hmoob-dock/enrollment.token",
	})

	// The samples have to fail without their variables.
	for _, sample := range []string{controlSample, agentSample} {
		if _, err := readComposeConfig(t, sample, nil); err == nil {
			t.Fatalf("the sample %s was accepted without its variables", sample)
		}
	}

	controlService := serviceOf(t, control, "control")
	agentService := serviceOf(t, agent, "agent")

	// The control端 publishes the port of its HTTPS service and nothing
	// else.
	if len(controlService.Ports) != 1 {
		t.Fatalf("the control端 publishes %d ports, want 1", len(controlService.Ports))
	}
	if port := controlService.Ports[0]; port.Target != 8443 || port.Protocol != "tcp" {
		t.Fatalf("the published port of the control端 = %+v, want the HTTPS port 8443", port)
	}

	// The control端 manages no container of the host.
	for _, volume := range controlService.Volumes {
		if strings.Contains(volume.Source, "docker.sock") || strings.Contains(volume.Target, "docker.sock") {
			t.Fatalf("the control端 mounts the socket of Docker: %+v", volume)
		}
	}

	// The database has a volume of its own, the secrets are mounted read
	// only from the host.
	secretsDirectory := path.Dir(controlService.Environment["HMOOB_KEY_FILE"])
	var database, secrets bool
	for _, volume := range controlService.Volumes {
		switch {
		case volume.Type == "volume" && volume.Target == "/app/data":
			database = true
		case volume.Type == "bind" && volume.Target == secretsDirectory && volume.ReadOnly:
			secrets = true
		}
	}
	if !database {
		t.Fatal("the database of the control端 has no volume of its own")
	}
	if !secrets {
		t.Fatalf("the secrets of the control端 are not mounted read only at %s", secretsDirectory)
	}

	// The agent opens no port and needs no privileged mode.
	if len(agentService.Ports) != 0 {
		t.Fatalf("the agent publishes %d ports, want none", len(agentService.Ports))
	}
	if agentService.Privileged {
		t.Fatal("the agent runs in the privileged mode")
	}

	// The agent mounts the socket of Docker and its work directory, and
	// nothing else of the host.
	var socket, work bool
	for _, volume := range agentService.Volumes {
		if volume.Target == "/" {
			t.Fatalf("the agent mounts the root of the host: %+v", volume)
		}
		if volume.Source != volume.Target {
			t.Fatalf("the mount %+v of the agent uses different paths on the host and in the container", volume)
		}
		switch volume.Target {
		case "/var/run/docker.sock":
			socket = true
		case "/opt/hmoob-dock":
			work = true
		}
	}
	if !socket {
		t.Fatal("the agent mounts no socket of Docker")
	}
	if !work {
		t.Fatal("the agent mounts no work directory")
	}

	// The samples configure the variables of the deployment and no image
	// with a moving tag.
	for name, service := range map[string]composeService{"control": controlService, "agent": agentService} {
		if strings.HasSuffix(service.Image, ":latest") || !strings.Contains(service.Image, ":") {
			t.Fatalf("the image of the %s = %q, want a pinned tag", name, service.Image)
		}
	}
	for name, value := range map[string]string{
		"HMOOB_DB_PATH":         controlService.Environment["HMOOB_DB_PATH"],
		"HMOOB_KEY_FILE":        controlService.Environment["HMOOB_KEY_FILE"],
		"HMOOB_BOOTSTRAP_FILE":  controlService.Environment["HMOOB_BOOTSTRAP_FILE"],
		"HMOOB_PUBLIC_URL":      controlService.Environment["HMOOB_PUBLIC_URL"],
		"HMOOB_TLS_CERT":        controlService.Environment["HMOOB_TLS_CERT"],
		"HMOOB_TLS_KEY":         controlService.Environment["HMOOB_TLS_KEY"],
		"HMOOB_WEB_DIR":         controlService.Environment["HMOOB_WEB_DIR"],
		"HMOOB_SERVER_URL":      agentService.Environment["HMOOB_SERVER_URL"],
		"HMOOB_CA_FILE":         agentService.Environment["HMOOB_CA_FILE"],
		"HMOOB_CREDENTIAL_FILE": agentService.Environment["HMOOB_CREDENTIAL_FILE"],
		"HMOOB_ENROLLMENT_FILE": agentService.Environment["HMOOB_ENROLLMENT_FILE"],
		"HMOOB_DOCKER_SOCKET":   agentService.Environment["HMOOB_DOCKER_SOCKET"],
		"HMOOB_WORK_ROOT":       agentService.Environment["HMOOB_WORK_ROOT"],
	} {
		if value == "" {
			t.Fatalf("the sample does not configure %s", name)
		}
	}
}

// TestPackagingImagesHoldTheExpectedTools proves that the images of the
// delivery hold what they promise.
//
// The control端 serves the frontend and needs neither Node nor the command
// line of Docker. The agent drives the Docker engine of its node, so it
// needs the command line of Docker and the plugin of Compose v2, and it
// must not hold the code of the control端.
func TestPackagingImagesHoldTheExpectedTools(t *testing.T) {
	if imageExists(t, controlImage) {
		out := runInImage(t, controlImage, "sh", "-c",
			"command -v node; command -v docker; command -v npm; test -f /app/web/index.html && echo web-present")
		if strings.Contains(out, "node") || strings.Contains(out, "npm") {
			t.Fatalf("the control端 image holds Node: %q", out)
		}
		if strings.Contains(out, "/usr/bin/docker") || strings.Contains(out, "/usr/local/bin/docker") {
			t.Fatalf("the control端 image holds the command line of Docker: %q", out)
		}
		if !strings.Contains(out, "web-present") {
			t.Fatalf("the control端 image holds no frontend: %q", out)
		}
	} else {
		t.Logf("the image %s does not exist yet: the acceptance of the image is blocked", controlImage)
	}

	if imageExists(t, agentImage) {
		version := runInImage(t, agentImage, "sh", "-c", "docker --version")
		if !strings.Contains(version, "Docker version") {
			t.Fatalf("the agent image holds no command line of Docker: %q", version)
		}
		compose := runInImage(t, agentImage, "sh", "-c", "docker compose version")
		if !strings.Contains(compose, "Docker Compose version") {
			t.Fatalf("the agent image holds no plugin of Compose v2: %q", compose)
		}
		control := runInImage(t, agentImage, "sh", "-c", "command -v control || true")
		if strings.TrimSpace(control) != "" {
			t.Fatalf("the agent image holds the code of the control端: %q", control)
		}
	} else {
		t.Logf("the image %s does not exist yet: the acceptance of the image is blocked", agentImage)
	}
}

// TestPackagingSamplesUseOnlyTheKnownVariables proves that the samples of
// the delivery configure the processes with the variables of the design
// and with nothing else.
//
// The list of the variables is fixed: the deployment of the control端 and
// the deployment of the agent have to look the same for every operator,
// and a variable that the process does not know is a silent mistake.
func TestPackagingSamplesUseOnlyTheKnownVariables(t *testing.T) {
	for _, sample := range []struct {
		path      string
		variables []string
	}{
		{controlSample, controlVariables},
		{agentSample, agentVariables},
	} {
		known := map[string]bool{}
		for _, name := range sample.variables {
			known[name] = true
		}

		// Every variable of an interpolation of the sample has to be one
		// of the variables of the design.
		for _, match := range composeVariable.FindAllStringSubmatch(readRepositoryFile(t, sample.path), -1) {
			if !known[match[1]] {
				t.Fatalf("the sample %s uses the unknown variable %s", sample.path, match[1])
			}
		}

		// Every variable of the environment of the service has to be one
		// of the variables of the design, and no variable of the design
		// may be left out.
		config := composeConfigOf(t, sample.path, sampleVariablesOf(t, sample.path))
		service := serviceOf(t, config, firstServiceOf(t, config))
		for name := range service.Environment {
			if !known[name] {
				t.Fatalf("the sample %s configures the unknown variable %s", sample.path, name)
			}
		}
		for _, name := range sample.variables {
			if service.Environment[name] == "" {
				t.Fatalf("the sample %s does not configure %s", sample.path, name)
			}
		}
	}
}

// TestPackagingDockerfilesPinTheBaseImages proves that the images of the
// delivery are built from base images that are pinned to a digest.
//
// A tag alone moves: the same Dockerfile would build a different image
// tomorrow. A digest names one image, so a rebuild of the delivery uses
// the base image that was verified.
func TestPackagingDockerfilesPinTheBaseImages(t *testing.T) {
	for _, dockerfile := range []string{"deploy/Dockerfile.control", "deploy/Dockerfile.agent"} {
		content := readRepositoryFile(t, dockerfile)

		found := false
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "FROM ") {
				continue
			}
			found = true

			fields := strings.Fields(strings.TrimPrefix(line, "FROM "))
			for _, field := range fields {
				if !strings.Contains(field, ":") {
					continue
				}
				reference := field
				if index := strings.Index(reference, "@"); index >= 0 {
					reference = reference[:index]
				}
				if strings.HasSuffix(reference, ":latest") {
					t.Fatalf("the file %s uses the moving tag %s", dockerfile, reference)
				}
			}
			if !strings.Contains(line, "@sha256:") {
				t.Fatalf("the file %s does not pin a base image to a digest: %s", dockerfile, line)
			}
		}
		if !found {
			t.Fatalf("the file %s holds no base image", dockerfile)
		}
	}
}

// TestPackagingAgentImageAsksForItsToolsDuringTheBuild proves that the
// image of the agent asks the command line of Docker and the plugin of
// Compose for their version while it is built.
//
// An image that reaches a node without them would fail at the first
// container of an application, far away from the build that could have
// caught it.
func TestPackagingAgentImageAsksForItsToolsDuringTheBuild(t *testing.T) {
	content := readRepositoryFile(t, "deploy/Dockerfile.agent")

	for _, command := range []string{"docker --version", "docker compose version"} {
		if !strings.Contains(content, command) {
			t.Fatalf("the file deploy/Dockerfile.agent does not run %q during the build", command)
		}
	}
}

// readRepositoryFile reads a file of the repository.
func readRepositoryFile(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(repoPath(t, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}

// sampleVariablesOf returns the variables a sample needs to be read.
func sampleVariablesOf(t *testing.T, path string) map[string]string {
	t.Helper()

	switch path {
	case controlSample:
		return map[string]string{"HMOOB_PUBLIC_URL": "https://hmoob.example.com"}
	case agentSample:
		return map[string]string{
			"HMOOB_SERVER_URL":      "https://hmoob.example.com",
			"HMOOB_CA_FILE":         "/etc/hmoob-dock/ca.pem",
			"HMOOB_ENROLLMENT_FILE": "/etc/hmoob-dock/enrollment.token",
		}
	default:
		t.Fatalf("unknown sample %s", path)
		return nil
	}
}

// firstServiceOf returns the name of the first service of the answer of
// Compose.
func firstServiceOf(t *testing.T, config composeConfig) string {
	t.Helper()

	for name := range config.Services {
		return name
	}
	t.Fatal("the answer of Compose holds no service")
	return ""
}

// composeConfigOf reads the answer of Compose for the sample and fails
// when Compose does not accept it.
func composeConfigOf(t *testing.T, sample string, variables map[string]string) composeConfig {
	t.Helper()

	config, err := readComposeConfig(t, sample, variables)
	if err != nil {
		t.Fatalf("the sample %s was refused: %v", sample, err)
	}
	return config
}

// readComposeConfig runs "docker compose config" for the sample.
//
// The variables of the deployment are the only ones of the environment of
// the command, so a variable of the shell of the developer cannot hide a
// missing variable of the sample.
func readComposeConfig(t *testing.T, sample string, variables map[string]string) (composeConfig, error) {
	t.Helper()

	command := exec.Command("docker", "compose", "-f", repoPath(t, sample), "config", "--format", "json")
	command.Env = composeEnvironment(variables)

	output, err := command.CombinedOutput()
	if err != nil {
		return composeConfig{}, &composeError{output: string(output), err: err}
	}

	var config composeConfig
	if err := json.Unmarshal(output, &config); err != nil {
		return composeConfig{}, &composeError{output: string(output), err: err}
	}
	return config, nil
}

// composeError reports a refusal of Compose.
type composeError struct {
	output string
	err    error
}

// Error returns the message of the refusal.
func (e *composeError) Error() string {
	return e.err.Error() + ": " + strings.TrimSpace(e.output)
}

// composeEnvironment builds the environment of the command.
//
// The variables of the deployment are removed, so a variable of the shell
// of the developer cannot hide a missing variable of the sample. The rest
// of the environment stays, because the command line of Docker finds its
// plugins with it.
func composeEnvironment(variables map[string]string) []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "HMOOB_") {
			continue
		}
		environment = append(environment, entry)
	}
	for name, value := range variables {
		environment = append(environment, name+"="+value)
	}
	return environment
}

// serviceOf returns the service of the answer of Compose.
func serviceOf(t *testing.T, config composeConfig, name string) composeService {
	t.Helper()

	service, ok := config.Services[name]
	if !ok {
		t.Fatalf("the answer of Compose holds no service %s", name)
	}
	return service
}

// repoPath returns the path of a file of the repository.
func repoPath(t *testing.T, name string) string {
	t.Helper()

	return filepath.Join("..", "..", filepath.FromSlash(name))
}

// imageExists reports whether the image is known to the engine.
func imageExists(t *testing.T, image string) bool {
	t.Helper()

	command := exec.Command("docker", "image", "inspect", image)
	return command.Run() == nil
}

// runInImage runs a command in the image and returns its answer.
func runInImage(t *testing.T, image string, arguments ...string) string {
	t.Helper()

	command := exec.Command("docker", append([]string{"run", "--rm", "--entrypoint", arguments[0], image}, arguments[1:]...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run %s in %s: %v: %s", strings.Join(arguments, " "), image, err, strings.TrimSpace(string(output)))
	}
	return string(output)
}
