// Package yamlspec validates the deployment configuration of an
// application.
//
// The package is a pure function of the application and of the submitted
// configuration: it neither talks to Docker nor to the database, so a
// configuration is checked before anything on the target changes.
//
// The variables of a configuration come from the explicit variables of the
// application only. The environment of the control端 is never read, so a
// configuration cannot depend on the machine the control端 happens to run
// on.
package yamlspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/schema"
	"gopkg.in/yaml.v3"

	"hmoob-dock/internal/model"
)

// The modes of an application.
const (
	// ModeCompose runs an application with Docker Compose.
	ModeCompose = "compose"
	// ModeSwarm runs an application as a Docker Stack.
	ModeSwarm = "swarm"
)

// The limits of a configuration.
const (
	// MaxDocumentSize is the largest configuration document that is
	// accepted, in bytes.
	MaxDocumentSize = 1 << 20
	// MaxVariables is the largest number of variables of an application.
	MaxVariables = 256
	// MaxVariablesSize is the largest total size of the variables of an
	// application, in bytes.
	MaxVariablesSize = 256 << 10
)

// The limits of the structure of a configuration document.
const (
	// maxDepth is the deepest nesting a document may have.
	maxDepth = 64
	// maxNodes is the largest number of nodes a document may expand to.
	maxNodes = 50000
	// maxAliases is the largest number of aliases a document may use.
	maxAliases = 2000
)

// LifecycleExtension marks a service with the lifecycle the platform
// expects.
//
// The extension is a platform extension and not part of the Compose
// specification, so it is removed before the document reaches the Docker
// CLI.
const (
	// LifecycleExtension is the name of the platform extension.
	LifecycleExtension = "x-hmoob-lifecycle"
	// LifecycleOneShot marks a service that runs once and exit.
	LifecycleOneShot = "oneshot"
)

// The codes of a failed check. They are part of the answer of the API, so
// they stay stable.
const (
	// CodeInvalidConfig marks a configuration that does not pass the check.
	CodeInvalidConfig = "invalid_config"
	// CodeConfigTooLarge marks a configuration that exceeds the limits.
	CodeConfigTooLarge = "config_too_large"
	// CodeInvalidYAML marks a document that is not valid YAML.
	CodeInvalidYAML = "invalid_yaml"
	// CodeMissingVariable marks a required variable that is not set.
	CodeMissingVariable = "missing_variable"
	// CodeUnsupportedField marks a field the platform does not support.
	CodeUnsupportedField = "unsupported_field"
	// CodeNameConflict marks a name that does not match the namespace.
	CodeNameConflict = "name_conflict"
	// CodeUnsupportedMode marks an application mode the platform does not
	// support.
	CodeUnsupportedMode = "unsupported_mode"
)

// The tags of the YAML nodes the platform reads. A node with another tag is
// refused instead of being executed.
const (
	strTag   = "!!str"
	mergeTag = "!!merge"
)

// allowedTags holds the YAML tags the platform reads.
var allowedTags = map[string]bool{
	strTag:        true,
	"!!int":       true,
	"!!float":     true,
	"!!bool":      true,
	"!!null":      true,
	"!!timestamp": true,
	"!!binary":    true,
	"!!map":       true,
	"!!seq":       true,
	mergeTag:      true,
}

// yamlErrorLine reads the line of a YAML error.
var yamlErrorLine = regexp.MustCompile(`line (\d+)`)

// FieldError describes one field of the configuration that failed the
// validation.
type FieldError struct {
	// Path is the path of the field inside the configuration, for example
	// "services.api.image".
	Path string `json:"path"`
	// Line is the line of the field in the submitted document. It is 0
	// when the field does not belong to the document.
	Line int `json:"line,omitempty"`
	// Message describes the problem of the field.
	Message string `json:"message"`
}

// Error is the error of a failed configuration check.
//
// The fields carry the path and the line of every field that failed, so the
// API can point the administrator at the line to change. The message never
// holds the value of a variable.
type Error struct {
	// Code names the kind of the failure.
	Code string `json:"code"`
	// Message describes the failure.
	Message string `json:"message"`
	// Fields lists the fields that failed the validation.
	Fields []FieldError `json:"fields,omitempty"`
}

