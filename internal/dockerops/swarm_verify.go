package dockerops

import (
	"fmt"
	"strings"

	swarmapi "github.com/docker/docker/api/types/swarm"

	"hmoob-dock/internal/model"
)

// The codes of the outcomes of the convergence check of a stack.
const (
	// CodeSwarmReady marks a stack whose services reached the expected
	// state.
	CodeSwarmReady = "swarm_ready"
	// CodeSwarmNotReady marks a stack whose services did not reach the
	// expected state yet. The state may still change, so the executor
	// waits.
	CodeSwarmNotReady = "swarm_not_ready"
	// CodeSwarmSpecMismatch marks a service that does not run the
	// specification of this deployment.
	CodeSwarmSpecMismatch = "swarm_spec_mismatch"
	// CodeSwarmTaskFailed marks a service with a task that failed or that
	// the cluster rejected.
	CodeSwarmTaskFailed = "swarm_task_failed"
	// CodeSwarmUpdatePaused marks a service whose update is paused.
	CodeSwarmUpdatePaused = "swarm_update_paused"
	// CodeSwarmRolledBack marks a service the cluster rolled back.
	CodeSwarmRolledBack = "swarm_rolled_back"
	// CodeSwarmTargetUnknown marks a service whose effective target cannot
	// be proven, because the platform cannot read the constraints of its
	// placement.
	CodeSwarmTargetUnknown = "swarm_target_unknown"
)

// The states of a service of the convergence check.
const (
	// swarmStateReady marks a service that reached the expected state.
	swarmStateReady = "ready"
	// swarmStatePending marks a service that did not reach the expected state
	// yet.
	swarmStatePending = "pending"
	// swarmStateFailed marks a service that will not reach the expected state.
	swarmStateFailed = "failed"
)

// The states of the update of a service of a cluster.
const (
	// updateUpdating marks an update that runs.
	updateUpdating = "updating"
	// updatePaused marks an update that the cluster paused.
	updatePaused = "paused"
	// updateRollbackStarted marks a rollback that runs.
	updateRollbackStarted = "rollback_started"
	// updateRollbackPaused marks a rollback that the cluster paused.
	updateRollbackPaused = "rollback_paused"
	// updateRollbackCompleted marks a rollback the cluster finished.
	updateRollbackCompleted = "rollback_completed"
)

// ServiceObservation is what the platform knows about one service of a stack.
//
// The observation holds the facts the convergence check reasons about. The
// facts come from the Docker SDK, so the check never trusts the exit code of
// a command: a stack whose services run an old specification is not ready,
// even when its old replicas still run.
type ServiceObservation struct {
	// Name is the name of the service inside of the stack.
	Name string
	// UpdateState is the state of the update of the service, for example
	// updating, paused, completed or rollback_completed.
	UpdateState string
	// Desired is the target of this deployment: the number of the replicas
	// the configuration asks for and the number of the eligible nodes of a
	// global service. The target comes from the configuration and not from
	// the cluster, so a cluster that was changed by hand never reaches it.
	Desired int
	// Running is the number of the tasks of the current specification that
	// run.
	Running int
	// FailedTasks are the tasks of the current specification that failed or
	// that the cluster rejected.
	FailedTasks []string
	// CurrentSpecDigest identifies the specification the service runs.
	CurrentSpecDigest string
	// TargetSpecDigest identifies the specification of this deployment.
	TargetSpecDigest string
}

// CheckSwarmReadiness reports whether the services of a stack reached the
// expected state.
//
// The check reads the actual state of the services:
//
//   - a service whose specification is not the specification of this
//     deployment is not ready, even when its old replicas still run;
//   - a service with a failed task is not ready;
//   - a service whose update the cluster paused or rolled back is not ready,
//     and a rollback is reported as such;
//   - a service that runs fewer tasks than the target of this deployment is
//     not ready yet, and a service that runs none of them is ready.
//
// The target of a service comes from the configuration of the deployment and
// not from the cluster, so a cluster that was changed by hand never reaches
// it.
//
// The outcome of a deployment the administrator caused is a result and not an
// error. The executor waits while the result is "not ready", because the
// state of a service may still change.
func CheckSwarmReadiness(services []ServiceObservation) model.Result {
	if len(services) == 0 {
		return model.Result{
			State:   model.Failed,
			Code:    CodeSwarmNotReady,
			Message: "Stack 没有服务",
		}
	}

	var messages []string
	ready := 0
	failure := ""
	for _, service := range services {
		state, code, message := swarmServiceState(service)
		messages = append(messages, message)
		switch state {
		case swarmStateReady:
			ready++
		default:
			if failure == "" || failure == CodeSwarmNotReady {
				failure = code
			}
		}
	}

	switch failure {
	case "":
		return model.Result{
			State:   model.Succeeded,
			Code:    CodeSwarmReady,
			Message: strings.Join(messages, "; "),
		}
	default:
		result := model.Result{
			State:   model.Failed,
			Code:    failure,
			Message: strings.Join(messages, "; "),
		}
		// A part of the stack is ready and a part is not: the stack is in
		// a mixed state.
		result.PartialUpdate = ready > 0
		if rolledBack(services) {
			// The cluster undid the change, so the new version never
			// became the version that runs.
			result.RolledBack = true
		}
		return result
	}
}

