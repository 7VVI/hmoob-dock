package dockerops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/yamlspec"
)

// The actions of the command line of Compose the platform runs.
//
// The platform starts a project and prepares it. It never stops a project
// and never removes a volume, so there is no action for that.
const (
	// ActionConfig validates the configuration of the project.
	ActionConfig = "config"
	// ActionPull pulls the images of the project.
	ActionPull = "pull"
	// ActionUp starts the project.
	ActionUp = "up"
)

// The codes of the outcomes of the executor of Compose.
const (
	// CodeInvalidConfig marks a configuration the node refuses.
	CodeInvalidConfig = "invalid_config"
	// CodePreflightFailed marks a preflight that refused the change.
	CodePreflightFailed = "preflight_failed"
	// CodeExternalFile marks a configuration that refers to a file the
	// agent may not read.
	CodeExternalFile = "external_file_invalid"
	// CodeConfigInvalid marks a configuration the Docker CLI refuses.
	CodeConfigInvalid = "compose_config_invalid"
	// CodePullFailed marks images the Docker CLI cannot pull.
	CodePullFailed = "compose_pull_failed"
	// CodeUpFailed marks a project the Docker CLI cannot start.
	CodeUpFailed = "compose_up_failed"
	// CodeInspectFailed marks containers the node cannot read.
	CodeInspectFailed = "compose_inspect_failed"
	// CodeExecutionUncertain marks a command that ran out of time.
	//
	// The daemon may still work on the change, so the outcome of the task
	// cannot be proven.
	CodeExecutionUncertain = "execution_uncertain"
	// CodeMappingAsserted marks a deployment that ran on a node whose work
	// root was asserted and not proven.
	CodeMappingAsserted = "mapping_asserted"
)

// namespacePattern matches the names of the projects of Compose.
//
// The name of the project is the namespace of the application, which the
// platform fixed when the application was created.
var namespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// servicePattern matches the names of the services of Compose.
var servicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ComposeArgs builds the argument list of a command of Compose.
//
// The list is fixed: the name of the project, the file of the configuration
// and the action. The arguments never reach a shell, so a value of a
// variable cannot become a command.
//
// The action is one of config, pull and up. The removal of the orphans is
// only asked for when the administrator confirmed the removal of the
// services.
func ComposeArgs(namespace, file, action string, removeOrphans bool) ([]string, error) {
	if !namespacePattern.MatchString(namespace) {
		return nil, fmt.Errorf("dockerops: %q is not a valid name of a Compose project", namespace)
	}
	if strings.TrimSpace(file) == "" {
		return nil, errors.New("dockerops: the file of the configuration is missing")
	}
	switch action {
	case ActionConfig, ActionPull, ActionUp:
	default:
		return nil, fmt.Errorf("dockerops: the action %q of Compose is not supported", action)
	}

	args := []string{"compose", "--project-name", namespace, "--file", file, action}
	switch action {
	case ActionConfig:
		// The configuration the CLI expands holds the values of the
		// variables of the application, so it is validated and not
		// printed.
		args = append(args, "--quiet")
	case ActionUp:
		args = append(args, "--detach")
		if removeOrphans {
			args = append(args, "--remove-orphans")
		}
	}
	return args, nil
}

// ContainerReader reads the containers of a Compose project.
type ContainerReader interface {
	// ProjectContainers returns the containers of the project with the
	// given name.
	ProjectContainers(ctx context.Context, namespace string) ([]protocol.Container, error)
}

// engine is the Docker engine the executor of Compose reads.
//
// The interface keeps the executor testable: a test replaces the engine and
// proves which commands run and in which order they run.
type engine interface {
	Inspector
	ContainerReader
	// Host returns the address of the Docker engine, so the command line
	// of Docker reaches the engine of the node as well.
	Host() string
}