// Error returns the message of the failure with the fields it names.
func (e *Error) Error() string {
	if len(e.Fields) == 0 {
		return e.Message
	}
	details := make([]string, 0, len(e.Fields))
	for _, field := range e.Fields {
		if field.Line > 0 {
			details = append(details, fmt.Sprintf("%s（第 %d 行）：%s", field.Path, field.Line, field.Message))
			continue
		}
		details = append(details, fmt.Sprintf("%s：%s", field.Path, field.Message))
	}
	return e.Message + "：" + strings.Join(details, "；")
}

// Check validates the configuration of an application and prepares the
// payload of its deployment.
//
// The check reads the configuration and the explicit variables of the
// application and nothing else: it does not talk to Docker and it does not
// touch the database. The variables are resolved from the explicit variables
// only, the environment of the control端 is never read.
func Check(app model.App, cfg model.Config) (model.CheckedConfig, error) {
	if err := checkInputBounds(cfg); err != nil {
		return model.CheckedConfig{}, err
	}
	if app.Mode != ModeCompose && app.Mode != ModeSwarm {
		return model.CheckedConfig{}, &Error{
			Code:    CodeUnsupportedMode,
			Message: fmt.Sprintf("平台不支持应用模式 %q", app.Mode),
		}
	}
	if strings.TrimSpace(app.Namespace) == "" {
		return model.CheckedConfig{}, &Error{
			Code:    CodeInvalidConfig,
			Message: "应用缺少命名空间",
		}
	}

	document, index, err := parseDocument(cfg.YAML)
	if err != nil {
		return model.CheckedConfig{}, err
	}
	warnings, err := scanVariables(document, index, cfg.Variables)
	if err != nil {
		return model.CheckedConfig{}, err
	}
	resolved, err := interpolate(document, cfg.Variables, index)
	if err != nil {
		return model.CheckedConfig{}, err
	}
	if err := checkName(resolved, app.Namespace, index); err != nil {
		return model.CheckedConfig{}, err
	}
	switch app.Mode {
	case ModeCompose:
		err = checkCompose(resolved, index)
	case ModeSwarm:
		err = checkSwarm(resolved, index)
	}
	if err != nil {
		return model.CheckedConfig{}, err
	}
	services, oneShot, err := collectServices(resolved, app.Mode, index)
	if err != nil {
		return model.CheckedConfig{}, err
	}
	warnings = append(warnings, collectWarnings(resolved)...)
	sort.Strings(warnings)

	digest, err := computeDigest(app, cfg.Variables, resolved)
	if err != nil {
		return model.CheckedConfig{}, err
	}
	payload, err := renderPayload(resolved)
	if err != nil {
		return model.CheckedConfig{}, err
	}

	return model.CheckedConfig{
		RenderedYAML:    payload,
		Services:        services,
		OneShotServices: oneShot,
		Warnings:        warnings,
		Digest:          digest,
	}, nil
}

// checkInputBounds refuses a configuration that exceeds the limits of the
// platform.
func checkInputBounds(cfg model.Config) error {
	if len(cfg.YAML) > MaxDocumentSize {
		return &Error{
			Code:    CodeConfigTooLarge,
			Message: fmt.Sprintf("配置正文超过 %d 字节", MaxDocumentSize),
		}
	}
	if len(cfg.Variables) > MaxVariables {
		return &Error{
			Code:    CodeConfigTooLarge,
			Message: fmt.Sprintf("变量超过 %d 个", MaxVariables),
		}
	}
	total := 0
	for name, value := range cfg.Variables {
		total += len(name) + len(value)
	}
	if total > MaxVariablesSize {
		return &Error{
			Code:    CodeConfigTooLarge,
			Message: fmt.Sprintf("变量总计超过 %d 字节", MaxVariablesSize),
		}
	}
	return nil
}

