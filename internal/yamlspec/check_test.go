package yamlspec_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/yamlspec"
)

// composeApp is the application the Compose samples belong to.
func composeApp() model.App {
	return model.App{
		ID:        "app-1",
		Name:      "demo",
		Mode:      yamlspec.ModeCompose,
		TargetID:  "node-1",
		Namespace: "demo",
	}
}

// swarmApp is the application the Swarm samples belong to.
func swarmApp() model.App {
	return model.App{
		ID:        "app-2",
		Name:      "stack",
		Mode:      yamlspec.ModeSwarm,
		TargetID:  "cluster-1",
		Namespace: "demo",
	}
}

// checkFails runs the check and returns the error it reported.
func checkFails(t *testing.T, app model.App, cfg model.Config) *yamlspec.Error {
	t.Helper()
	_, err := yamlspec.Check(app, cfg)
	if err == nil {
		t.Fatal("the configuration was accepted")
	}
	var checkErr *yamlspec.Error
	if !errors.As(err, &checkErr) {
		t.Fatalf("the error is not a check error: %v", err)
	}
	return checkErr
}

// fieldError returns the error of the field with the given path.
func fieldError(t *testing.T, err *yamlspec.Error, path string) yamlspec.FieldError {
	t.Helper()
	for _, field := range err.Fields {
		if field.Path == path {
			return field
		}
	}
	t.Fatalf("the error does not name the field %q: %v", path, err)
	return yamlspec.FieldError{}
}

// readTestdata reads one of the sample documents of the package.
func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read the sample %s: %v", name, err)
	}
	return string(data)
}

// serviceValue returns the value of a field of a service of a rendered
// document.
func serviceValue(t *testing.T, document []byte, service, field string) any {
	t.Helper()
	var raw map[string]any
	if err := yaml.Unmarshal(document, &raw); err != nil {
		t.Fatalf("read the rendered document: %v", err)
	}
	services, ok := raw["services"].(map[string]any)
	if !ok {
		t.Fatalf("the rendered document has no services: %s", document)
	}
	definition, ok := services[service].(map[string]any)
	if !ok {
		t.Fatalf("the rendered document has no service %q: %s", service, document)
	}
	return definition[field]
}

// hasWarning reports whether one of the warnings mentions the given text.
func hasWarning(warnings []string, text string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, text) {
			return true
		}
	}
	return false
}

func TestMissingVariableNeverReadsProcessEnvironment(t *testing.T) {
	t.Setenv("PRIVATE_TAG", "host-secret")
	app := model.App{Mode: "compose", Namespace: "demo"}
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:${PRIVATE_TAG:?required}\n", Variables: map[string]string{}}
	if _, err := yamlspec.Check(app, cfg); err == nil {
		t.Fatal("process environment leaked into configuration")
	}
}

func TestCheckNeverReportsTheValueOfAVariable(t *testing.T) {
	app := composeApp()
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n    environment:\n      PASSWORD: \"${PASSWORD}\"\n    ports: \"8080:80\"\n",
		Variables: map[string]string{"PASSWORD": "p4ssw0rd"},
	}
	err := checkFails(t, app, cfg)
	if strings.Contains(err.Error(), "p4ssw0rd") {
		t.Fatalf("the value of the variable reached the error: %v", err)
	}
}

func TestCheckAcceptsTheComposeSample(t *testing.T) {
	cfg := model.Config{YAML: readTestdata(t, "compose.yaml"), Variables: map[string]string{}}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the sample was rejected: %v", err)
	}
	if len(checked.Services) != 2 {
		t.Fatalf("services=%v", checked.Services)
	}
	if len(checked.OneShotServices) != 1 || checked.OneShotServices[0] != "migrate" {
		t.Fatalf("one shot services=%v", checked.OneShotServices)
	}
	if len(checked.Warnings) != 0 {
		t.Fatalf("warnings=%v", checked.Warnings)
	}
	if checked.Digest == "" {
		t.Fatal("the configuration has no digest")
	}
	if strings.Contains(string(checked.RenderedYAML), yamlspec.LifecycleExtension) {
		t.Fatalf("the platform extension reached the Docker CLI: %s", checked.RenderedYAML)
	}
}

