package dockerops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	swarmapi "github.com/docker/docker/api/types/swarm"
	"gopkg.in/yaml.v3"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
	"hmoob-dock/internal/yamlspec"
)

// The codes of the outcomes of the executor of Swarm.
const (
	// CodeSwarmNotManager marks a node that is not a manager of the
	// cluster of the application.
	CodeSwarmNotManager = "swarm_not_manager"
	// CodeSwarmClusterMismatch marks a connection to another cluster than
	// the one of the application.
	CodeSwarmClusterMismatch = "swarm_cluster_mismatch"
	// CodeSwarmNoQuorum marks a cluster that cannot accept a change.
	CodeSwarmNoQuorum = "swarm_no_quorum"
	// CodeStackConfigInvalid marks a configuration the Docker CLI refuses.
	CodeStackConfigInvalid = "stack_config_invalid"
	// CodeStackPullFailed marks images the Docker CLI cannot pull.
	CodeStackPullFailed = "stack_pull_failed"
	// CodeStackDeployFailed marks a stack the Docker CLI cannot deploy.
	CodeStackDeployFailed = "stack_deploy_failed"
	// CodeStackInspectFailed marks a cluster the node cannot read.
	CodeStackInspectFailed = "stack_inspect_failed"
	// CodeStackPruneRefused marks a removal of a service nobody confirmed.
	CodeStackPruneRefused = "stack_prune_refused"
)

// SwarmVersionLabel is the label the platform puts on every service of a
// stack.
//
// The label holds the digest of the definition of the service, so the
// service that runs can be told apart from the service of an older version.
// The specification of a service cannot be compared with the document of the
// deployment field by field: the command line of Docker resolves the images,
// applies the defaults of the cluster and converts the ports and the mounts,
// so the fields of the two sides do not have the same shape. The label is
// part of the specification of the service, so a service that does not carry
// the digest of this deployment does not run the target of this deployment.
//
// The digest is the evidence that the platform wrote the specification of the
// service. It is not the only evidence: a change of the cluster that was
// made by hand keeps the labels of the service, so the fields the
// configuration determines are compared with the cluster as well.
const SwarmVersionLabel = "com.hmoob-dock.version"

// The modes a service of a stack may run in.
const (
	// modeReplicated marks a service that runs a number of replicas.
	modeReplicated = "replicated"
	// modeGlobal marks a service that runs one task on every node that may
	// run it.
	modeGlobal = "global"
)

// swarmEngine reads the facts of the Swarm cluster of the node.
//
// The interface keeps the executor testable: a test replaces the engine and
// proves what the executor does with the facts of the cluster.
type swarmEngine interface {
	// Identity returns the identity of the local Docker engine.
	Identity(ctx context.Context) (protocol.NodeIdentity, error)
	// SwarmNodes returns the nodes of the cluster.
	SwarmNodes(ctx context.Context) ([]swarmapi.Node, error)
	// SwarmServices returns the services of the stack of the namespace.
	SwarmServices(ctx context.Context, namespace string) ([]swarmapi.Service, error)
	// SwarmTasks returns the tasks of the stack of the namespace.
	SwarmTasks(ctx context.Context, namespace string) ([]swarmapi.Task, error)
	// Host returns the address of the Docker engine.
	Host() string
}

// Swarm is the executor of the applications that run as a Docker Stack.
//
// The executor updates a fixed stack and reads the actual state of the
// cluster afterwards. The exit code of the command line of Docker reports
// that a stack was written, not that its services run the version that was
// deployed, so the state of the cluster decides whether the deployment
// succeeded.
type Swarm struct {
	// root is the work root of the agent.
	root string
	// runner runs the commands of the Docker CLI.
	runner Runner
	// engine reads the facts of the cluster.
	engine swarmEngine
}

// NewSwarm builds the executor of the Swarm applications.
//
// The work root is the directory the agent works in. The runner runs the
// commands of the Docker CLI and the engine reads the facts of the cluster.
func NewSwarm(root string, runner Runner, local *Local) Executor {
	executor := &Swarm{root: root, runner: runner}
	if local != nil {
		executor.engine = local
	}
	return executor
}

// StackArgs builds the argument list of a command of a Swarm stack.
//
// The list is fixed: the action, the file of the configuration, the flags of
// the update and the name of the stack. The arguments never reach a shell,
// so a value of a variable cannot become a command.
//
// The prune flag is only asked for when the administrator confirmed the
// removal of the services of the stack that the configuration does not hold
// any more. The command line of Docker prunes the services of the stack of
// the command and of no other stack.
func StackArgs(namespace, file string, prune bool) ([]string, error) {
	return stackArgs(namespace, file, prune, false)
}

