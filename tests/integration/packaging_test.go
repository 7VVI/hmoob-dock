//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
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
	controlSample     = "deploy/compose.control.yaml"
	agentSample       = "deploy/compose.agent.yaml"
	controlDockerfile = "deploy/Dockerfile.control"
	agentDockerfile   = "deploy/Dockerfile.agent"
	controlImage      = "hmoob-dock-control:local"
	agentImage        = "hmoob-dock-agent:local"
)

// controlImageProbe lists the tools that the image of the control端 must
// not hold and the frontend that it has to hold.
//
// The names of the tools are printed one per line, so the answer of the
// image can be read without guessing.
const controlImageProbe = `command -v node >/dev/null 2>&1 && echo node-present
command -v npm >/dev/null 2>&1 && echo npm-present
command -v docker >/dev/null 2>&1 && echo docker-present
command -v curl >/dev/null 2>&1 && echo curl-present
test -f /app/web/index.html && echo web-present
echo probe-done`

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
	requireDocker(t)

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

// TestPackagingControlImageHoldsTheExpectedTools proves that the image of
// the control端 holds what it promises.
//
// The control端 serves the frontend and needs neither Node nor the command
// line of Docker. Its health check asks the own listener with the command
// of the control端, so the image needs no command line of its own either.
//
// The acceptance needs an image that was built. A machine that never built
// it skips the acceptance and says that it did not verify anything.
func TestPackagingControlImageHoldsTheExpectedTools(t *testing.T) {
	requireDocker(t)
	requireImage(t, controlImage, controlDockerfile)

	out := runInImage(t, controlImage, "sh", "-c", controlImageProbe)
	for _, tool := range []string{"node", "npm", "docker", "curl"} {
		if strings.Contains(out, tool+"-present") {
			t.Fatalf("the control端 image holds %s: %q", tool, out)
		}
	}
	if !strings.Contains(out, "web-present") {
		t.Fatalf("the control端 image holds no frontend: %q", out)
	}

	// The health check of the image asks the own listener with the command
	// of the control端, so the image needs no command line of its own.
	check := imageHealthCheck(t, controlImage)
	if !strings.Contains(check, "-health-check") {
		t.Fatalf("the health check of the control端 image does not use the self check: %s", check)
	}
	if strings.Contains(check, "curl") {
		t.Fatalf("the health check of the control端 image uses curl: %s", check)
	}
}

// TestPackagingAgentImageHoldsTheExpectedTools proves that the image of the
// agent holds what it promises.
//
// The agent drives the Docker engine of its node, so it needs the command
// line of Docker and the plugin of Compose v2, and it must not hold the code
// of the control端.
//
// The acceptance needs an image that was built. A machine that never built
// it skips the acceptance and says that it did not verify anything.
func TestPackagingAgentImageHoldsTheExpectedTools(t *testing.T) {
	requireDocker(t)
	requireImage(t, agentImage, agentDockerfile)

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
}

// TestTheAcceptanceOfAMissingImageIsMarkedAsNotVerified proves that a
// skipped acceptance says that it verified nothing and names the command
// that builds the image.
//
// The acceptance of an image is the proof that the delivery works. A
// machine that never built the image has to say so instead of reporting a
// pass.
func TestTheAcceptanceOfAMissingImageIsMarkedAsNotVerified(t *testing.T) {
	message := imageMissingMessage(controlImage, controlDockerfile)

	if !strings.Contains(message, "未验收") {
		t.Fatalf("the message of a missing image does not mark the acceptance as not verified: %s", message)
	}
	command := "docker build -f " + controlDockerfile + " -t " + controlImage + " ."
	if !strings.Contains(message, command) {
		t.Fatalf("the message of a missing image does not name the build command %q: %s", command, message)
	}
	if !strings.Contains(message, controlImage) {
		t.Fatalf("the message of a missing image does not name the image: %s", message)
	}
}

// TestTheAcceptanceWithoutDockerIsMarkedAsNotVerified proves that a machine
// without the command line of Docker says that it verified nothing.
//
// The acceptance needs Docker to read the images and the samples, so it
// cannot run there. The machine has to say so instead of reporting a pass.
func TestTheAcceptanceWithoutDockerIsMarkedAsNotVerified(t *testing.T) {
	message := dockerMissingMessage()

	if !strings.Contains(message, "未验收") {
		t.Fatalf("the message of a missing Docker does not mark the acceptance as not verified: %s", message)
	}
	if !strings.Contains(message, "docker") {
		t.Fatalf("the message of a missing Docker does not name the command: %s", message)
	}
}

