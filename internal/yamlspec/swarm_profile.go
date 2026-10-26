package yamlspec

import (
	"errors"
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/schema"
)

// swarmTopLevelFields holds the top level fields the Swarm profile
// supports.
//
// A field that starts with "x-" is an extension of the Compose
// specification. The Swarm CLI ignores such a field, so the platform
// accepts it as well.
var swarmTopLevelFields = map[string]bool{
	"version":  true,
	"name":     true,
	"services": true,
	"networks": true,
	"volumes":  true,
	"configs":  true,
	"secrets":  true,
}

// swarmServiceFields holds the service fields the Swarm profile supports.
//
// A service of a stack describes a Swarm service. The fields that only
// describe a container of a Compose project, such as the name of the
// container or its restart policy, have no meaning in a Swarm service and
// are refused instead of being ignored.
var swarmServiceFields = map[string]bool{
	"image":             true,
	"command":           true,
	"entrypoint":        true,
	"environment":       true,
	"ports":             true,
	"volumes":           true,
	"networks":          true,
	"healthcheck":       true,
	"configs":           true,
	"secrets":           true,
	"labels":            true,
	"logging":           true,
	"stop_grace_period": true,
	"deploy":            true,
}

// swarmDeployFields holds the fields of the deploy section the Swarm
// profile supports.
var swarmDeployFields = map[string]bool{
	"replicas":        true,
	"mode":            true,
	"placement":       true,
	"resources":       true,
	"restart_policy":  true,
	"update_config":   true,
	"rollback_config": true,
}

// swarmDeployModes holds the modes a service of a stack may run in.
//
// The job mode describes a task that runs once and exits. The first version
// of the platform does not support it, because a job needs a different
// readiness check than a service.
var swarmDeployModes = map[string]bool{
	"replicated": true,
	"global":     true,
}

// checkSwarm validates the document against the supported Swarm profile.
//
// The profile is the part of the Compose specification the Swarm stack
// understands. A field outside of the profile is refused, so the CLI does
// not ignore it silently.
func checkSwarm(document map[string]any, index map[string]int) error {
	for _, key := range sortedKeys(document) {
		if swarmTopLevelFields[key] || strings.HasPrefix(key, "x-") {
			continue
		}
		return unsupportedField(key, index, "Swarm 模式不支持顶层字段 "+key)
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
		for _, key := range sortedKeys(service) {
			if swarmServiceFields[key] {
				continue
			}
			return unsupportedField(path+"."+key, index, "Swarm 模式不支持服务字段 "+key)
		}
		if err := checkServiceImage(service, name, path, index); err != nil {
			return err
		}
		if err := checkSwarmDeploy(service, path, index); err != nil {
			return err
		}
	}
	if err := checkSwarmResources(document, services, names, index); err != nil {
		return err
	}
	if err := schema.Validate(document); err != nil {
		return schemaError(err, index)
	}
	return nil
}

// checkSwarmDeploy validates the deploy section of a service.
func checkSwarmDeploy(service map[string]any, path string, index map[string]int) error {
	deploy, ok := service["deploy"].(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range sortedKeys(deploy) {
		if swarmDeployFields[key] {
			continue
		}
		return unsupportedField(path+".deploy."+key, index, "Swarm 模式不支持 deploy 字段 "+key)
	}
	mode, ok := deploy["mode"].(string)
	if !ok || mode == "" {
		return nil
	}
	if swarmDeployModes[mode] {
		return nil
	}
	if mode == "job" {
		return unsupportedField(path+".deploy.mode", index,
			"Swarm 的一次性任务（deploy.mode: job）首版不支持")
	}
	return unsupportedField(path+".deploy.mode", index, "deploy.mode 只能是 replicated 或 global")
}

// checkSwarmResources checks the references to the resources of the
// document.
//
// A service may only use a config or a secret the document declares, and a
// resource that is managed outside of the platform must not carry a file,
// because the file belongs to the machine that keeps the resource.
func checkSwarmResources(document map[string]any, services map[string]any, names []string, index map[string]int) error {
	declared := map[string]map[string]bool{}
	for _, kind := range []string{"configs", "secrets"} {
		resources := map[string]bool{}
		value, ok := document[kind].(map[string]any)
		if ok {
			for _, name := range sortedKeys(value) {
				definition := value[name]
				resources[name] = true
				fields, ok := definition.(map[string]any)
				if !ok {
					continue
				}
				external, ok := fields["external"].(bool)
				if !ok || !external {
					continue
				}
				if file, ok := fields["file"].(string); ok && strings.TrimSpace(file) != "" {
					return unsupportedField(kind+"."+name+".file", index,
						fmt.Sprintf("外部资源 %s %s 不能带有 file", kind, name))
				}
			}
		}
		declared[kind] = resources
	}
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			continue
		}
		for _, kind := range []string{"configs", "secrets"} {
			references, err := resourceReferences(service[kind])
			if err != nil {
				return unsupportedField("services."+name+"."+kind, index, err.Error())
			}
			for _, reference := range references {
				if !declared[kind][reference] {
					return unsupportedField("services."+name+"."+kind, index,
						fmt.Sprintf("服务 %s 引用了未声明的 %s %s", name, kind, reference))
				}
			}
		}
	}
	return nil
}

// resourceReferences returns the names of the resources a service refers to.
func resourceReferences(value any) ([]string, error) {
	values, ok := value.([]any)
	if !ok {
		return nil, nil
	}
	var references []string
	for _, item := range values {
		switch typed := item.(type) {
		case string:
			references = append(references, typed)
		case map[string]any:
			source, ok := typed["source"].(string)
			if !ok {
				return nil, errors.New("资源配置必须指定 source")
			}
			references = append(references, source)
		default:
			return nil, errors.New("资源配置必须是字符串或映射")
		}
	}
	return references, nil
}