// stackArgs builds the argument list of the update of a stack.
//
// The credentials of the registry of the manager are only handed to the
// cluster when the stack needs them, so the flag is not part of the exported
// function.
func stackArgs(namespace, file string, prune, registryAuth bool) ([]string, error) {
	if !namespacePattern.MatchString(namespace) {
		return nil, fmt.Errorf("dockerops: %q is not a valid name of a Swarm stack", namespace)
	}
	if strings.TrimSpace(file) == "" {
		return nil, errors.New("dockerops: the file of the configuration is missing")
	}

	args := []string{"stack", "deploy", "--compose-file", file, "--resolve-image", "always"}
	if registryAuth {
		args = append(args, "--with-registry-auth")
	}
	if prune {
		args = append(args, "--prune")
	}
	args = append(args, namespace)
	return args, nil
}

// Execute deploys the application of the command.
//
// The order of the steps is fixed. The identity of the node decides whether
// the stack may be updated at all, then the configuration is checked, then
// the Docker CLI validates it, then the images are prepared, then the stack
// is updated and the actual state of the cluster decides whether the
// deployment succeeded.
//
// The outcome of a deployment the administrator caused is a result and not an
// error. An error means that the executor could not tell what happened to the
// target.
func (s *Swarm) Execute(ctx context.Context, cmd model.Command, output io.Writer) (model.Result, error) {
	if s.runner == nil {
		return model.Result{}, errors.New("dockerops: the executor of Swarm has no runner")
	}
	if s.engine == nil {
		return model.Result{}, errors.New("dockerops: the executor of Swarm has no Docker engine")
	}
	if output == nil {
		output = io.Discard
	}
	if cmd.App.Mode != yamlspec.ModeSwarm {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: fmt.Sprintf("应用模式 %q 不是 Swarm", cmd.App.Mode),
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

	// The identity of the connection decides whether this node may update
	// the stack at all. The role comes from the Docker engine of the node
	// and not from a report of the agent: a node that lost its role may not
	// run an instruction of a cluster it does not manage any more, and the
	// platform never asks another agent to run it instead.
	identity, err := s.engine.Identity(ctx)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodeStackInspectFailed,
			Message: "读取节点身份失败：" + err.Error(),
		}, nil
	}
	if result, ok := checkIdentity(cmd.App, identity); !ok {
		return result, nil
	}

	nodes, err := s.engine.SwarmNodes(ctx)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodeStackInspectFailed,
			Message: "读取集群节点失败：" + err.Error(),
		}, nil
	}
	if result, ok := checkQuorum(nodes, identity); !ok {
		return result, nil
	}

	// The workspace of the application holds the configuration of this run.
	// Every service of the stack carries the digest of its own definition,
	// so the platform can tell the version that runs from an older one.
	workspace, err := PrepareWorkspace(s.root, cmd.App.ID)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: err.Error(),
		}, nil
	}
	document, err := prepareStack(checked.RenderedYAML)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: err.Error(),
		}, nil
	}
	if err := workspace.WriteConfig(document.Payload); err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: err.Error(),
		}, nil
	}
	if err := CheckExternalFiles(document.Payload, workspace.Dir); err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodeExternalFile,
			Message: err.Error(),
		}, nil
	}
	// The cluster resolves a relative path of a bind on the node that runs
	// the task, so the path would point at another place on every node of
	// the cluster. The manager cannot prove the mapping of the work root of
	// the agent for the workers, so a relative bind is refused.
	if err := CheckRelativeBinds(document.Payload); err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodePreflightFailed,
			Message: err.Error() + "；Swarm 的相对路径由运行任务的节点解析，平台拒绝相对绑定",
		}, nil
	}

	// The services of the stack that the configuration does not hold any
	// more are only removed when the administrator confirmed exactly these
	// services.
	prune, err := s.confirmedRemovals(ctx, cmd, checked)
	if err != nil {
		return model.Result{
			State:   model.Failed,
			Code:    CodeStackPruneRefused,
			Message: err.Error(),
		}, nil
	}

	execution, cancel := executionContext(ctx, cmd.ExecutionTimeout)
	defer cancel()

	// The Docker CLI validates the configuration itself before anything is
	// pulled or deployed.
	if err := s.runConfig(execution, workspace); err != nil {
		return failureOf(execution, CodeStackConfigInvalid, "Stack 配置校验未通过", err), nil
	}

	// The images are prepared before the stack changes: an image that cannot
	// be pulled is found before the services of the stack change. The
	// workers of the cluster pull the images themselves, so a worker that
	// cannot pull is found in the state of its tasks.
	for _, image := range document.Images {
		if err := s.pull(execution, workspace, image, output); err != nil {
			return failureOf(execution, CodeStackPullFailed, "镜像拉取失败", err), nil
		}
	}

	// The stack is updated. The credentials of the registry of the manager
	// are only handed to the cluster when the stack needs them.
	registryAuth := needsRegistryAuth(document.Images)
	deployErr := s.deploy(execution, cmd.App.Namespace, workspace, prune, registryAuth, output)

	// The state of the cluster decides whether the deployment succeeded, not
	// the exit code of the command line.
	result := s.verify(ctx, cmd, document)
	if note := hostPathNote(document.Paths, nodes, identity); note != "" {
		result.Message = joinNote(result.Message, note)
	}
	if deployErr != nil {
		if interrupted(execution, deployErr) {
			return model.Result{
				State:   model.Uncertain,
				Code:    CodeExecutionUncertain,
				Message: "命令没有在期限内结束，目标可能仍在变更",
			}, nil
		}
		result.State = model.Failed
		result.Code = CodeStackDeployFailed
		result.Message = "Stack 部署失败：" + deployErr.Error()
		// The stack may have changed before the command failed, so the
		// application may be in a mixed state.
		result.PartialUpdate = true
		return result, nil
	}
	if result.State != model.Succeeded {
		// The stack was written, so the target changed even when a service
		// of it did not reach the expected state.
		result.PartialUpdate = true
	}
	return result, nil
}

