package yamlspec

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/interpolation"
	"github.com/compose-spec/compose-go/v2/template"
)

// plainReference matches a variable a document refers to without an
// operator.
//
// The leading dollar signs are part of the match, so an escaped reference
// can be told from a real one.
var plainReference = regexp.MustCompile(`\$+\{([_a-zA-Z][_a-zA-Z0-9]*)\}`)

// bareReference matches a variable a document refers to without braces.
//
// The Compose library reads a dollar sign followed by a name as a variable
// reference, so the platform has to read it the same way. The name follows
// the rule of the library: it starts with a letter or an underscore and
// holds letters, digits and underscores.
//
// The leading dollar signs are part of the match, so an escaped reference
// can be told from a real one.
var bareReference = regexp.MustCompile(`\$+([_a-zA-Z][_a-zA-Z0-9]*)`)

// interpolationPath reads the path of the field an interpolation error
// belongs to.
var interpolationPath = regexp.MustCompile(`(?:interpolating|format for)\s+([^\s:]+)`)

// missingValue reads the name of a variable the Compose library refused.
var missingValue = regexp.MustCompile(`required variable (\S+) is missing a value`)

// scanVariables reports the variables the document refers to.
//
// A required variable that is not set is an error, because the deployment
// would run with a value the administrator did not choose. A variable
// without a default that is not set is reported as a warning, because the
// Compose semantics replace it with an empty string.
//
// The scan runs before the interpolation, so a missing variable is reported
// with the path and the line of the field that refers to it.
func scanVariables(document map[string]any, index map[string]int, variables map[string]string) ([]string, error) {
	var missing []FieldError
	unset := map[string]bool{}
	walkStrings(document, "", func(path, value string) {
		for name, reference := range template.ExtractVariables(map[string]any{"value": value}, nil) {
			if _, ok := variables[name]; ok {
				continue
			}
			if !reference.Required {
				continue
			}
			missing = append(missing, FieldError{
				Path:    path,
				Line:    index[path],
				Message: fmt.Sprintf("变量 %s 是必填变量，但没有提供", name),
			})
		}
		for _, name := range plainReferences(value) {
			if _, ok := variables[name]; ok {
				continue
			}
			unset[name] = true
		}
		for _, name := range bareReferences(value) {
			if _, ok := variables[name]; ok {
				continue
			}
			unset[name] = true
		}
	})
	if len(missing) > 0 {
		sort.Slice(missing, func(i, j int) bool {
			if missing[i].Path != missing[j].Path {
				return missing[i].Path < missing[j].Path
			}
			return missing[i].Message < missing[j].Message
		})
		return nil, &Error{
			Code:    CodeMissingVariable,
			Message: "配置引用了没有提供的必填变量",
			Fields:  missing,
		}
	}

	names := make([]string, 0, len(unset))
	for name := range unset {
		names = append(names, name)
	}
	sort.Strings(names)
	warnings := make([]string, 0, len(names))
	for _, name := range names {
		warnings = append(warnings, fmt.Sprintf(warningVariableTemplate, name))
	}
	return warnings, nil
}

// plainReferences returns the names of the variables a value refers to
// without an operator.
//
// A reference that is escaped with a dollar sign is not a reference: the
// Compose semantics turn "$$" into a literal dollar sign.
func plainReferences(value string) []string {
	var names []string
	for _, match := range plainReference.FindAllStringSubmatch(value, -1) {
		dollars := len(match[0]) - len(match[1]) - 2
		if dollars%2 == 0 {
			continue
		}
		names = append(names, match[1])
	}
	return names
}

// bareReferences returns the names of the variables a value refers to
// without braces.
//
// The Compose library reads a dollar sign followed by a name as a variable
// reference, so the platform reports a variable of that shape the same way
// it reports a variable of the braced shape. A reference that is escaped
// with a dollar sign is not a reference: the Compose semantics turn "$$"
// into a literal dollar sign.
func bareReferences(value string) []string {
	var names []string
	for _, match := range bareReference.FindAllStringSubmatch(value, -1) {
		dollars := len(match[0]) - len(match[1])
		if dollars%2 == 0 {
			continue
		}
		names = append(names, match[1])
	}
	return names
}

// interpolate resolves the variables of the document.
//
// The lookup knows the explicit variables of the application only. The
// environment of the control端 is never read, so a configuration cannot
// depend on the machine the control端 happens to run on.
func interpolate(document map[string]any, variables map[string]string, index map[string]int) (map[string]any, error) {
	lookup := func(name string) (string, bool) {
		value, ok := variables[name]
		return value, ok
	}
	resolved, err := interpolation.Interpolate(document, interpolation.Options{
		LookupValue: lookup,
		Substitute:  substitute,
	})
	if err != nil {
		return nil, interpolationError(err, index)
	}
	return resolved, nil
}

// substitute replaces the variables of a value.
//
// The Compose library writes a warning to the log of the process for a
// variable that is not set. The platform reports a missing variable itself,
// so the log of the control端 stays free of noise.
func substitute(value string, mapping template.Mapping) (string, error) {
	return template.SubstituteWithOptions(value, mapping, template.WithoutLogging)
}

// interpolationError turns the error of the Compose interpolation into the
// error of the platform.
//
// The message of the library names the path of the field it failed at, so
// the path is taken from there and the line of the field is looked up in the
// index of the document.
func interpolationError(err error, index map[string]int) error {
	message := err.Error()
	code := CodeInvalidConfig
	detail := message
	if match := missingValue.FindStringSubmatch(message); match != nil {
		code = CodeMissingVariable
		detail = fmt.Sprintf("变量 %s 是必填变量，但没有提供值", match[1])
	}
	path := ""
	if match := interpolationPath.FindStringSubmatch(message); match != nil {
		path = strings.TrimSuffix(match[1], ".")
	}
	field := FieldError{Message: detail}
	if path != "" {
		field.Path = path
		field.Line = index[path]
	}
	return &Error{
		Code:    code,
		Message: "配置的变量插值失败",
		Fields:  []FieldError{field},
	}
}

// walkStrings visits every string of the document with its path.
func walkStrings(value any, path string, visit func(path, value string)) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			child := key
			if path != "" {
				child = path + "." + key
			}
			walkStrings(item, child, visit)
		}
	case []any:
		for i, item := range typed {
			walkStrings(item, fmt.Sprintf("%s.%d", path, i), visit)
		}
	case string:
		visit(path, typed)
	}
}