func TestCheckAcceptsTheSwarmSample(t *testing.T) {
	cfg := model.Config{YAML: readTestdata(t, "swarm.yaml"), Variables: map[string]string{}}
	checked, err := yamlspec.Check(swarmApp(), cfg)
	if err != nil {
		t.Fatalf("the sample was rejected: %v", err)
	}
	if len(checked.Services) != 1 || checked.Services[0] != "api" {
		t.Fatalf("services=%v", checked.Services)
	}
	if len(checked.OneShotServices) != 0 {
		t.Fatalf("one shot services=%v", checked.OneShotServices)
	}
	if len(checked.Warnings) != 0 {
		t.Fatalf("warnings=%v", checked.Warnings)
	}
	if checked.Digest == "" {
		t.Fatal("the configuration has no digest")
	}
}

func TestCheckReportsTheLineOfAnInvalidDocument(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    ports: \"8080:80\"\n"}
	err := checkFails(t, composeApp(), cfg)
	field := fieldError(t, err, "services.api.ports")
	if field.Line != 4 {
		t.Fatalf("line=%d", field.Line)
	}
}

func TestCheckRejectsADocumentThatIsNoMapping(t *testing.T) {
	cfg := model.Config{YAML: "- one\n- two\n"}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeInvalidConfig {
		t.Fatalf("code=%q", err.Code)
	}
}

func TestCheckRejectsAnAliasBomb(t *testing.T) {
	var document strings.Builder
	document.WriteString("services:\n  api:\n    image: demo:1\n")
	document.WriteString("a: &a [\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\"]\n")
	document.WriteString("b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a,*a]\n")
	document.WriteString("c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b,*b]\n")
	document.WriteString("d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c,*c]\n")
	document.WriteString("e: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d,*d]\n")
	document.WriteString("f: [*e,*e,*e,*e,*e,*e,*e,*e,*e,*e]\n")
	cfg := model.Config{YAML: document.String(), Variables: map[string]string{}}
	err := checkFails(t, composeApp(), cfg)
	if !strings.Contains(err.Error(), "别名") && !strings.Contains(err.Error(), "节点") {
		t.Fatalf("the error does not name the limit: %v", err)
	}
}

func TestCheckRejectsAnExcessivelyDeepDocument(t *testing.T) {
	var document strings.Builder
	document.WriteString("services:\n  api:\n    image: demo:1\n    environment:\n")
	for i := 0; i < 80; i++ {
		document.WriteString(strings.Repeat(" ", 6+i*2) + "key:\n")
	}
	cfg := model.Config{YAML: document.String(), Variables: map[string]string{}}
	err := checkFails(t, composeApp(), cfg)
	if !strings.Contains(err.Error(), "层级") {
		t.Fatalf("the error does not name the limit: %v", err)
	}
}

func TestCheckRejectsAnOversizedDocument(t *testing.T) {
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n    command: [\"" + strings.Repeat("x", yamlspec.MaxDocumentSize) + "\"]\n",
		Variables: map[string]string{},
	}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeConfigTooLarge {
		t.Fatalf("code=%q", err.Code)
	}
}

func TestCheckRejectsTooManyVariables(t *testing.T) {
	variables := make(map[string]string, yamlspec.MaxVariables+1)
	for i := 0; i <= yamlspec.MaxVariables; i++ {
		variables[fmt.Sprintf("VAR%d", i)] = "value"
	}
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n", Variables: variables}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeConfigTooLarge {
		t.Fatalf("code=%q", err.Code)
	}
}

func TestCheckRejectsOversizedVariables(t *testing.T) {
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n",
		Variables: map[string]string{"BIG": strings.Repeat("x", yamlspec.MaxVariablesSize+1)},
	}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeConfigTooLarge {
		t.Fatalf("code=%q", err.Code)
	}
}

func TestCheckRejectsBuild(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    build: .\n"}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeUnsupportedField {
		t.Fatalf("code=%q", err.Code)
	}
	field := fieldError(t, err, "services.api.build")
	if field.Line != 3 {
		t.Fatalf("line=%d", field.Line)
	}
}