// checkIdentity makes sure that the connection of the node matches the
// cluster of the application and that the node is still a manager of it.
//
// The role comes from the Docker engine of the node. A node that was demoted
// may not run the instructions of a cluster it does not manage any more, and
// the platform never hands the instruction to another agent: the instruction
// belongs to the task of this node.
func checkIdentity(app model.App, identity protocol.NodeIdentity) (model.Result, bool) {
	if identity.SwarmID == "" {
		return model.Result{
			State:   model.Failed,
			Code:    CodeSwarmNotManager,
			Message: "节点不属于任何 Swarm 集群，不能更新 Stack",
		}, false
	}
	if identity.SwarmID != app.TargetID {
		return model.Result{
			State:   model.Failed,
			Code:    CodeSwarmClusterMismatch,
			Message: "当前连接的集群与所选目标集群不一致，拒绝更新 Stack",
		}, false
	}
	if identity.Role != protocol.RoleManager {
		return model.Result{
			State:   model.Failed,
			Code:    CodeSwarmNotManager,
			Message: "当前节点已不是集群的 Manager，拒绝更新 Stack",
		}, false
	}
	return model.Result{}, true
}

// checkQuorum makes sure that the cluster may accept a change.
//
// A cluster without a leader cannot accept the update of a service, and a
// manager that cannot reach the other managers cannot ask them for one. The
// platform refuses the change in both cases, so the administrator sees the
// reason instead of a timeout.
func checkQuorum(nodes []swarmapi.Node, identity protocol.NodeIdentity) (model.Result, bool) {
	leader := false
	for _, node := range nodes {
		if node.Spec.Role != swarmapi.NodeRoleManager {
			continue
		}
		if node.ManagerStatus != nil && node.ManagerStatus.Leader {
			leader = true
		}
		if node.ID != identity.SwarmNodeID {
			continue
		}
		if node.ManagerStatus != nil && node.ManagerStatus.Reachability == swarmapi.ReachabilityUnreachable {
			return model.Result{
				State:   model.Failed,
				Code:    CodeSwarmNoQuorum,
				Message: "当前 Manager 无法连接集群，集群没有法定人数",
			}, false
		}
	}
	if !leader {
		return model.Result{
			State:   model.Failed,
			Code:    CodeSwarmNoQuorum,
			Message: "集群没有 Leader（没有法定人数），拒绝更新 Stack",
		}, false
	}
	return model.Result{}, true
}