// Compose is the executor of the applications that run with Docker Compose.
//
// The executor runs a fixed sequence of commands of the Docker CLI: it
// checks the configuration, pulls the images, starts the project and reads
// the actual state of the containers. It never stops a project, never
// removes a volume and never prunes the engine.
type Compose struct {
	// root is the work root of the agent.
	root string
	// runner runs the commands of the Docker CLI.
	runner Runner
	// engine reads the state of the Docker engine of the node.
	engine engine
}

// NewCompose builds the executor of the Compose applications.
//
// The work root is the directory the agent works in. The runner runs the
// commands of the Docker CLI and the engine reads the state of the
// containers of the node.
func NewCompose(root string, runner Runner, local *Local) Executor {
	executor := &Compose{root: root, runner: runner}
	if local != nil {
		executor.engine = local
	}
	return executor
}

// Execute deploys the application of the command.
//
// The order of the steps is fixed. The configuration is checked, then the
// Docker CLI validates it, then the images are pulled, then the project
// starts and the actual state of the containers decides whether the
// deployment succeeded. A step that fails ends the deployment before the
// project changes.
//
// The outcome of a deployment the administrator caused is a result and not an
// error. An error means that the executor could not tell what happened to
// the target.
func (c *Compose) Execute(ctx context.Context, cmd model.Command, output io.Writer) (model.Result, error) {
	if c.runner == nil {
		return model.Result{}, errors.New("dockerops: the executor of Compose has no runner")
	}
	if c.engine == nil {
		return model.Result{}, errors.New("dockerops: the executor of Compose has no Docker engine")
	}
	if output == nil {
		output = io.Discard
	}
	if cmd.App.Mode != yamlspec.ModeCompose {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: fmt.Sprintf("应用模式 %q 不是 Compose", cmd.App.Mode),
		}, nil
	}

	// The configuration is checked again on the node. The control端 checked
	// it before it sent the command, but the node deploys what it holds.
	checked, err := yamlspec.Check(cmd.App, cmd.Config)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    configErrorCode(err),
			Message: err.Error(),
		}, nil
	}

	// The mapping of the work root has to be proven before anything
	// changes: the relative path of a configuration is resolved against the
	// directory of the application, and the directory of the host has to be
	// the one the agent works in.
	proof, err := c.proveMapping(ctx)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: err.Error(),
		}, nil
	}
	if proof.asserted {
		// The mapping of the work root is only asserted and not proven, so
		// the place of the host a relative bind points at is unknown. The
		// configuration is refused before a single command runs.
		if err := CheckRelativeBinds(checked.RenderedYAML); err != nil {
			return model.Result{
				State:   model.Failed,
				Code:    CodePreflightFailed,
				Message: err.Error() + "；工作目录的路径映射只有断言，没有自证，不能使用相对绑定",
			}, nil
		}
	}

	workspace, err := PrepareWorkspace(c.root, cmd.App.ID)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: err.Error(),
		}, nil
	}
	if err := workspace.WriteConfig(checked.RenderedYAML); err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: err.Error(),
		}, nil
	}
	if err := CheckExternalFiles(checked.RenderedYAML, workspace.Dir); err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodeExternalFile,
			Message: err.Error(),
		}, nil
	}

	// The removals are checked against the configuration of this run and
	// against the containers of the project: a service that the
	// configuration still holds is never removed, and a container that
	// nobody confirmed is never removed either.
	removeOrphans, err := c.removalsConfirmed(ctx, cmd, checked)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: err.Error(),
		}, nil
	}

	execution, cancel := executionContext(ctx, cmd.ExecutionTimeout)
	defer cancel()

	// The Docker CLI validates the configuration itself before anything is
	// pulled or started.
	if err := c.run(execution, cmd.App.Namespace, workspace, ActionConfig, false, output); err != nil {
		return mappingTrace(failureOf(execution, CodeConfigInvalid, "Compose 配置校验未通过", err), proof), nil
	}
	// The images are pulled before the project starts: an image that cannot
	// be pulled is found before the containers of the application change.
	if err := c.run(execution, cmd.App.Namespace, workspace, ActionPull, false, output); err != nil {
		return mappingTrace(failureOf(execution, CodePullFailed, "镜像拉取失败", err), proof), nil
	}
	startErr := c.run(execution, cmd.App.Namespace, workspace, ActionUp, removeOrphans, output)

	// The state of the containers decides whether the deployment succeeded,
	// not the exit code of the command line: a project can start and a
	// service of it can fail.
	result := c.verify(ctx, cmd, checked)
	if startErr != nil {
		if interrupted(execution, startErr) {
			return mappingTrace(model.Result{
				State:   model.Uncertain,
				Code:    CodeExecutionUncertain,
				Message: "命令没有在期限内结束，目标可能仍在变更",
			}, proof), nil
		}
		result.State = model.Failed
		result.Code = CodeUpFailed
		result.Message = "Compose 启动失败：" + startErr.Error()
		// The project may have changed before the command failed, so the
		// application may be in a mixed state. The platform never stops the
		// project to undo the change: the update is not a transaction.
		result.PartialUpdate = true
		return mappingTrace(result, proof), nil
	}
	if result.State != model.Succeeded {
		// The project started, so the target changed even when a service of
		// it did not reach the expected state.
		result.PartialUpdate = true
	}
	return mappingTrace(result, proof), nil
}