// TestTheAgentSampleDemandsAbsoluteBindPaths proves that the sample of the
// agent tells the operator that the paths of its bind mounts have to be
// absolute.
//
// The same variable names the file on the host and in the container, so a
// relative path would be resolved on two sides against two directories and
// the two sides would point at different files.
func TestTheAgentSampleDemandsAbsoluteBindPaths(t *testing.T) {
	content := readRepositoryFile(t, agentSample)

	header := content
	if index := strings.Index(content, "services:"); index >= 0 {
		header = content[:index]
	}
	if !strings.Contains(header, "绝对路径") {
		t.Fatalf("the sample %s does not tell the operator that the bind paths have to be absolute", agentSample)
	}
	if !strings.Contains(header, "相对路径") {
		t.Fatalf("the sample %s does not tell the operator what a relative path does", agentSample)
	}
}

// TestTheAgentSampleResolvesRelativeBindPathsToDifferentFiles proves that a
// relative path in the bind variables of the agent names two different
// files.
//
// The path on the host is resolved against the directory of the sample, the
// path in the container against the working directory of the agent. The two
// sides drift apart, which is the reason the sample asks for absolute
// paths.
func TestTheAgentSampleResolvesRelativeBindPathsToDifferentFiles(t *testing.T) {
	requireDocker(t)

	config := composeConfigOf(t, agentSample, map[string]string{
		"HMOOB_SERVER_URL":      "https://hmoob.example.com",
		"HMOOB_CA_FILE":         "./relative/ca.pem",
		"HMOOB_ENROLLMENT_FILE": "/etc/hmoob-dock/enrollment.token",
	})
	service := serviceOf(t, config, "agent")

	var found bool
	for _, volume := range service.Volumes {
		if !strings.Contains(volume.Target, "relative/ca.pem") {
			continue
		}
		found = true
		if volume.Source == volume.Target {
			t.Fatalf("the mount %+v of the agent names the same file on the host and in the container", volume)
		}
		if !filepath.IsAbs(volume.Source) {
			t.Fatalf("the source of the mount %+v of the agent is not an absolute path", volume)
		}
	}
	if !found {
		t.Fatalf("the sample %s mounts no file of a relative path", agentSample)
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
	requireDocker(t)

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

// requireDocker skips the test when the command line of Docker is missing.
//
// The acceptance of the delivery needs Docker to read the images and the
// samples, so it cannot run on a machine without it. The machine has to say
// that it verified nothing instead of reporting a pass.
func requireDocker(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip(dockerMissingMessage())
	}
}

// requireImage skips the test when the image was never built.
//
// The acceptance of an image is the proof that the delivery works, so a
// machine that never built it has to say that it verified nothing and to
// name the command that builds the image.
func requireImage(t *testing.T, image, dockerfile string) {
	t.Helper()

	if !imageExists(t, image) {
		t.Skip(imageMissingMessage(image, dockerfile))
	}
	t.Logf("已验收镜像 %s", image)
}

// imageMissingMessage describes an acceptance that was not performed,
// because the image was never built.
func imageMissingMessage(image, dockerfile string) string {
	return fmt.Sprintf(
		"未验收：镜像 %s 不存在，镜像验收没有执行，交付没有在镜像上验证。构建命令：docker build -f %s -t %s .",
		image, dockerfile, image,
	)
}

// dockerMissingMessage describes an acceptance that was not performed,
// because the command line of Docker is missing.
func dockerMissingMessage() string {
	return "未验收：命令 docker 不存在，镜像与样例的验收没有执行，交付没有在镜像上验证。请安装 Docker 后重新运行本测试"
}

// imageHealthCheck returns the health check of the image.
func imageHealthCheck(t *testing.T, image string) string {
	t.Helper()

	command := exec.Command("docker", "image", "inspect", "--format", "{{json .Config.Healthcheck}}", image)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("read the health check of %s: %v: %s", image, err, strings.TrimSpace(string(output)))
	}
	return string(output)
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