// confirmedRemovals reports whether the services of the stack that the
// configuration does not hold any more may be removed.
//
// The scope of the removal is the stack of the application, so the services
// of another stack are never part of it. The removal only runs when the
// administrator confirmed exactly the services that are left over, so a
// service nobody confirmed stays where it is.
func (s *Swarm) confirmedRemovals(ctx context.Context, cmd model.Command, checked model.CheckedConfig) (bool, error) {
	services, err := s.engine.SwarmServices(ctx, cmd.App.Namespace)
	if err != nil {
		return false, fmt.Errorf("dockerops: 读取 Stack %s 的服务失败: %w", cmd.App.Namespace, err)
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

	leftovers := leftoverServices(services, cmd.App.Namespace, checked.Services)
	if len(leftovers) == 0 && len(confirmed) == 0 {
		// Nothing is left over and nothing was confirmed, so no service of
		// the stack is removed.
		return false, nil
	}
	if err := sameRemovals(leftovers, confirmed); err != nil {
		return false, err
	}
	return true, nil
}

// leftoverServices returns the services of the stack that the configuration
// of this run does not hold any more.
//
// The name of a service of a stack is the name of the stack, an underscore
// and the name of the service, so the name of the service is what follows
// the namespace.
func leftoverServices(services []swarmapi.Service, namespace string, configured []string) []string {
	current := make(map[string]bool, len(configured))
	for _, name := range configured {
		current[name] = true
	}

	seen := map[string]bool{}
	var leftovers []string
	for _, service := range services {
		name := serviceName(service, namespace)
		if name == "" || current[name] || seen[name] {
			continue
		}
		seen[name] = true
		leftovers = append(leftovers, name)
	}
	sort.Strings(leftovers)
	return leftovers
}

// serviceName returns the name of a service inside of its stack.
func serviceName(service swarmapi.Service, namespace string) string {
	name := service.Spec.Name
	if namespace != "" {
		name = strings.TrimPrefix(name, namespace+"_")
	}
	return name
}

// runConfig runs the validation of the configuration by the Docker CLI.
//
// The output of the command holds the expanded configuration with the values
// of the variables, so it never reaches the output of the task. The errors
// of the command are the diagnostics of the validation, and they do not hold
// a value of a variable.
func (s *Swarm) runConfig(ctx context.Context, workspace Workspace) error {
	program := DockerProgram()
	spec := RunSpec{
		Program: program,
		Dir:     workspace.Dir,
		Args:    []string{"stack", "config", "--compose-file", workspace.File},
		Env:     ToolEnv(program, s.root, s.engine.Host()),
	}
	buffer := &bytes.Buffer{}
	if err := s.runner.Run(ctx, spec, buffer); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(limitText(buffer.String(), 512)))
	}
	return nil
}

// pull prepares an image on the manager.
//
// The image is pulled before the stack changes, so an image that cannot be
// pulled is found before the services of the stack change. The workers pull
// the images themselves, so a worker that cannot pull is found in the state
// of its tasks.
func (s *Swarm) pull(ctx context.Context, workspace Workspace, image string, output io.Writer) error {
	program := DockerProgram()
	spec := RunSpec{
		Program: program,
		Dir:     workspace.Dir,
		Args:    []string{"pull", image},
		Env:     ToolEnv(program, s.root, s.engine.Host()),
	}
	return s.runner.Run(ctx, spec, output)
}

// deploy updates the stack of the application.
func (s *Swarm) deploy(ctx context.Context, namespace string, workspace Workspace, prune, registryAuth bool, output io.Writer) error {
	args, err := stackArgs(namespace, workspace.File, prune, registryAuth)
	if err != nil {
		return err
	}
	program := DockerProgram()
	spec := RunSpec{
		Program: program,
		Dir:     workspace.Dir,
		Args:    args,
		Env:     ToolEnv(program, s.root, s.engine.Host()),
	}
	return s.runner.Run(ctx, spec, output)
}