// parseDocument reads the configuration document.
//
// The parser applies the limits of the platform: the depth of the nesting,
// the number of expanded nodes and the number of aliases are bounded, so a
// document cannot exhaust the memory of the control端. The walk collects
// the line of every field, so a failed validation can point at the line the
// administrator has to change.
func parseDocument(text string) (map[string]any, map[string]int, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, nil, &Error{
			Code:    CodeInvalidYAML,
			Message: "配置不是合法的 YAML 文档",
			Fields: []FieldError{{
				Line:    errorLine(err),
				Message: err.Error(),
			}},
		}
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, nil, &Error{
			Code:    CodeInvalidConfig,
			Message: "配置为空",
		}
	}
	content := root.Content[0]
	if content.Kind != yaml.MappingNode {
		return nil, nil, &Error{
			Code:    CodeInvalidConfig,
			Message: "配置必须是键值映射",
			Fields:  []FieldError{{Line: content.Line, Message: "配置的根节点必须是映射"}},
		}
	}
	if err := checkLimits(content); err != nil {
		return nil, nil, err
	}
	index, err := inspectDocument(content)
	if err != nil {
		return nil, nil, err
	}
	var document map[string]any
	if err := content.Decode(&document); err != nil {
		return nil, nil, &Error{
			Code:    CodeInvalidYAML,
			Message: "配置无法读取",
			Fields:  []FieldError{{Line: content.Line, Message: err.Error()}},
		}
	}
	return document, index, nil
}

// errorLine reads the line of a YAML error.
func errorLine(err error) int {
	match := yamlErrorLine.FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return 0
	}
	line := 0
	for _, digit := range match[1] {
		line = line*10 + int(digit-'0')
	}
	return line
}

// limitState counts the nodes and the aliases of a document.
type limitState struct {
	nodes   int
	aliases int
}

// checkLimits walks the document and enforces the limits of the platform.
//
// The aliases are followed, because a small document can expand into a huge
// tree. Every expansion counts towards the limit, so a document that hides
// thousands of nodes behind a few anchors is refused.
func checkLimits(root *yaml.Node) error {
	return walkLimits(root, 0, &limitState{}, map[*yaml.Node]bool{})
}

// walkLimits walks one node of the document.
func walkLimits(node *yaml.Node, depth int, state *limitState, anchors map[*yaml.Node]bool) error {
	if depth > maxDepth {
		return &Error{
			Code:    CodeInvalidConfig,
			Message: fmt.Sprintf("配置的嵌套层级超过 %d 层", maxDepth),
			Fields:  []FieldError{{Line: node.Line, Message: "文档嵌套过深"}},
		}
	}
	state.nodes++
	if state.nodes > maxNodes {
		return &Error{
			Code:    CodeInvalidConfig,
			Message: fmt.Sprintf("配置展开后的节点超过 %d 个", maxNodes),
			Fields:  []FieldError{{Line: node.Line, Message: "文档展开后过大"}},
		}
	}
	switch node.Kind {
	case yaml.AliasNode:
		state.aliases++
		if state.aliases > maxAliases {
			return &Error{
				Code:    CodeInvalidConfig,
				Message: fmt.Sprintf("配置的别名超过 %d 个", maxAliases),
				Fields:  []FieldError{{Line: node.Line, Message: "文档使用了过多别名"}},
			}
		}
		target := node.Alias
		if target == nil {
			return &Error{
				Code:    CodeInvalidConfig,
				Message: "配置的别名没有目标",
				Fields:  []FieldError{{Line: node.Line, Message: "别名没有目标"}},
			}
		}
		if anchors[target] {
			return &Error{
				Code:    CodeInvalidConfig,
				Message: "配置的别名形成了循环",
				Fields:  []FieldError{{Line: node.Line, Message: "别名形成了循环"}},
			}
		}
		anchors[target] = true
		defer delete(anchors, target)
		return walkLimits(target, depth+1, state, anchors)
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := walkLimits(node.Content[i], depth+1, state, anchors); err != nil {
				return err
			}
			if err := walkLimits(node.Content[i+1], depth+1, state, anchors); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if err := walkLimits(item, depth+1, state, anchors); err != nil {
				return err
			}
		}
	}
	return nil
}

// inspectDocument walks the document, checks the shape of its nodes and
// collects the line of every field.
//
// A custom tag is refused, because the platform never executes it, and a key
// has to be a string, because the Compose specification describes the
// document with named fields.
func inspectDocument(root *yaml.Node) (map[string]int, error) {
	index := map[string]int{}
	if err := inspectNode(root, "", index); err != nil {
		return nil, err
	}
	return index, nil
}

