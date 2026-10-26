package yamlspec

import "strings"

// WarningKind tells the administrator what a message of the platform asks
// of them.
//
// The kind belongs to the message, not to the place it was written: the
// platform writes its messages from the templates below and recognizes them
// by them, so a message never changes its kind by accident.
type WarningKind string

const (
	// WarningNotice is a message of the platform that needs no
	// confirmation. It describes a configuration the platform refuses
	// anyway, so the administrator confirms nothing.
	WarningNotice WarningKind = "notice"
	// WarningVariable is the notice of a variable that was not provided.
	// The value becomes an empty string, so the administrator confirms the
	// risks of a configuration, not the values of its variables.
	WarningVariable WarningKind = "variable"
	// WarningRisk is a risk of the configuration: the service does
	// something that can harm the target, so the administrator has to
	// confirm it.
	WarningRisk WarningKind = "risk"
)

// The templates of the messages of the platform.
//
// The templates are the single source of the messages: the platform writes
// them and recognizes them by them. A message the platform does not know is
// a risk, so a new risk of the platform is never accepted silently.
const (
	// warningVariableTemplate marks a variable that was not provided.
	warningVariableTemplate = "变量 %s 没有提供，将按空字符串处理"
	// warningPrivilegedTemplate marks a service that runs privileged.
	warningPrivilegedTemplate = "服务 %s 以特权模式运行"
	// warningPIDTemplate marks a service that shares the PID namespace of
	// the host.
	warningPIDTemplate = "服务 %s 共享宿主机的 PID 命名空间"
	// warningNetworkTemplate marks a service that shares the network
	// namespace of the host.
	warningNetworkTemplate = "服务 %s 共享宿主机网络命名空间"
	// warningSocketTemplate marks a service that mounts the Docker socket.
	warningSocketTemplate = "服务 %s 挂载了 Docker socket"
	// warningHostPathTemplate marks a service that mounts a sensitive path
	// of the host.
	warningHostPathTemplate = "服务 %s 挂载了敏感的宿主机路径 %s"
	// warningUndeclaredTemplate marks a service that refers to a resource
	// the document does not declare. The configuration is refused, so the
	// message asks for no confirmation.
	warningUndeclaredTemplate = "服务 %s 引用了未声明的 %s %s"
	// warningMissingImageTemplate marks a service that names no image. The
	// configuration is refused, so the message asks for no confirmation.
	warningMissingImageTemplate = "服务 %s 必须指定 image"
)

// warningTemplate is a message of the platform and the kind of it.
type warningTemplate struct {
	// text is the format of the message.
	text string
	// kind tells what the message asks of the administrator.
	kind WarningKind
}

// warningTemplates holds the messages of the platform and their kinds.
//
// The order matters: the first template that matches a message wins, so the
// risks are told before the notices.
var warningTemplates = []warningTemplate{
	{text: warningPrivilegedTemplate, kind: WarningRisk},
	{text: warningPIDTemplate, kind: WarningRisk},
	{text: warningNetworkTemplate, kind: WarningRisk},
	{text: warningSocketTemplate, kind: WarningRisk},
	{text: warningHostPathTemplate, kind: WarningRisk},
	{text: warningVariableTemplate, kind: WarningVariable},
	{text: warningUndeclaredTemplate, kind: WarningNotice},
	{text: warningMissingImageTemplate, kind: WarningNotice},
}

// WarningKindOf returns the kind of a message of the platform.
//
// A message the platform does not know is a risk: the administrator
// confirms it, so a risk the platform did not describe yet is never
// accepted silently.
func WarningKindOf(message string) WarningKind {
	for _, template := range warningTemplates {
		if matchesTemplate(message, template.text) {
			return template.kind
		}
	}
	return WarningRisk
}

// matchesTemplate reports whether a message was written from the template.
//
// The template holds the fixed parts around its parameters, so a message
// matches when it starts with the first part, ends with the last part and
// holds the parts in between.
func matchesTemplate(message, template string) bool {
	parts := strings.Split(template, "%s")
	if len(parts) == 1 {
		return message == template
	}
	if !strings.HasPrefix(message, parts[0]) {
		return false
	}
	if !strings.HasSuffix(message, parts[len(parts)-1]) {
		return false
	}
	for _, part := range parts[1 : len(parts)-1] {
		if !strings.Contains(message, part) {
			return false
		}
	}
	return len(message) >= len(parts[0])+len(parts[len(parts)-1])
}