// verify waits for the services of the stack to reach the expected state.
//
// The state of the cluster is read again and again: the command line reports
// that a stack was written, not that its services run the version that was
// deployed.
func (s *Swarm) verify(ctx context.Context, cmd model.Command, document stackDocument) model.Result {
	var deadline time.Time
	if cmd.VerifyTimeout > 0 {
		deadline = time.Now().Add(cmd.VerifyTimeout)
	}
	for {
		services, err := s.engine.SwarmServices(ctx, cmd.App.Namespace)
		if err != nil {
			return model.Result{
				State:   model.Failed,
				Code:    CodeStackInspectFailed,
				Message: "读取 Stack 服务失败：" + err.Error(),
			}
		}
		tasks, err := s.engine.SwarmTasks(ctx, cmd.App.Namespace)
		if err != nil {
			return model.Result{
				State:   model.Failed,
				Code:    CodeStackInspectFailed,
				Message: "读取 Stack 任务失败：" + err.Error(),
			}
		}
		nodes, err := s.engine.SwarmNodes(ctx)
		if err != nil {
			return model.Result{
				State:   model.Failed,
				Code:    CodeStackInspectFailed,
				Message: "读取集群节点失败：" + err.Error(),
			}
		}

		observations, missing, err := observe(services, tasks, nodes, cmd.App.Namespace, document)
		if err != nil {
			var mismatch *specMismatch
			if errors.As(err, &mismatch) {
				return model.Result{
					State:   model.Failed,
					Code:    CodeSwarmSpecMismatch,
					Message: mismatch.Error(),
				}
			}
			return model.Result{
				State:   model.Failed,
				Code:    CodeSwarmTargetUnknown,
				Message: err.Error(),
			}
		}
		if len(missing) > 0 {
			return model.Result{
				State:   model.Failed,
				Code:    CodeStackInspectFailed,
				Message: "Stack 中不存在服务：" + strings.Join(missing, ", "),
			}
		}

		result := CheckSwarmReadiness(observations)
		if result.State == model.Succeeded || !swarmWaits(result.Code) {
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
				Message: "等待服务收敛被中断，目标状态未知",
			}
		case <-time.After(wait):
		}
	}
}

// observe turns the facts of the cluster into the observations of the
// services of the stack.
//
// The observation of a service holds the number of the tasks of the current
// specification that run: a task of an older specification is a task of the
// version before this deployment, so it does not count even when it still
// runs.
//
// The target of the observation comes from the configuration of this
// deployment and not from the cluster: a cluster that was changed by hand
// never reached the target of this deployment. The specification of the
// cluster is compared with the configuration first, so a service that runs
// another specification is reported instead of being counted.
func observe(
	services []swarmapi.Service,
	tasks []swarmapi.Task,
	nodes []swarmapi.Node,
	namespace string,
	document stackDocument,
) ([]ServiceObservation, []string, error) {
	byName := map[string]swarmapi.Service{}
	for _, service := range services {
		byName[serviceName(service, namespace)] = service
	}

	names := make([]string, 0, len(document.Digests))
	for name := range document.Digests {
		names = append(names, name)
	}
	sort.Strings(names)

	var observations []ServiceObservation
	var missing []string
	for _, name := range names {
		service, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		target := document.Targets[name]
		if !rolledBackState(updateStateOf(service)) {
			// A service the cluster rolled back runs the specification of
			// the version before this deployment, so the difference is
			// expected and the rollback is reported instead of a
			// difference of the specification.
			if matches, reason := specMatches(target, service); !matches {
				return nil, nil, &specMismatch{service: name, reason: reason}
			}
		}
		desired, known := configuredTarget(target, nodes)
		if !known {
			return nil, nil, fmt.Errorf(
				"dockerops: 服务 %s 的目标无法核实，placement 约束无法读取", name)
		}
		running, failed := taskStates(tasks, service)
		observations = append(observations, ServiceObservation{
			Name:              name,
			UpdateState:       updateStateOf(service),
			Desired:           desired,
			Running:           running,
			FailedTasks:       failed,
			CurrentSpecDigest: service.Spec.Labels[SwarmVersionLabel],
			TargetSpecDigest:  document.Digests[name],
		})
	}
	return observations, missing, nil
}

// specMismatch marks a service whose specification of the cluster is not the
// specification the configuration of this deployment asks for.
type specMismatch struct {
	// service is the name of the service inside of the stack.
	service string
	// reason describes the difference between the two sides.
	reason string
}

// Error returns the message of the difference.
func (m *specMismatch) Error() string {
	return fmt.Sprintf("服务 %s 的规格与本次部署的配置不符：%s", m.service, m.reason)
}

// specMatches reports whether the specification of the cluster is the
// specification the configuration of this deployment asks for.
//
// The digest of the version is not enough: a change of the cluster that was
// made by hand keeps the labels of the service, so the digest of the
// version still names this deployment. The fields the configuration
// determines are compared as well: the mode of the service, the number of
// its replicas and the repository of its image.
//
// The image is compared by its repository, because the configuration names
// it with a tag and the cluster holds it with the digest the registry
// answered with.
func specMatches(target stackTarget, service swarmapi.Service) (bool, string) {
	mode := modeOf(service)
	if mode == "" {
		mode = modeReplicated
	}
	if mode != target.Mode {
		return false, fmt.Sprintf("集群中的模式是 %q，配置要求 %q", mode, target.Mode)
	}
	if target.Mode == modeReplicated {
		if replicas := replicasOf(service); replicas != target.Replicas {
			return false, fmt.Sprintf("集群中的副本数是 %d，配置要求 %d", replicas, target.Replicas)
		}
	}
	repository := imageRepository(target.Image)
	if other := imageRepository(imageOfService(service)); repository != other {
		return false, fmt.Sprintf("集群中的镜像 %q 与配置要求的 %q 不同", other, repository)
	}
	return true, ""
}