// inspectNode walks one node of the document.
func inspectNode(node *yaml.Node, path string, index map[string]int) error {
	switch node.Kind {
	case yaml.AliasNode:
		// The target of an alias is checked where its anchor is defined.
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			child := key.Value
			if path != "" {
				child = path + "." + key.Value
			}
			if key.Kind != yaml.ScalarNode || key.Tag != strTag {
				return &Error{
					Code:    CodeInvalidConfig,
					Message: "配置的键必须是字符串",
					Fields:  []FieldError{{Path: child, Line: key.Line, Message: "键必须是字符串"}},
				}
			}
			index[child] = key.Line
			if err := inspectNode(value, child, index); err != nil {
				return err
			}
		}
		return nil
	case yaml.SequenceNode:
		for i, item := range node.Content {
			child := fmt.Sprintf("%s.%d", path, i)
			index[child] = item.Line
			if err := inspectNode(item, child, index); err != nil {
				return err
			}
		}
		return nil
	case yaml.ScalarNode:
		if !allowedTags[node.Tag] {
			return &Error{
				Code:    CodeInvalidConfig,
				Message: "配置使用了平台不支持的 YAML 标签",
				Fields:  []FieldError{{Path: path, Line: node.Line, Message: "不支持的 YAML 标签 " + node.Tag}},
			}
		}
		return nil
	default:
		return &Error{
			Code:    CodeInvalidConfig,
			Message: "配置包含平台无法读取的节点",
			Fields:  []FieldError{{Path: path, Line: node.Line, Message: "无法读取的节点"}},
		}
	}
}

// checkName makes sure that the document does not redirect the application.
//
// The name of the document is the name of the Compose project and of the
// Swarm stack. It has to match the namespace of the application, so a
// configuration cannot deploy into another namespace. The name is checked
// after the interpolation, because a name may come from a variable.
func checkName(document map[string]any, namespace string, index map[string]int) error {
	value, ok := document["name"]
	if !ok {
		return nil
	}
	name, ok := value.(string)
	if !ok {
		return &Error{
			Code:    CodeInvalidConfig,
			Message: "配置的 name 必须是字符串",
			Fields:  []FieldError{{Path: "name", Line: index["name"], Message: "name 必须是字符串"}},
		}
	}
	if name != namespace {
		return &Error{
			Code:    CodeNameConflict,
			Message: fmt.Sprintf("配置的 name %q 与应用命名空间 %q 不一致", name, namespace),
			Fields:  []FieldError{{Path: "name", Line: index["name"], Message: "name 必须与应用命名空间一致"}},
		}
	}
	return nil
}

// checkCompose validates the document against the complete Compose schema
// and against the restrictions of the platform.
//
// The schema of the Compose specification is the reference for the values a
// service may hold. The restrictions of the platform are stricter: the
// platform deploys a ready made image and never reads a file of the machine
// the control端 runs on.
func checkCompose(document map[string]any, index map[string]int) error {
	if _, ok := document["include"]; ok {
		return unsupportedField("include", index, "配置必须自包含，平台不支持 include")
	}
	services, names, err := serviceList(document, index)
	if err != nil {
		return err
	}
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		path := "services." + name
		if err := checkForbiddenServiceFields(service, path, index); err != nil {
			return err
		}
		if err := checkServiceImage(service, name, path, index); err != nil {
			return err
		}
	}
	if err := schema.Validate(document); err != nil {
		return schemaError(err, index)
	}
	return nil
}

// checkForbiddenServiceFields refuses the service fields the platform does
// not support in any mode.
//
// A service is built from a ready made image, a service with a profile would
// be skipped silently and an extension of another file would not be part of
// the version the platform stores.
func checkForbiddenServiceFields(service map[string]any, path string, index map[string]int) error {
	if _, ok := service["build"]; ok {
		return unsupportedField(path+".build", index, "平台只部署现成镜像，不支持 build")
	}
	if _, ok := service["profiles"]; ok {
		return unsupportedField(path+".profiles", index, "平台不激活 profile，带 profiles 的服务会被静默跳过")
	}
	if extends, ok := service["extends"].(map[string]any); ok {
		if file, ok := extends["file"].(string); ok && strings.TrimSpace(file) != "" {
			return unsupportedField(path+".extends.file", index, "平台不支持跨文件 extends")
		}
	}
	return nil
}

// checkServiceImage makes sure that a service names the image it runs.
func checkServiceImage(service map[string]any, name, path string, index map[string]int) error {
	if _, ok := service["image"]; ok {
		return nil
	}
	return &Error{
		Code:    CodeInvalidConfig,
		Message: fmt.Sprintf("服务 %s 必须指定 image", name),
		Fields:  []FieldError{{Path: path, Line: index[path], Message: "服务必须指定 image"}},
	}
}