// serviceState reports the state of one service of a stack.
//
// The second return value names the kind of the failure, so a service whose
// specification is not the target is told apart from a service with a failed
// task.
func swarmServiceState(service ServiceObservation) (string, string, string) {
	switch service.UpdateState {
	case updateRollbackCompleted, updateRollbackStarted, updateRollbackPaused:
		return swarmStateFailed, CodeSwarmRolledBack, fmt.Sprintf(
			"服务 %s 的更新被集群回滚（%s）", service.Name, service.UpdateState)
	case updatePaused:
		return swarmStateFailed, CodeSwarmUpdatePaused, fmt.Sprintf(
			"服务 %s 的更新被集群暂停", service.Name)
	case updateUpdating:
		return swarmStatePending, CodeSwarmNotReady, fmt.Sprintf(
			"服务 %s 正在更新", service.Name)
	}

	if service.TargetSpecDigest == "" {
		return swarmStateFailed, CodeSwarmSpecMismatch, fmt.Sprintf(
			"服务 %s 没有本次部署的目标规格", service.Name)
	}
	if service.CurrentSpecDigest != service.TargetSpecDigest {
		return swarmStateFailed, CodeSwarmSpecMismatch, fmt.Sprintf(
			"服务 %s 运行的不是本次部署的规格", service.Name)
	}
	if service.Running != service.Desired {
		// The service did not reach its target, so it is not ready. The
		// cluster retries a task that failed, so the deployment waits for
		// the retry instead of giving up on the first failure.
		if len(service.FailedTasks) > 0 {
			return swarmStatePending, CodeSwarmTaskFailed, fmt.Sprintf(
				"服务 %s 运行 %d 个任务，目标 %d 个，失败的任务：%s",
				service.Name, service.Running, service.Desired,
				strings.Join(service.FailedTasks, ", "))
		}
		return swarmStatePending, CodeSwarmNotReady, fmt.Sprintf(
			"服务 %s 运行 %d 个任务，目标 %d 个", service.Name, service.Running, service.Desired)
	}
	return swarmStateReady, CodeSwarmReady, fmt.Sprintf(
		"服务 %s 已达到目标状态（%d 个任务）", service.Name, service.Running)
}

// swarmWaits reports whether the executor has to wait for the outcome.
//
// A service that did not reach its target may still reach it: the cluster
// retries a task that failed, so the executor waits for the retry and gives
// up when the deadline of the task is over.
func swarmWaits(code string) bool {
	return code == CodeSwarmNotReady || code == CodeSwarmTaskFailed
}

// rolledBack reports whether the cluster rolled back the change of one of the
// services.
func rolledBack(services []ServiceObservation) bool {
	for _, service := range services {
		if rolledBackState(service.UpdateState) {
			return true
		}
	}
	return false
}

// rolledBackState reports whether the state of the update of a service means
// that the cluster undid the change.
//
// A service the cluster rolled back runs the specification of the version
// before the deployment, so its specification differs from the specification
// of the deployment by its own accord.
func rolledBackState(state string) bool {
	switch state {
	case updateRollbackCompleted, updateRollbackStarted, updateRollbackPaused:
		return true
	}
	return false
}

// modeOf returns the mode of a service of a cluster.
func modeOf(service swarmapi.Service) string {
	switch {
	case service.Spec.Mode.Replicated != nil:
		return "replicated"
	case service.Spec.Mode.Global != nil:
		return "global"
	default:
		return ""
	}
}

// updateStateOf returns the state of the update of a service of a cluster.
//
// A service that was never updated has no state, and that is not a failure:
// the tasks of the service are the ones of its specification.
func updateStateOf(service swarmapi.Service) string {
	if service.UpdateStatus == nil {
		return ""
	}
	return string(service.UpdateStatus.State)
}

// replicasOf returns the number of the replicas of a replicated service.
//
// A service without a number of replicas runs one of them, which is the
// default of the cluster.
func replicasOf(service swarmapi.Service) int {
	if service.Spec.Mode.Replicated == nil || service.Spec.Mode.Replicated.Replicas == nil {
		if service.Spec.Mode.Replicated == nil {
			return 0
		}
		return 1
	}
	return int(*service.Spec.Mode.Replicated.Replicas)
}