// taskStates returns the tasks of the current specification of a service that
// run and the ones that failed.
//
// The tasks of an older specification are the tasks of the version before
// this deployment: they do not count, even when they still run. The cluster
// keeps them until it shut them down, so the platform tells them apart by
// the image and the environment of their specification.
func taskStates(tasks []swarmapi.Task, service swarmapi.Service) (int, []string) {
	image := ""
	var env []string
	if service.Spec.TaskTemplate.ContainerSpec != nil {
		image = service.Spec.TaskTemplate.ContainerSpec.Image
		env = service.Spec.TaskTemplate.ContainerSpec.Env
	}

	latest := map[string]swarmapi.Task{}
	for _, task := range tasks {
		if task.ServiceID != service.ID {
			continue
		}
		if !taskMatches(task, image, env) {
			continue
		}
		key := taskKey(task, service)
		previous, seen := latest[key]
		if !seen || newerTask(task, previous) {
			latest[key] = task
		}
	}

	running := 0
	var failed []string
	for _, task := range latest {
		switch task.Status.State {
		case swarmapi.TaskStateRunning:
			running++
		case swarmapi.TaskStateFailed, swarmapi.TaskStateRejected:
			failed = append(failed, describeTask(task))
		}
	}
	sort.Strings(failed)
	return running, failed
}

// taskMatches reports whether a task runs the specification of its service.
//
// The image and the environment are the parts of the specification that tell
// the task of the current version apart from the task of the version before
// it.
func taskMatches(task swarmapi.Task, image string, env []string) bool {
	if task.Spec.ContainerSpec == nil {
		return false
	}
	if task.Spec.ContainerSpec.Image != image {
		return false
	}
	return sameEnvironment(task.Spec.ContainerSpec.Env, env)
}

// sameEnvironment reports whether two lists of variables hold the same
// values.
func sameEnvironment(first, second []string) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

// taskKey returns the key that tells the tasks of one position of a service
// apart.
//
// A replicated service holds one task per slot, a global service one task
// per node.
func taskKey(task swarmapi.Task, service swarmapi.Service) string {
	if service.Spec.Mode.Global != nil {
		return task.NodeID
	}
	return fmt.Sprintf("slot-%d", task.Slot)
}

// newerTask reports whether a task is the newer one of two tasks.
func newerTask(candidate, previous swarmapi.Task) bool {
	if candidate.CreatedAt.After(previous.CreatedAt) {
		return true
	}
	if candidate.CreatedAt.Before(previous.CreatedAt) {
		return false
	}
	return candidate.ID > previous.ID
}

// describeTask describes a task that failed.
func describeTask(task swarmapi.Task) string {
	if task.Status.Err != "" {
		return fmt.Sprintf("%s（%s）", task.ID, task.Status.Err)
	}
	return fmt.Sprintf("%s（%s）", task.ID, task.Status.State)
}

// stackDocument is the document of a stack that the platform deploys.
type stackDocument struct {
	// Payload is the document the Docker CLI reads.
	Payload []byte
	// Digests are the digests of the definitions of the services, by name.
	Digests map[string]string
	// Targets are what the configuration asks of every service of the
	// stack, by name.
	Targets map[string]stackTarget
	// Images are the images of the services of the stack.
	Images []string
	// Paths are the paths of the host the services of the stack mount.
	Paths []string
}

// stackTarget is what the configuration of a deployment asks of one service
// of the stack.
//
// The fields are the ones the platform can compare with the specification of
// the cluster: the mode of the service, the number of its replicas and the
// image it runs.
type stackTarget struct {
	// Mode is the mode of the service: replicated or global.
	Mode string
	// Replicas is the number of the replicas of a replicated service.
	Replicas int
	// Image is the image of the service.
	Image string
	// Constraints are the constraints of the placement of the service.
	Constraints []string
}