// serviceList returns the services of the document with their names in a
// stable order.
func serviceList(document map[string]any, index map[string]int) (map[string]any, []string, error) {
	value, ok := document["services"]
	if !ok {
		return nil, nil, &Error{
			Code:    CodeInvalidConfig,
			Message: "配置缺少 services 段",
		}
	}
	services, ok := value.(map[string]any)
	if !ok {
		return nil, nil, &Error{
			Code:    CodeInvalidConfig,
			Message: "services 必须是映射",
			Fields:  []FieldError{{Path: "services", Line: index["services"], Message: "services 必须是映射"}},
		}
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return services, names, nil
}

// sortedKeys returns the keys of a mapping in a stable order.
//
// The order of a Go map is random, so a validation that walks a mapping has
// to sort its keys. Otherwise the same configuration would report a
// different field on every run.
func sortedKeys(mapping map[string]any) []string {
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// collectServices returns the services of the document and the services that
// run once and exit.
//
// A service is a one shot service only when it says so. The restart policy
// of a service does not tell whether the service ends by itself, so the
// platform never guesses it.
func collectServices(document map[string]any, mode string, index map[string]int) ([]string, []string, error) {
	services, names, err := serviceList(document, index)
	if err != nil {
		return nil, nil, err
	}
	var oneShot []string
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		value, ok := service[LifecycleExtension]
		if !ok {
			continue
		}
		path := "services." + name + "." + LifecycleExtension
		if mode != ModeCompose {
			return nil, nil, unsupportedField(path, index,
				"Swarm 模式不支持 "+LifecycleExtension+"，Swarm 的一次性任务使用 deploy.mode: job")
		}
		lifecycle, ok := value.(string)
		if !ok || lifecycle != LifecycleOneShot {
			return nil, nil, unsupportedField(path, index,
				LifecycleExtension+" 只接受 "+LifecycleOneShot)
		}
		oneShot = append(oneShot, name)
	}
	return names, oneShot, nil
}

// sensitiveHostPaths are the paths of the host that a container must not see
// without a confirmation.
var sensitiveHostPaths = []string{
	"/", "/boot", "/dev", "/etc", "/home", "/proc", "/root", "/run", "/sys",
	"/var/lib/docker", "/var/run",
}

// collectWarnings reports the risks of the document.
//
// Every warning reaches the preview of the deployment, so the administrator
// confirms a risky configuration instead of discovering it on the target.
func collectWarnings(document map[string]any) []string {
	services, names, err := serviceList(document, nil)
	if err != nil {
		return nil
	}
	var warnings []string
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		if privileged, ok := service["privileged"].(bool); ok && privileged {
			warnings = append(warnings, fmt.Sprintf("服务 %s 以特权模式运行", name))
		}
		if pid, ok := service["pid"].(string); ok && pid == "host" {
			warnings = append(warnings, fmt.Sprintf("服务 %s 共享宿主机的 PID 命名空间", name))
		}
		if network, ok := service["network_mode"].(string); ok && network == "host" {
			warnings = append(warnings, fmt.Sprintf("服务 %s 共享宿主机网络命名空间", name))
		}
		for _, source := range volumeSources(service) {
			if strings.HasSuffix(source, "docker.sock") {
				warnings = append(warnings, fmt.Sprintf("服务 %s 挂载了 Docker socket", name))
				continue
			}
			if sensitiveHostPath(source) {
				warnings = append(warnings, fmt.Sprintf("服务 %s 挂载了敏感的宿主机路径 %s", name, source))
			}
		}
	}
	return warnings
}

// volumeSources returns the host paths a service mounts.
func volumeSources(service map[string]any) []string {
	values, ok := service["volumes"].([]any)
	if !ok {
		return nil
	}
	var sources []string
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if source, _, ok := strings.Cut(typed, ":"); ok {
				sources = append(sources, source)
			}
		case map[string]any:
			if source, ok := typed["source"].(string); ok {
				sources = append(sources, source)
			}
		}
	}
	return sources
}

// sensitiveHostPath reports whether a path of the host is one that a
// container must not see without a confirmation.
func sensitiveHostPath(source string) bool {
	if source == "" || !strings.HasPrefix(source, "/") {
		return false
	}
	cleaned := strings.TrimSuffix(source, "/")
	if cleaned == "" {
		cleaned = "/"
	}
	for _, path := range sensitiveHostPaths {
		if cleaned == path {
			return true
		}
		if path != "/" && strings.HasPrefix(cleaned, path+"/") {
			return true
		}
	}
	return false
}