// mappingProof is the proof of the mapping of the work root of the agent.
type mappingProof struct {
	// asserted reports that the operator of the node asserted the mapping
	// instead of proving it.
	asserted bool
	// hostPath is the path of the work root on the host as the operator
	// stated it.
	hostPath string
}

// mappingTrace writes the assertion of the operator of the node into the
// outcome of the deployment.
//
// The deployment ran on a work root whose mapping was asserted and not
// proven, so the outcome carries a code of its own: the history of the
// deployments and the audit show that the mapping of this deployment was
// never proven. A failed deployment keeps the code of its failure, the note
// names the assertion anyway.
func mappingTrace(result model.Result, proof mappingProof) model.Result {
	if !proof.asserted {
		return result
	}
	note := fmt.Sprintf("工作目录的路径映射由节点运维方断言（宿主机路径 %s），没有自证", proof.hostPath)
	if result.Message == "" {
		result.Message = note
	} else {
		result.Message = result.Message + "；" + note
	}
	if result.State == model.Succeeded {
		result.Code = CodeMappingAsserted
	}
	return result
}

// proveMapping proves that the work root of the agent is the same path on
// the host as it is in the container of the agent.
//
// The proof reads the mounts of the container of the agent. When the agent
// does not run in a container there is no container to inspect, so the
// operator of the node may assert the mapping instead. The assertion has to
// name the path of the work root on the host, and it is only accepted when
// the agent really runs outside of a container: an agent in a container has
// to prove the mapping by itself.
func (c *Compose) proveMapping(ctx context.Context) (mappingProof, error) {
	assertion, asserted, err := ParseMappingAssertion(os.Getenv(MappingEnv))
	if err != nil {
		return mappingProof{}, err
	}
	if !asserted {
		if err := ProveMapping(ctx, c.engine, SelfContainerID(), c.root); err != nil {
			return mappingProof{}, err
		}
		return mappingProof{}, nil
	}

	// The assertion has to name the path the agent works in. An assertion
	// about another path proves nothing.
	if !samePath(filepath.Clean(assertion.HostPath), filepath.Clean(c.root)) {
		return mappingProof{}, fmt.Errorf(
			"dockerops: 断言的路径 %q 与工作目录 %q 不一致，变更操作已禁用",
			assertion.HostPath, c.root)
	}
	// An agent that runs in a container can inspect itself, so it proves
	// the mapping by itself. The assertion is only for the nodes whose
	// agent runs outside of a container.
	if SelfContainerID() != "" {
		return mappingProof{}, errors.New(
			"dockerops: Agent 在容器内必须自证映射，不能用断言，变更操作已禁用")
	}
	return mappingProof{asserted: true, hostPath: assertion.HostPath}, nil
}

