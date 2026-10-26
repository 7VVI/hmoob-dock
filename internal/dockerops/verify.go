package dockerops

import (
	"fmt"
	"strings"

	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// The codes of the outcomes of the readiness check.
const (
	// CodeComposeReady marks an application whose services are ready.
	CodeComposeReady = "compose_ready"
	// CodeComposeNotReady marks an application whose services are not
	// ready yet. The state may still change, so the executor waits.
	CodeComposeNotReady = "compose_not_ready"
	// CodeComposeUnhealthy marks a service whose health check failed.
	CodeComposeUnhealthy = "compose_unhealthy"
	// CodeComposeFailed marks a service that is gone or that ended with an
	// error.
	CodeComposeFailed = "compose_failed"
)

// The states of a service of the readiness check.
const (
	// stateReady marks a service that reached the expected state.
	stateReady = "ready"
	// statePending marks a service that did not reach the expected state
	// yet.
	statePending = "pending"
	// stateFailed marks a service that will not reach the expected state.
	stateFailed = "failed"
)

// CheckComposeReadiness reports whether the services of an application
// reached the expected state.
//
// The check reads the actual state of the containers: every expected service
// needs a container, a service that runs forever has to run, a service with
// a health check has to be healthy, and a service that runs once has to have
// ended with the exit code 0.
//
// A service without a health check is only reported as running: the platform
// never claims that a service is healthy when nothing checks it.
//
// The services that run once are named by the caller. They are the services
// that carry the lifecycle extension of the platform, so a service that
// ends is not a failure but the result the administrator asked for.
func CheckComposeReadiness(expected []string, oneShot []string, containers []protocol.Container) model.Result {
	oneShots := make(map[string]bool, len(oneShot))
	for _, name := range oneShot {
		oneShots[name] = true
	}

	byService := make(map[string][]protocol.Container, len(containers))
	for _, item := range containers {
		byService[item.Service] = append(byService[item.Service], item)
	}

	var ready []string
	var messages []string
	failure := ""
	for _, name := range expected {
		list := byService[name]
		if len(list) == 0 {
			failure = CodeComposeFailed
			messages = append(messages, fmt.Sprintf("服务 %s 没有容器", name))
			continue
		}
		for _, item := range list {
			state, code, message := serviceState(name, oneShots[name], item)
			messages = append(messages, message)
			switch state {
			case stateReady:
				ready = append(ready, name)
			case statePending:
				if failure == "" {
					failure = CodeComposeNotReady
				}
			default:
				if failure == "" || failure == CodeComposeNotReady {
					failure = code
				}
			}
		}
	}

	switch failure {
	case "":
		if len(messages) == 0 {
			// The configuration holds no service, so there is nothing to
			// report.
			messages = []string{"应用没有服务"}
		}
		return model.Result{
			State:   model.Succeeded,
			Code:    CodeComposeReady,
			Message: strings.Join(messages, "; "),
		}
	case CodeComposeNotReady:
		return model.Result{
			State:   model.Failed,
			Code:    CodeComposeNotReady,
			Message: strings.Join(messages, "; "),
		}
	default:
		result := model.Result{
			State:   model.Failed,
			Code:    failure,
			Message: strings.Join(messages, "; "),
		}
		// A part of the application is ready and a part is not: the
		// application is in a mixed state.
		result.PartialUpdate = len(ready) > 0
		return result
	}
}

// serviceState reports the state of one container of a service.
//
// The second return value names the kind of the failure, so a service whose
// health check failed is told apart from a service that is gone.
func serviceState(name string, oneShot bool, item protocol.Container) (string, string, string) {
	switch item.State {
	case "running":
		if oneShot {
			// The service runs once and has not ended yet.
			return statePending, CodeComposeNotReady, fmt.Sprintf("服务 %s 仍在运行", name)
		}
		switch item.Health {
		case "":
			// Nothing checks the service, so it is only running.
			return stateReady, CodeComposeReady, fmt.Sprintf("服务 %s 已运行", name)
		case "healthy":
			return stateReady, CodeComposeReady, fmt.Sprintf("服务 %s 已健康", name)
		case "unhealthy":
			return stateFailed, CodeComposeUnhealthy, fmt.Sprintf("服务 %s 的健康检查失败", name)
		default:
			// The health check is starting or has a state the platform
			// does not know: the service is not ready yet.
			return statePending, CodeComposeNotReady, fmt.Sprintf("服务 %s 的健康检查还在启动", name)
		}
	case "exited":
		if oneShot {
			if item.ExitCode == nil {
				return statePending, CodeComposeNotReady, fmt.Sprintf("服务 %s 已结束，但没有退出码", name)
			}
			if *item.ExitCode == 0 {
				return stateReady, CodeComposeReady, fmt.Sprintf("服务 %s 已完成（退出码 0）", name)
			}
			return stateFailed, CodeComposeFailed, fmt.Sprintf("服务 %s 以退出码 %d 结束", name, *item.ExitCode)
		}
		return stateFailed, CodeComposeFailed, fmt.Sprintf("服务 %s 已退出（退出码 %s）", name, exitCodeOf(item))
	case "dead":
		return stateFailed, CodeComposeFailed, fmt.Sprintf("服务 %s 已经停止", name)
	default:
		// The container was created, is restarting or is paused: it did not
		// start yet.
		return statePending, CodeComposeNotReady, fmt.Sprintf("服务 %s 还没有启动（状态 %s）", name, item.State)
	}
}

// exitCodeOf returns the exit code of a container that ended.
func exitCodeOf(item protocol.Container) string {
	if item.ExitCode == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d", *item.ExitCode)
}