// computeDigest computes the digest of a checked configuration.
//
// The digest covers the normalized configuration, the explicit variables and
// the immutable identity of the application, so a change of any of them
// produces a different digest. It is computed before the dollar signs are
// encoded for the Docker CLI, so the digest describes the semantics of the
// configuration and not the encoding of its payload.
//
// The digest is a hash, so it never holds the value of a variable.
func computeDigest(app model.App, variables map[string]string, document map[string]any) (string, error) {
	payload := struct {
		App       model.App         `json:"app"`
		Variables map[string]string `json:"variables"`
		Config    map[string]any    `json:"config"`
	}{
		App: model.App{
			ID:        app.ID,
			Name:      app.Name,
			Mode:      app.Mode,
			TargetID:  app.TargetID,
			Namespace: app.Namespace,
		},
		Variables: variables,
		Config:    document,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("yamlspec: compute the digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// renderPayload builds the document that is handed to the Docker CLI.
//
// The platform extension is removed, because the Docker CLI does not know
// it, and every literal dollar sign is encoded as "$$", so the second
// interpolation of the CLI does not change the value of a password or of a
// command.
func renderPayload(document map[string]any) ([]byte, error) {
	payload, err := yaml.Marshal(escapeDollar(stripLifecycle(document)))
	if err != nil {
		return nil, fmt.Errorf("yamlspec: render the configuration: %w", err)
	}
	return payload, nil
}

// stripLifecycle removes the platform extension from the services of the
// document.
func stripLifecycle(document map[string]any) map[string]any {
	stripped := make(map[string]any, len(document))
	for key, value := range document {
		if key != "services" {
			stripped[key] = value
			continue
		}
		services, ok := value.(map[string]any)
		if !ok {
			stripped[key] = value
			continue
		}
		cleaned := make(map[string]any, len(services))
		for name, service := range services {
			definition, ok := service.(map[string]any)
			if !ok {
				cleaned[name] = service
				continue
			}
			withoutLifecycle := make(map[string]any, len(definition))
			for field, item := range definition {
				if field == LifecycleExtension {
					continue
				}
				withoutLifecycle[field] = item
			}
			cleaned[name] = withoutLifecycle
		}
		stripped[key] = cleaned
	}
	return stripped
}

// escapeDollar encodes every literal dollar sign as "$$".
//
// The Docker CLI interpolates the document it reads a second time. A dollar
// sign that reaches the CLI unchanged would start a variable reference
// there, so a password or a command would change on the way to the target.
func escapeDollar(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		escaped := make(map[string]any, len(typed))
		for key, item := range typed {
			escaped[strings.ReplaceAll(key, "$", "$$")] = escapeDollar(item)
		}
		return escaped
	case []any:
		escaped := make([]any, len(typed))
		for i, item := range typed {
			escaped[i] = escapeDollar(item)
		}
		return escaped
	case string:
		return strings.ReplaceAll(typed, "$", "$$")
	default:
		return value
	}
}

// schemaError turns the error of the Compose schema into the error of the
// platform.
//
// The message of the schema names the path of the field first, so the path
// is taken from there and the line of the field is looked up in the index of
// the document.
func schemaError(err error, index map[string]int) error {
	message := err.Error()
	path := ""
	if end := strings.IndexByte(message, ' '); end > 0 {
		path = knownPath(message[:end], index)
	}
	field := FieldError{Message: message}
	if path != "" {
		field.Path = path
		field.Line = index[path]
	}
	return &Error{
		Code:    CodeInvalidConfig,
		Message: "配置不符合 Compose 规范",
		Fields:  []FieldError{field},
	}
}

// knownPath returns the longest path of the document the message starts
// with.
func knownPath(candidate string, index map[string]int) string {
	for candidate != "" {
		if _, ok := index[candidate]; ok {
			return candidate
		}
		next := strings.LastIndex(candidate, ".")
		if next < 0 {
			return ""
		}
		candidate = candidate[:next]
	}
	return ""
}

// unsupportedField reports a field the platform does not support.
func unsupportedField(path string, index map[string]int, message string) error {
	return &Error{
		Code:    CodeUnsupportedField,
		Message: message,
		Fields:  []FieldError{{Path: path, Line: index[path], Message: message}},
	}
}
