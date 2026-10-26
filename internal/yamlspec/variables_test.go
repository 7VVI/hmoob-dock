package yamlspec_test

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/interpolation"
	"github.com/compose-spec/compose-go/v2/template"
	"gopkg.in/yaml.v3"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/yamlspec"
)

// interpolateAgain simulates the interpolation of the Docker CLI, which reads
// the rendered document and resolves the variables it finds.
func interpolateAgain(t *testing.T, document []byte) map[string]any {
	t.Helper()
	var raw map[string]any
	if err := yaml.Unmarshal(document, &raw); err != nil {
		t.Fatalf("read the rendered document: %v", err)
	}
	result, err := interpolation.Interpolate(raw, interpolation.Options{
		LookupValue: func(string) (string, bool) { return "", false },
		// The library writes a warning to the log for a variable that is
		// not set. The test reads the log of the test run, so the warning
		// is turned off here.
		Substitute: func(value string, mapping template.Mapping) (string, error) {
			return template.SubstituteWithOptions(value, mapping, template.WithoutLogging)
		},
	})
	if err != nil {
		t.Fatalf("the second interpolation failed: %v", err)
	}
	return result
}

func TestInterpolationNeverReadsTheProcessEnvironment(t *testing.T) {
	t.Setenv("HOST_ONLY", "host-secret")
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: \"demo:${HOST_ONLY:-fallback}\"\n",
		Variables: map[string]string{},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if got := serviceValue(t, checked.RenderedYAML, "api", "image"); got != "demo:fallback" {
		t.Fatalf("image=%v", got)
	}
}

func TestVariableDefaultValue(t *testing.T) {
	cases := []struct {
		name      string
		template  string
		variables map[string]string
		want      string
	}{
		{
			name:      "the default is used when the variable is missing",
			template:  "demo:${TAG:-fallback}",
			variables: map[string]string{},
			want:      "demo:fallback",
		},
		{
			name:      "the default is used when the variable is empty",
			template:  "demo:${TAG:-fallback}",
			variables: map[string]string{"TAG": ""},
			want:      "demo:fallback",
		},
		{
			name:      "the value wins over the default",
			template:  "demo:${TAG:-fallback}",
			variables: map[string]string{"TAG": "1.0"},
			want:      "demo:1.0",
		},
		{
			name:      "the hard default keeps an empty value",
			template:  "demo:${TAG-fallback}",
			variables: map[string]string{"TAG": ""},
			want:      "demo:",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cfg := model.Config{
				YAML:      "services:\n  api:\n    image: \"" + test.template + "\"\n",
				Variables: test.variables,
			}
			checked, err := yamlspec.Check(composeApp(), cfg)
			if err != nil {
				t.Fatalf("the configuration was rejected: %v", err)
			}
			if got := serviceValue(t, checked.RenderedYAML, "api", "image"); got != test.want {
				t.Fatalf("image=%v, want %v", got, test.want)
			}
		})
	}
}

func TestVariableRequiredValue(t *testing.T) {
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: \"demo:${TAG:?the tag is required}\"\n",
		Variables: map[string]string{},
	}
	err := checkFails(t, composeApp(), cfg)
	if err.Code != yamlspec.CodeMissingVariable {
		t.Fatalf("code=%q", err.Code)
	}
	field := fieldError(t, err, "services.api.image")
	if field.Line != 3 {
		t.Fatalf("line=%d", field.Line)
	}
	if !strings.Contains(err.Error(), "TAG") {
		t.Fatalf("the error does not name the variable: %v", err)
	}
}

func TestVariableEmptyValue(t *testing.T) {
	// A variable that is required and empty is refused.
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: \"demo:${TAG:?}\"\n",
		Variables: map[string]string{"TAG": ""},
	}
	if err := checkFails(t, composeApp(), cfg); err.Code != yamlspec.CodeMissingVariable {
		t.Fatalf("code=%q", err.Code)
	}

	// A variable that is only required to exist may be empty.
	cfg = model.Config{
		YAML:      "services:\n  api:\n    image: \"demo:${TAG?}\"\n",
		Variables: map[string]string{"TAG": ""},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if got := serviceValue(t, checked.RenderedYAML, "api", "image"); got != "demo:" {
		t.Fatalf("image=%v", got)
	}
}

func TestUnsetVariableIsReportedAsWarning(t *testing.T) {
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: \"demo:${TAG}\"\n",
		Variables: map[string]string{},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if !hasWarning(checked.Warnings, "TAG") {
		t.Fatalf("warnings=%v", checked.Warnings)
	}
	if got := serviceValue(t, checked.RenderedYAML, "api", "image"); got != "demo:" {
		t.Fatalf("image=%v", got)
	}
}