// prepareStack binds the version of this deployment to the services of the
// document.
//
// Every service of the stack carries the digest of its own definition in its
// labels. The label is part of the specification of the service, so a service
// that runs another digest does not run the target of this deployment, even
// when the tasks of the older version still run.
//
// The digest of a service covers its own definition, so a service that did
// not change keeps its label and its tasks are not restarted for nothing.
func prepareStack(payload []byte) (stackDocument, error) {
	document := map[string]any{}
	if err := yaml.Unmarshal(payload, &document); err != nil {
		return stackDocument{}, fmt.Errorf("dockerops: 读取 Stack 配置失败: %w", err)
	}
	normalized, ok := normalize(document).(map[string]any)
	if !ok {
		return stackDocument{}, errors.New("dockerops: Stack 配置必须是映射")
	}
	services, ok := normalized["services"].(map[string]any)
	if !ok {
		return stackDocument{}, errors.New("dockerops: Stack 配置缺少 services 段")
	}

	digests := map[string]string{}
	targets := map[string]stackTarget{}
	images := []string{}
	seenImage := map[string]bool{}
	for name, definition := range services {
		service, ok := definition.(map[string]any)
		if !ok {
			return stackDocument{}, fmt.Errorf("dockerops: 服务 %s 的定义必须是映射", name)
		}
		digest, err := digestOfService(definition)
		if err != nil {
			return stackDocument{}, err
		}
		digests[name] = digest
		targets[name] = targetOfService(service)
		bindVersion(service, digest)

		if image := imageOf(service); image != "" && !seenImage[image] {
			seenImage[image] = true
			images = append(images, image)
		}
	}
	sort.Strings(images)

	rendered, err := yaml.Marshal(normalized)
	if err != nil {
		return stackDocument{}, fmt.Errorf("dockerops: 渲染 Stack 配置失败: %w", err)
	}
	return stackDocument{
		Payload: rendered,
		Digests: digests,
		Targets: targets,
		Images:  images,
		Paths:   yamlspec.HostPaths(rendered),
	}, nil
}

// targetOfService reads what the configuration asks of one service of the
// stack.
//
// The mode of a service is replicated and its number of replicas is one when
// the configuration does not say otherwise: those are the defaults of the
// Docker CLI.
func targetOfService(service map[string]any) stackTarget {
	target := stackTarget{Mode: modeReplicated, Replicas: 1, Image: imageOf(service)}

	deploy, ok := service["deploy"].(map[string]any)
	if !ok {
		return target
	}
	if mode, ok := deploy["mode"].(string); ok && mode != "" {
		target.Mode = mode
	}
	if replicas, ok := numberOf(deploy["replicas"]); ok {
		target.Replicas = replicas
	}
	placement, ok := deploy["placement"].(map[string]any)
	if !ok {
		return target
	}
	values, ok := placement["constraints"].([]any)
	if !ok {
		return target
	}
	for _, value := range values {
		constraint, ok := value.(string)
		if !ok {
			continue
		}
		target.Constraints = append(target.Constraints, constraint)
	}
	return target
}

// numberOf reads a number of the document.
//
// The parser of YAML hands the numbers of a document over as integers, but
// a number that does not fit into an integer arrives as a float.
func numberOf(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case uint64:
		return int(typed), true
	case float64:
		return int(typed), true
	default:
		return 0, false
	}
}

// bindVersion puts the digest of the definition of a service into the labels
// of the service.
func bindVersion(service map[string]any, digest string) {
	deploy, ok := service["deploy"].(map[string]any)
	if !ok {
		deploy = map[string]any{}
		service["deploy"] = deploy
	}
	labels, ok := deploy["labels"].(map[string]any)
	if !ok {
		labels = map[string]any{}
		deploy["labels"] = labels
	}
	labels[SwarmVersionLabel] = digest
}

// digestOfService returns the digest of the definition of a service.
//
// The digest covers the definition as it is deployed, so two versions of an
// application that differ in any field of a service have different digests.
// The digest is a hash, so it never holds a value of a variable.
func digestOfService(definition any) (string, error) {
	encoded, err := json.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("dockerops: 计算服务定义的摘要失败: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// normalize turns the nodes of a document into values the digest can cover.
//
// The parser of YAML hands a mapping with string keys over as a mapping of
// strings, but a mapping with other keys becomes a mapping of interfaces.
// The digest covers the same values on both sides, so the nodes are turned
// into one shape first.
func normalize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[key] = normalize(item)
		}
		return normalized
	case map[any]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[fmt.Sprintf("%v", key)] = normalize(item)
		}
		return normalized
	case []any:
		normalized := make([]any, len(typed))
		for index, item := range typed {
			normalized[index] = normalize(item)
		}
		return normalized
	default:
		return value
	}
}