// run runs one command of the Docker CLI.
//
// The command runs in the directory of the application, so the relative
// paths of the configuration are resolved against it.
func (c *Compose) run(ctx context.Context, namespace string, workspace Workspace, action string, removeOrphans bool, output io.Writer) error {
	args, err := ComposeArgs(namespace, workspace.File, action, removeOrphans)
	if err != nil {
		return err
	}
	program := DockerProgram()
	spec := RunSpec{
		Program: program,
		Dir:     workspace.Dir,
		Args:    args,
		Env:     ToolEnv(program, c.root, c.engine.Host()),
	}
	return c.runner.Run(ctx, spec, output)
}

// verify waits for the services of the application to become ready.
//
// The state of the containers is read again and again: the command line
// reports that a project started, not that its services are ready.
func (c *Compose) verify(ctx context.Context, cmd model.Command, checked model.CheckedConfig) model.Result {
	var deadline time.Time
	if cmd.VerifyTimeout > 0 {
		deadline = time.Now().Add(cmd.VerifyTimeout)
	}
	for {
		containers, err := c.engine.ProjectContainers(ctx, cmd.App.Namespace)
		if err != nil {
			return model.Result{
				State:   model.Failed,
				Code:    CodeInspectFailed,
				Message: "读取项目容器失败：" + err.Error(),
			}
		}
		result := CheckComposeReadiness(checked.Services, checked.OneShotServices, containers)
		if result.State == model.Succeeded || result.Code != CodeComposeNotReady {
			return result
		}
		if deadline.IsZero() || !time.Now().Before(deadline) {
			return result
		}
		// The wait never lasts longer than the deadline of the task.
		wait := pollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return model.Result{
				State:   model.Uncertain,
				Code:    CodeExecutionUncertain,
				Message: "等待服务就绪被中断，目标状态未知",
			}
		case <-time.After(wait):
		}
	}
}

// pollInterval is the time the executor waits between two readings of the
// state of the containers.
const pollInterval = 2 * time.Second

// executionContext bounds the commands of the Docker CLI.
//
// A command that runs out of time may still work in the daemon, so the
// outcome of such a command is uncertain.
func executionContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// failureOf turns the error of a command into the outcome of the task.
func failureOf(ctx context.Context, code, message string, err error) model.Result {
	if interrupted(ctx, err) {
		return model.Result{
			State:   model.Uncertain,
			Code:    CodeExecutionUncertain,
			Message: "命令没有在期限内结束，目标可能仍在变更",
		}
	}
	return model.Result{
		State:   model.Failed,
		Code:    code,
		Message: message + ": " + err.Error(),
	}
}

// interrupted reports whether a command was stopped before it ended.
//
// A command that was stopped may still work in the daemon, so the outcome
// of the task cannot be proven and the task waits for a check.
func interrupted(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded) ||
		errors.Is(ctx.Err(), context.Canceled)
}

