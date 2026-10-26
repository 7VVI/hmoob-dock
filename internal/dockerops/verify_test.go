package dockerops_test

import (
	"strings"
	"testing"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/model"
	"hmoob-dock/internal/protocol"
)

// TestTheReadinessCheckNeedsEveryService proves that a service of the
// configuration without a container keeps the deployment from being ready.
//
// The service is not a failure yet: Compose creates the container of a
// service after the services it depends on are healthy, so the executor
// waits for it.
func TestTheReadinessCheckNeedsEveryService(t *testing.T) {
	result := dockerops.CheckComposeReadiness(
		[]string{"api", "worker"},
		nil,
		[]protocol.Container{{Service: "api", State: "running"}},
	)
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != dockerops.CodeComposeNotReady {
		t.Fatalf("the code = %q, want %q", result.Code, dockerops.CodeComposeNotReady)
	}
	if !strings.Contains(result.Message, "worker") {
		t.Fatalf("the message = %q, want the missing service", result.Message)
	}
}

// TestTheReadinessCheckWaitsForTheHealthCheck proves that a container whose
// health check did not pass yet is not ready and that a healthy container
// is.
func TestTheReadinessCheckWaitsForTheHealthCheck(t *testing.T) {
	starting := dockerops.CheckComposeReadiness(
		[]string{"api"},
		nil,
		[]protocol.Container{{Service: "api", State: "running", Health: "starting"}},
	)
	if starting.State != model.Failed {
		t.Fatalf("the state = %q, want %q", starting.State, model.Failed)
	}
	if starting.Code != dockerops.CodeComposeNotReady {
		t.Fatalf("the code = %q, want %q", starting.Code, dockerops.CodeComposeNotReady)
	}

	healthy := dockerops.CheckComposeReadiness(
		[]string{"api"},
		nil,
		[]protocol.Container{{Service: "api", State: "running", Health: "healthy"}},
	)
	if healthy.State != model.Succeeded {
		t.Fatalf("the state = %q, want %q", healthy.State, model.Succeeded)
	}
	if healthy.Code != dockerops.CodeComposeReady {
		t.Fatalf("the code = %q, want %q", healthy.Code, dockerops.CodeComposeReady)
	}
}

// TestTheReadinessCheckReportsAnUnhealthyService proves that a service whose
// health check failed fails the deployment.
func TestTheReadinessCheckReportsAnUnhealthyService(t *testing.T) {
	result := dockerops.CheckComposeReadiness(
		[]string{"api"},
		nil,
		[]protocol.Container{{Service: "api", State: "running", Health: "unhealthy"}},
	)
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != dockerops.CodeComposeUnhealthy {
		t.Fatalf("the code = %q, want %q", result.Code, dockerops.CodeComposeUnhealthy)
	}
}

// TestTheReadinessCheckDoesNotClaimHealthWithoutAHealthCheck proves that a
// service without a health check is only reported as running: the platform
// never claims that a service is healthy when nothing checks it.
func TestTheReadinessCheckDoesNotClaimHealthWithoutAHealthCheck(t *testing.T) {
	result := dockerops.CheckComposeReadiness(
		[]string{"api"},
		nil,
		[]protocol.Container{{Service: "api", State: "running"}},
	)
	if result.State != model.Succeeded {
		t.Fatalf("the state = %q, want %q", result.State, model.Succeeded)
	}
	if !strings.Contains(result.Message, "已运行") {
		t.Fatalf("the message = %q, want a report that the service runs", result.Message)
	}
	if strings.Contains(result.Message, "健康") {
		t.Fatalf("the message = %q, want no report about the health", result.Message)
	}
}

// TestTheReadinessCheckUsesTheExitCodeOfAOneShotService proves that a
// service that runs once is ready when it ended with the exit code 0 and
// that it fails when it ended with an error.
func TestTheReadinessCheckUsesTheExitCodeOfAOneShotService(t *testing.T) {
	zero := 0
	finished := dockerops.CheckComposeReadiness(
		[]string{"seed"},
		[]string{"seed"},
		[]protocol.Container{{Service: "seed", State: "exited", ExitCode: &zero}},
	)
	if finished.State != model.Succeeded {
		t.Fatalf("the state = %q, want %q", finished.State, model.Succeeded)
	}

	two := 2
	failed := dockerops.CheckComposeReadiness(
		[]string{"seed"},
		[]string{"seed"},
		[]protocol.Container{{Service: "seed", State: "exited", ExitCode: &two}},
	)
	if failed.State != model.Failed {
		t.Fatalf("the state = %q, want %q", failed.State, model.Failed)
	}
	if !strings.Contains(failed.Message, "2") {
		t.Fatalf("the message = %q, want the exit code", failed.Message)
	}
}

// TestTheReadinessCheckReportsAPartialUpdate proves that an application of
// which a part is ready and a part is not is reported as partly changed.
func TestTheReadinessCheckReportsAPartialUpdate(t *testing.T) {
	result := dockerops.CheckComposeReadiness(
		[]string{"api", "worker"},
		nil,
		[]protocol.Container{
			{Service: "api", State: "running"},
			{Service: "worker", State: "running", Health: "unhealthy"},
		},
	)
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if !result.PartialUpdate {
		t.Fatal("the deployment was not reported as a partial update")
	}
}

// TestTheReadinessCheckReportsAServiceThatEnded proves that a service that
// runs forever and that ended is a failure, even without a health check.
func TestTheReadinessCheckReportsAServiceThatEnded(t *testing.T) {
	one := 1
	result := dockerops.CheckComposeReadiness(
		[]string{"api"},
		nil,
		[]protocol.Container{{Service: "api", State: "exited", ExitCode: &one}},
	)
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.PartialUpdate {
		t.Fatal("nothing of the application is ready")
	}
}