// imageOf returns the image of a service of the stack.
func imageOf(service map[string]any) string {
	image, _ := service["image"].(string)
	return strings.TrimSpace(image)
}

// imageOfService returns the image the service of the cluster runs.
func imageOfService(service swarmapi.Service) string {
	if service.Spec.TaskTemplate.ContainerSpec == nil {
		return ""
	}
	return strings.TrimSpace(service.Spec.TaskTemplate.ContainerSpec.Image)
}

// imageRepository returns the repository of an image reference.
//
// A reference may name a registry, a tag and a digest. The registry is
// recognized by the rule of Docker: the first part of the path names a
// registry when it holds a point or a colon or when it is "localhost". The
// result is the name of the repository alone, so "nginx", "nginx:alpine",
// "nginx@sha256:..." and "docker.io/library/nginx@sha256:..." name the same
// repository. The comparison of the images therefore does not mistake the
// digest of the registry for a difference.
func imageRepository(image string) string {
	reference := strings.TrimSpace(image)
	if reference == "" {
		return ""
	}
	// The digest comes last.
	if at := strings.Index(reference, "@"); at >= 0 {
		reference = reference[:at]
	}
	parts := strings.Split(reference, "/")
	// The tag belongs to the last part of the path.
	last := len(parts) - 1
	if colon := strings.LastIndex(parts[last], ":"); colon >= 0 {
		parts[last] = parts[last][:colon]
	}
	if len(parts) > 1 && registryHost(parts[0]) {
		host := parts[0]
		parts = parts[1:]
		// The "library" namespace only holds the default namespace of the
		// Docker Hub, so "docker.io/library/nginx" names the same
		// repository as "nginx".
		if len(parts) > 1 && parts[0] == "library" && dockerHubHost(host) {
			parts = parts[1:]
		}
	}
	return strings.Join(parts, "/")
}

// registryHost reports whether the first part of the path of an image
// reference names a registry.
func registryHost(part string) bool {
	return part == "localhost" || strings.ContainsAny(part, ".:")
}

// dockerHubHost reports whether a registry host names the Docker Hub.
func dockerHubHost(host string) bool {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	}
	return false
}

// needsRegistryAuth reports whether the stack needs the credentials of the
// registry of the manager.
//
// The credentials are only handed to the cluster when the stack pulls an
// image of a registry of its own: the Docker Hub needs no credentials. The
// platform never reads the credentials itself, the command line of Docker
// reads them and hands them to the cluster.
func needsRegistryAuth(images []string) bool {
	for _, image := range images {
		if privateRegistry(image) {
			return true
		}
	}
	return false
}

// privateRegistry reports whether an image comes from a registry of its own.
//
// The first part of a reference names a registry when it holds a point or a
// colon or when it is "localhost". Every other reference comes from the
// Docker Hub.
func privateRegistry(image string) bool {
	reference := strings.TrimSpace(image)
	if reference == "" {
		return false
	}
	parts := strings.SplitN(reference, "/", 2)
	if len(parts) < 2 {
		return false
	}
	return registryHost(parts[0])
}

// hostPathNote reports what the manager could not prove about the paths of
// the host the stack mounts.
//
// The manager checks the paths of its own node. The paths of the workers
// belong to the hosts of the workers, so the manager never claims that they
// exist: the nodes it cannot check are named, so the administrator sees the
// targets no agent of the platform has verified yet.
func hostPathNote(paths []string, nodes []swarmapi.Node, identity protocol.NodeIdentity) string {
	if len(paths) == 0 {
		return ""
	}

	var missing []string
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)

	var others []string
	for _, node := range nodes {
		if node.ID == identity.SwarmNodeID {
			continue
		}
		name := node.Description.Hostname
		if name == "" {
			name = node.ID
		}
		others = append(others, name)
	}
	sort.Strings(others)

	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "Manager 上不存在的路径："+strings.Join(missing, ", "))
	}
	if len(others) > 0 {
		parts = append(parts, "以下节点的主机路径未核实："+strings.Join(others, ", "))
	}
	return strings.Join(parts, "；")
}

// joinNote adds a note to the message of an outcome.
func joinNote(message, note string) string {
	if note == "" {
		return message
	}
	if message == "" {
		return note
	}
	return message + "；" + note
}

// limitText shortens a text to the given number of bytes.
func limitText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