func TestCheckRejectsInclude(t *testing.T) {
	cfg := model.Config{YAML: "include:\n  - other.yaml\nservices:\n  api:\n    image: demo:1\n"}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeUnsupportedField {
		t.Fatalf("code=%q", err.Code)
	}
	field := fieldError(t, err, "include")
	if field.Line != 1 {
		t.Fatalf("line=%d", field.Line)
	}
}

func TestCheckRejectsCrossFileExtends(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    extends:\n      service: base\n      file: other.yaml\n"}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeUnsupportedField {
		t.Fatalf("code=%q", err.Code)
	}
	field := fieldError(t, err, "services.api.extends.file")
	if field.Line != 5 {
		t.Fatalf("line=%d", field.Line)
	}
}

func TestCheckRejectsProfiles(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    profiles: [debug]\n"}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeUnsupportedField {
		t.Fatalf("code=%q", err.Code)
	}
	field := fieldError(t, err, "services.api.profiles")
	if field.Line != 4 {
		t.Fatalf("line=%d", field.Line)
	}
}

func TestCheckRejectsANameThatDoesNotMatchTheNamespace(t *testing.T) {
	cfg := model.Config{YAML: "name: other\nservices:\n  api:\n    image: demo:1\n"}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeNameConflict {
		t.Fatalf("code=%q", err.Code)
	}
	field := fieldError(t, err, "name")
	if field.Line != 1 {
		t.Fatalf("line=%d", field.Line)
	}
}

func TestCheckAcceptsAMatchingName(t *testing.T) {
	cfg := model.Config{YAML: "name: demo\nservices:\n  api:\n    image: demo:1\n"}
	if _, err := yamlspec.Check(composeApp(), cfg); err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
}

func TestCheckAcceptsANameFromAVariable(t *testing.T) {
	cfg := model.Config{
		YAML:      "name: ${APP_NAME}\nservices:\n  api:\n    image: demo:1\n",
		Variables: map[string]string{"APP_NAME": "demo"},
	}
	if _, err := yamlspec.Check(composeApp(), cfg); err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
}

func TestCheckRejectsANameFromAVariableThatDoesNotMatch(t *testing.T) {
	cfg := model.Config{
		YAML:      "name: ${APP_NAME}\nservices:\n  api:\n    image: demo:1\n",
		Variables: map[string]string{"APP_NAME": "other"},
	}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeNameConflict {
		t.Fatalf("code=%q", err.Code)
	}
}

func TestCheckWarnsAboutRiskyAccess(t *testing.T) {
	cfg := model.Config{
		YAML: "services:\n  api:\n    image: demo:1\n    privileged: true\n    pid: host\n" +
			"    network_mode: host\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n      - /:/host\n",
		Variables: map[string]string{},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	for _, want := range []string{"特权", "PID", "网络", "Docker socket", "敏感的宿主机路径"} {
		if !hasWarning(checked.Warnings, want) {
			t.Fatalf("the warnings %v do not mention %q", checked.Warnings, want)
		}
	}
}

func TestCheckRecordsOneShotServices(t *testing.T) {
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n  job:\n    image: busybox:1\n    x-hmoob-lifecycle: oneshot\n",
		Variables: map[string]string{},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if len(checked.OneShotServices) != 1 || checked.OneShotServices[0] != "job" {
		t.Fatalf("one shot services=%v", checked.OneShotServices)
	}
	if strings.Contains(string(checked.RenderedYAML), yamlspec.LifecycleExtension) {
		t.Fatalf("the platform extension reached the Docker CLI: %s", checked.RenderedYAML)
	}
}

func TestCheckDoesNotGuessOneShotFromTheRestartPolicy(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    restart: \"no\"\n", Variables: map[string]string{}}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if len(checked.OneShotServices) != 0 {
		t.Fatalf("one shot services=%v", checked.OneShotServices)
	}
}

func TestCheckRejectsAnUnknownLifecycle(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    x-hmoob-lifecycle: forever\n"}
	err := checkFails(t, composeApp(), cfg)
	field := fieldError(t, err, "services.api."+yamlspec.LifecycleExtension)
	if field.Line != 4 {
		t.Fatalf("line=%d", field.Line)
	}
}