// removalsConfirmed reports whether the removals of the command are allowed.
//
// A service is only removed when the administrator confirmed the removal,
// and the confirmation is checked against the configuration of this run: a
// service that the configuration still holds is never removed.
//
// The scope of the removal is checked against the containers of the project.
// The Docker CLI removes every container of the project that the new
// configuration does not hold, so the orphans of the project have to be
// exactly the services the administrator confirmed. An orphan that nobody
// confirmed -- the remains of a failed deployment, a container that was
// started by hand with the label of the project or a replica of a scaled
// service -- would be deleted without a word, so the deployment is refused
// before a single command runs.
func (c *Compose) removalsConfirmed(ctx context.Context, cmd model.Command, checked model.CheckedConfig) (bool, error) {
	if len(cmd.RemovedServices) == 0 {
		// Nothing was confirmed, so nothing is removed: the command line of
		// Compose does not ask for the removal of the orphans and the
		// containers of the project stay where they are.
		return false, nil
	}
	current := make(map[string]bool, len(checked.Services))
	for _, name := range checked.Services {
		current[name] = true
	}
	confirmed := make(map[string]bool, len(cmd.RemovedServices))
	for _, name := range cmd.RemovedServices {
		if !servicePattern.MatchString(name) {
			return false, fmt.Errorf("dockerops: 被删除的服务名 %q 不合法", name)
		}
		if current[name] {
			return false, fmt.Errorf("dockerops: 服务 %q 仍在本次配置中，不能删除", name)
		}
		confirmed[name] = true
	}

	orphans, err := c.orphanServices(ctx, cmd.App.Namespace, checked.Services)
	if err != nil {
		return false, err
	}
	if err := sameRemovals(orphans, confirmed); err != nil {
		return false, err
	}
	return true, nil
}

// orphanServices returns the services of the project that the configuration
// of this run does not hold.
//
// The containers are the ones that carry the label of the project, and the
// service of a container is named by the label of the service, so both sides
// speak about the names of the services of Compose.
func (c *Compose) orphanServices(ctx context.Context, namespace string, services []string) ([]string, error) {
	containers, err := c.engine.ProjectContainers(ctx, namespace)
	if err != nil {
		return nil, fmt.Errorf("dockerops: 读取项目 %s 的容器失败: %w", namespace, err)
	}
	current := make(map[string]bool, len(services))
	for _, name := range services {
		current[name] = true
	}
	seen := map[string]bool{}
	var orphans []string
	for _, item := range containers {
		if current[item.Service] || seen[item.Service] {
			continue
		}
		seen[item.Service] = true
		orphans = append(orphans, item.Service)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// sameRemovals compares the orphans of the project with the removals the
// administrator confirmed.
//
// The comparison does not care about the order of the two sets, but the two
// sets have to be the same: an orphan that nobody confirmed and a confirmed
// service without a container are both a mismatch.
func sameRemovals(orphans []string, confirmed map[string]bool) error {
	orphanSet := make(map[string]bool, len(orphans))
	for _, name := range orphans {
		orphanSet[name] = true
	}

	var unconfirmed []string
	for _, name := range orphans {
		if !confirmed[name] {
			unconfirmed = append(unconfirmed, name)
		}
	}
	var missing []string
	for name := range confirmed {
		if !orphanSet[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	switch {
	case len(unconfirmed) > 0 && len(missing) > 0:
		return fmt.Errorf(
			"dockerops: 项目的孤儿服务 %s 没有经过确认，已确认的删除服务 %s 在项目里没有容器，拒绝删除",
			namesOf(unconfirmed), namesOf(missing))
	case len(unconfirmed) > 0:
		return fmt.Errorf(
			"dockerops: 项目的孤儿服务 %s 没有经过确认，拒绝删除",
			namesOf(unconfirmed))
	case len(missing) > 0:
		return fmt.Errorf(
			"dockerops: 已确认的删除服务 %s 在项目里没有容器，拒绝删除",
			namesOf(missing))
	}
	return nil
}

// namesOf formats the names of the services for a message.
//
// A container of the project that belongs to no service has no name, so it
// is told apart from a service of the configuration.
func namesOf(names []string) string {
	formatted := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			formatted = append(formatted, "(没有服务名的容器)")
			continue
		}
		formatted = append(formatted, name)
	}
	return strings.Join(formatted, ", ")
}

// configErrorCode returns the code of a failed check.
func configErrorCode(err error) string {
	var failure *yamlspec.Error
	if errors.As(err, &failure) && failure.Code != "" {
		return failure.Code
	}
	return CodeInvalidConfig
}