// TestDollarEscapeKeepsTheKeys proves that the escape of the dollar signs
// only touches the values of a mapping.
//
// The Docker CLI reads the keys of a mapping literally: it interpolates the
// values and copies the keys as they are. A key the platform escapes would
// reach the target with a dollar sign more than the administrator wrote.
func TestDollarEscapeKeepsTheKeys(t *testing.T) {
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n    labels:\n      a$b: v1\n    environment:\n      TOKEN: \"pa$$ss\"\n",
		Variables: map[string]string{},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	rendered := string(checked.RenderedYAML)
	if strings.Contains(rendered, "a$$b") {
		t.Fatalf("the key was escaped: %s", rendered)
	}
	if !strings.Contains(rendered, "a$b") {
		t.Fatalf("the key was lost: %s", rendered)
	}
	if !strings.Contains(rendered, "pa$$ss") {
		t.Fatalf("the value was not escaped: %s", rendered)
	}

	// The Docker CLI interpolates the rendered document a second time. The
	// key has to reach the target as the administrator wrote it, while the
	// value keeps its literal dollar sign.
	second := interpolateAgain(t, checked.RenderedYAML)
	service := second["services"].(map[string]any)["api"].(map[string]any)
	labels, ok := service["labels"].(map[string]any)
	if !ok {
		t.Fatalf("labels=%v", service["labels"])
	}
	if _, ok := labels["a$b"]; !ok {
		t.Fatalf("labels=%v", labels)
	}
	environment := service["environment"].(map[string]any)
	if environment["TOKEN"] != "pa$ss" {
		t.Fatalf("token=%v", environment["TOKEN"])
	}
}

func TestUnsetBareVariableIsReportedLikeABracedVariable(t *testing.T) {
	bare := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n    command: [\"sh\", \"-c\", \"curl -u $USER:$PASS\"]\n",
		Variables: map[string]string{},
	}
	braced := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n    command: [\"sh\", \"-c\", \"curl -u ${USER}:${PASS}\"]\n",
		Variables: map[string]string{},
	}
	bareChecked, err := yamlspec.Check(composeApp(), bare)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	bracedChecked, err := yamlspec.Check(composeApp(), braced)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if len(bareChecked.Warnings) != len(bracedChecked.Warnings) {
		t.Fatalf("warnings=%v, want %v", bareChecked.Warnings, bracedChecked.Warnings)
	}
	for i, warning := range bracedChecked.Warnings {
		if bareChecked.Warnings[i] != warning {
			t.Fatalf("the warning of the bare form is %q, want %q",
				bareChecked.Warnings[i], warning)
		}
	}
	for _, name := range []string{"USER", "PASS"} {
		if !hasWarning(bareChecked.Warnings, name) {
			t.Fatalf("the warnings %v do not mention %q", bareChecked.Warnings, name)
		}
	}
	if got := serviceValue(t, bareChecked.RenderedYAML, "api", "command"); got == nil {
		t.Fatal("the command was lost")
	}
}

func TestBareVariableIsInterpolated(t *testing.T) {
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n    environment:\n      TOKEN: \"$TOKEN\"\n",
		Variables: map[string]string{"TOKEN": "secret"},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	environment, ok := serviceValue(t, checked.RenderedYAML, "api", "environment").(map[string]any)
	if !ok {
		t.Fatal("the environment was lost")
	}
	if environment["TOKEN"] != "secret" {
		t.Fatalf("token=%v", environment["TOKEN"])
	}
	if len(checked.Warnings) != 0 {
		t.Fatalf("warnings=%v", checked.Warnings)
	}
}

func TestEscapedBareVariableIsNotAReference(t *testing.T) {
	cfg := model.Config{
		YAML:      "services:\n  api:\n    image: demo:1\n    environment:\n      TOKEN: \"$$TOKEN\"\n",
		Variables: map[string]string{},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	if hasWarning(checked.Warnings, "TOKEN") {
		t.Fatalf("the escaped reference was reported: %v", checked.Warnings)
	}
	environment, ok := serviceValue(t, checked.RenderedYAML, "api", "environment").(map[string]any)
	if !ok {
		t.Fatal("the environment was lost")
	}
	if environment["TOKEN"] != "$$TOKEN" {
		t.Fatalf("token=%v", environment["TOKEN"])
	}
	second := interpolateAgain(t, checked.RenderedYAML)
	service := second["services"].(map[string]any)["api"].(map[string]any)
	if got := service["environment"].(map[string]any)["TOKEN"]; got != "$TOKEN" {
		t.Fatalf("token=%v", got)
	}
}

func TestDollarEscapeRoundTrip(t *testing.T) {
	cfg := model.Config{
		YAML: "services:\n  api:\n    image: demo:1\n    command: [\"sh\", \"-c\", \"echo $$HOME\"]\n" +
			"    environment:\n      TOKEN: \"${TOKEN}\"\n",
		Variables: map[string]string{"TOKEN": "a${literal}b"},
	}
	checked, err := yamlspec.Check(composeApp(), cfg)
	if err != nil {
		t.Fatalf("the configuration was rejected: %v", err)
	}
	rendered := string(checked.RenderedYAML)
	if !strings.Contains(rendered, "$$HOME") {
		t.Fatalf("the escaped dollar sign is missing: %s", rendered)
	}
	if !strings.Contains(rendered, "a$${literal}b") {
		t.Fatalf("the value was not escaped: %s", rendered)
	}

	// The Docker CLI reads the rendered document and interpolates it a
	// second time. The result has to be the value the platform resolved,
	// otherwise a password or a command would change on the way to the
	// target.
	second := interpolateAgain(t, checked.RenderedYAML)
	service := second["services"].(map[string]any)["api"].(map[string]any)
	environment := service["environment"].(map[string]any)
	if environment["TOKEN"] != "a${literal}b" {
		t.Fatalf("token=%v", environment["TOKEN"])
	}
	command := service["command"].([]any)
	if command[2] != "echo $HOME" {
		t.Fatalf("command=%v", command)
	}
}