func TestCheckRejectsACustomTag(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: !secret demo:1\n"}
	err := checkFails(t, composeApp(), cfg)
	if !strings.Contains(err.Error(), "标签") {
		t.Fatalf("the error does not name the tag: %v", err)
	}
}

func TestCheckRejectsANonStringKey(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    labels:\n      1: value\n"}
	err := checkFails(t, composeApp(), cfg)
	if !strings.Contains(err.Error(), "键") {
		t.Fatalf("the error does not name the key: %v", err)
	}
}

func TestCheckRejectsAnUnknownMode(t *testing.T) {
	app := model.App{Mode: "nomad", Namespace: "demo"}
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n"}
	err := checkFails(t, app, cfg)
	if err.Code != yamlspec.CodeUnsupportedMode {
		t.Fatalf("code=%q", err.Code)
	}
}

func TestSwarmProfileRejectsUnsupportedServiceFields(t *testing.T) {
	fields := []string{
		"depends_on: [db]",
		"container_name: fixed",
		"restart: always",
		"privileged: true",
		"extends:\n      service: base",
		"env_file: .env",
	}
	for _, field := range fields {
		cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    " + field + "\n"}
		err := checkFails(t, swarmApp(), cfg)
		if err.Code != yamlspec.CodeUnsupportedField {
			t.Fatalf("field %q: code=%q", field, err.Code)
		}
	}
}

func TestSwarmProfileRejectsJobMode(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    deploy:\n      mode: job\n"}
	err := checkFails(t, swarmApp(), cfg)
	if !strings.Contains(err.Error(), "job") {
		t.Fatalf("the error does not name the job mode: %v", err)
	}
}

func TestSwarmProfileRejectsAnUnsupportedDeployField(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    deploy:\n      endpoint_mode: dnsrr\n"}
	err := checkFails(t, swarmApp(), cfg)
	if err.Code != yamlspec.CodeUnsupportedField {
		t.Fatalf("code=%q", err.Code)
	}
	field := fieldError(t, err, "services.api.deploy.endpoint_mode")
	if field.Line != 5 {
		t.Fatalf("line=%d", field.Line)
	}
}

func TestSwarmProfileChecksTheReferencesOfTheResources(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n    configs:\n      - source: app-config\n"}
	err := checkFails(t, swarmApp(), cfg)
	if !strings.Contains(err.Error(), "app-config") {
		t.Fatalf("the error does not name the resource: %v", err)
	}
}

func TestSwarmProfileRejectsAnExternalResourceWithAFile(t *testing.T) {
	cfg := model.Config{
		YAML: "services:\n  api:\n    image: demo:1\n    configs:\n      - source: app-config\n" +
			"configs:\n  app-config:\n    external: true\n    file: ./app.conf\n",
	}
	err := checkFails(t, swarmApp(), cfg)
	if !strings.Contains(err.Error(), "app-config") {
		t.Fatalf("the error does not name the resource: %v", err)
	}
}

func TestCheckDigestIsStable(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n", Variables: map[string]string{}}
	first, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	second, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("the digest is not stable: %q != %q", first.Digest, second.Digest)
	}
	if first.Digest == "" {
		t.Fatal("the configuration has no digest")
	}
}

func TestCheckDigestCoversTheVariables(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n", Variables: map[string]string{}}
	without, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	with := cfg
	with.Variables = map[string]string{"TAG": "2"}
	changed, err := yamlspec.Check(composeApp(), with)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if without.Digest == changed.Digest {
		t.Fatal("the digest does not cover the variables")
	}
}

func TestCheckDigestCoversTheIdentityOfTheApplication(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n", Variables: map[string]string{}}
	first, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	app := composeApp()
	app.ID = "app-2"
	second, err := yamlspec.Check(app, cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if first.Digest == second.Digest {
		t.Fatal("the digest does not cover the identity of the application")
	}
}

func TestCheckDigestIgnoresTheVersionOfTheApplication(t *testing.T) {
	cfg := model.Config{YAML: "services:\n  api:\n    image: demo:1\n", Variables: map[string]string{}}
	first, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	app := composeApp()
	app.LatestVersionID = "version-2"
	app.SuccessfulVersionID = "version-1"
	second, err := yamlspec.Check(app, cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatal("the digest covers a field of the application that changes")
	}
}