// effectiveTarget returns the number of the tasks a service of the cluster
// has to run.
//
// A replicated service runs the number of its replicas. A global service runs
// one task on every node that is eligible for it, so its target depends on
// the nodes of the cluster and on the constraints of its placement.
//
// The target is the target of the cluster and not the target of a
// deployment: the convergence check derives its own target from the
// configuration, so a cluster that was changed by hand never reaches it.
//
// The second return value reports whether the target could be proven. A
// constraint the platform cannot read leaves the target unknown, and the
// platform never claims that such a service converged.
func effectiveTarget(service swarmapi.Service, nodes []swarmapi.Node) (int, bool) {
	switch {
	case service.Spec.Mode.Replicated != nil:
		return replicasOf(service), true
	case service.Spec.Mode.Global != nil:
		eligible, known := eligibleNodes(placementConstraints(service), nodes)
		if !known {
			return 0, false
		}
		return len(eligible), true
	default:
		return 0, false
	}
}

// configuredTarget returns the number of the tasks a service of this
// deployment has to run.
//
// A replicated service asks for the number of its replicas, a global service
// for one task on every node that may run it. The target comes from the
// configuration of this deployment and not from the cluster: a cluster that
// was scaled by hand runs more tasks than the deployment asks for, so it
// never reached the target of this deployment.
//
// The second return value reports whether the target could be proven. A
// constraint the platform cannot read leaves the target unknown, and the
// platform never claims that such a service converged.
func configuredTarget(target stackTarget, nodes []swarmapi.Node) (int, bool) {
	switch target.Mode {
	case modeReplicated:
		return target.Replicas, true
	case modeGlobal:
		eligible, known := eligibleNodes(target.Constraints, nodes)
		if !known {
			return 0, false
		}
		return len(eligible), true
	default:
		return 0, false
	}
}

// eligibleNodes returns the nodes a global service may run on.
//
// A node is eligible when it is active, when it is ready and when it
// satisfies the constraints of the placement of the service. The second
// return value reports whether the constraints could be read.
func eligibleNodes(constraints []string, nodes []swarmapi.Node) ([]swarmapi.Node, bool) {
	var eligible []swarmapi.Node
	for _, node := range nodes {
		if node.Spec.Availability != swarmapi.NodeAvailabilityActive {
			// A node that is paused or that drains keeps its tasks but
			// gets no new ones.
			continue
		}
		if node.Status.State != swarmapi.NodeStateReady {
			continue
		}
		ok, known := nodeMatches(node, constraints)
		if !known {
			return nil, false
		}
		if !ok {
			continue
		}
		eligible = append(eligible, node)
	}
	return eligible, true
}

// placementConstraints returns the constraints of the placement of a service.
func placementConstraints(service swarmapi.Service) []string {
	if service.Spec.TaskTemplate.Placement == nil {
		return nil
	}
	return service.Spec.TaskTemplate.Placement.Constraints
}

// nodeMatches reports whether a node satisfies the constraints of a
// placement.
//
// The second return value reports whether the constraints could be read. A
// constraint the platform does not know leaves the answer unknown, so the
// platform never counts a node it cannot judge.
func nodeMatches(node swarmapi.Node, constraints []string) (bool, bool) {
	for _, constraint := range constraints {
		ok, known := matchesConstraint(node, constraint)
		if !known {
			return false, false
		}
		if !ok {
			return false, true
		}
	}
	return true, true
}

// matchesConstraint reports whether a node satisfies one constraint.
//
// The constraints the platform reads are the ones the cluster documents: the
// identity, the name, the role, a label, the platform and the engine labels
// of a node. The operators "==" and "!=" are the ones the platform reads, so
// a constraint with another operator leaves the answer unknown.
func matchesConstraint(node swarmapi.Node, constraint string) (bool, bool) {
	fields := strings.Fields(strings.TrimSpace(constraint))
	if len(fields) != 3 {
		return false, false
	}
	key, operator, value := fields[0], fields[1], fields[2]
	switch operator {
	case "==", "!=":
	default:
		return false, false
	}

	actual, known := constraintValue(node, key)
	if !known {
		return false, false
	}
	if operator == "==" {
		return actual == value, true
	}
	return actual != value, true
}

// constraintValue returns the value of a node a constraint refers to.
//
// The second return value reports whether the platform knows the key of the
// constraint.
func constraintValue(node swarmapi.Node, key string) (string, bool) {
	switch {
	case key == "node.id":
		return node.ID, true
	case key == "node.hostname":
		return node.Description.Hostname, true
	case key == "node.role":
		return string(node.Spec.Role), true
	case key == "node.platform.os":
		return node.Description.Platform.OS, true
	case key == "node.platform.arch":
		return node.Description.Platform.Architecture, true
	case strings.HasPrefix(key, "node.labels."):
		return node.Spec.Annotations.Labels[strings.TrimPrefix(key, "node.labels.")], true
	case strings.HasPrefix(key, "engine.labels."):
		return node.Description.Engine.Labels[strings.TrimPrefix(key, "engine.labels.")], true
	default:
		return "", false
	}
}

// runningTasksOf returns the number of the tasks of a service that run.
func runningTasksOf(tasks []swarmapi.Task, serviceID string) int {
	running := 0
	for _, task := range tasks {
		if task.ServiceID != serviceID {
			continue
		}
		if task.Status.State == swarmapi.TaskStateRunning {
			running++
		}
	}
	return running
}
