package dockerops_test

import (
	"strings"
	"testing"

	"hmoob-dock/internal/dockerops"
	"hmoob-dock/internal/model"
)

// TestRollbackCannotMarkNewVersionSuccessful proves that a service the
// cluster rolled back never reports the new version as successful.
//
// The command line of Docker wrote the stack and the cluster undid the
// change, so the version that runs is the version before the deployment.
func TestRollbackCannotMarkNewVersionSuccessful(t *testing.T) {
	result := dockerops.CheckSwarmReadiness([]dockerops.ServiceObservation{{
		Name: "demo_api", UpdateState: "rollback_completed", Desired: 2, Running: 2,
		CurrentSpecDigest: "old", TargetSpecDigest: "new",
	}})
	if result.State == model.Succeeded || !result.RolledBack {
		t.Fatalf("rollback hidden: %+v", result)
	}
}

// TestTheSwarmReadinessNeedsTheSpecificationOfTheDeployment proves that a
// service that runs another specification is not ready, even when its tasks
// run.
//
// The old replicas of a service keep running while the deployment waits, so
// the number of the running tasks alone never proves that the deployment
// reached its target.
func TestTheSwarmReadinessNeedsTheSpecificationOfTheDeployment(t *testing.T) {
	result := dockerops.CheckSwarmReadiness([]dockerops.ServiceObservation{{
		Name: "api", UpdateState: "completed", Desired: 2, Running: 2,
		CurrentSpecDigest: "old", TargetSpecDigest: "new",
	}})
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != dockerops.CodeSwarmSpecMismatch {
		t.Fatalf("the code = %q, want %q", result.Code, dockerops.CodeSwarmSpecMismatch)
	}
	if !strings.Contains(result.Message, "api") {
		t.Fatalf("the message = %q, want the service", result.Message)
	}
}

// TestTheSwarmReadinessNeedsTheSpecificationOfEveryService proves that one
// service of a stack that runs another specification keeps the whole stack
// from being ready.
func TestTheSwarmReadinessNeedsTheSpecificationOfEveryService(t *testing.T) {
	result := dockerops.CheckSwarmReadiness([]dockerops.ServiceObservation{
		{
			Name: "api", UpdateState: "completed", Desired: 1, Running: 1,
			CurrentSpecDigest: "new", TargetSpecDigest: "new",
		},
		{
			Name: "worker", UpdateState: "completed", Desired: 1, Running: 1,
			CurrentSpecDigest: "old", TargetSpecDigest: "new",
		},
	})
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if !result.PartialUpdate {
		t.Fatal("the part of the stack that is ready was not reported")
	}
}

// TestTheSwarmReadinessFailsWhenATaskFailed proves that a service with a
// failed task is not ready.
//
// The command line of Docker reports that the stack was written, so a task
// that failed has to be found in the state of the cluster.
func TestTheSwarmReadinessFailsWhenATaskFailed(t *testing.T) {
	result := dockerops.CheckSwarmReadiness([]dockerops.ServiceObservation{{
		Name: "api", UpdateState: "completed", Desired: 2, Running: 1,
		FailedTasks:       []string{"task-1（no suitable node）"},
		CurrentSpecDigest: "new", TargetSpecDigest: "new",
	}})
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != dockerops.CodeSwarmTaskFailed {
		t.Fatalf("the code = %q, want %q", result.Code, dockerops.CodeSwarmTaskFailed)
	}
	if !strings.Contains(result.Message, "task-1") {
		t.Fatalf("the message = %q, want the failed task", result.Message)
	}
}

// TestTheSwarmReadinessWaitsForTheRunningTasks proves that a service that
// runs fewer tasks than its target is not ready yet.
func TestTheSwarmReadinessWaitsForTheRunningTasks(t *testing.T) {
	result := dockerops.CheckSwarmReadiness([]dockerops.ServiceObservation{{
		Name: "api", UpdateState: "updating", Desired: 2, Running: 1,
		CurrentSpecDigest: "new", TargetSpecDigest: "new",
	}})
	if result.State != model.Failed {
		t.Fatalf("the state = %q, want %q", result.State, model.Failed)
	}
	if result.Code != dockerops.CodeSwarmNotReady {
		t.Fatalf("the code = %q, want %q", result.Code, dockerops.CodeSwarmNotReady)
	}
}

// TestTheSwarmReadinessAcceptsAServiceWithoutReplicas proves that a service
// that runs no task at all is ready when its target is no task at all.
func TestTheSwarmReadinessAcceptsAServiceWithoutReplicas(t *testing.T) {
	result := dockerops.CheckSwarmReadiness([]dockerops.ServiceObservation{{
		Name: "api", UpdateState: "completed", Desired: 0, Running: 0,
		CurrentSpecDigest: "new", TargetSpecDigest: "new",
	}})
	if result.State != model.Succeeded {
		t.Fatalf("the state = %q, want %q (%s)", result.State, model.Succeeded, result.Message)
	}
	if result.Code != dockerops.CodeSwarmReady {
		t.Fatalf("the code = %q, want %q", result.Code, dockerops.CodeSwarmReady)
	}
}

// TestTheSwarmReadinessAcceptsAGlobalService proves that a global service is
// ready when it runs one task on every node of its target.
func TestTheSwarmReadinessAcceptsAGlobalService(t *testing.T) {
	result := dockerops.CheckSwarmReadiness([]dockerops.ServiceObservation{{
		Name: "api", UpdateState: "completed", Desired: 2, Running: 2,
		CurrentSpecDigest: "new", TargetSpecDigest: "new",
	}})
	if result.State != model.Succeeded {
		t.Fatalf("the state = %q, want %q (%s)", result.State, model.Succeeded, result.Message)
	}
}

// TestTheSwarmReadinessReportsAPausedUpdate proves that an update the
// cluster paused never reports a success.
func TestTheSwarmReadinessReportsAPausedUpdate(t *testing.T) {
	result := dockerops.CheckSwarmReadiness([]dockerops.ServiceObservation{{
		Name: "api", UpdateState: "paused", Desired: 2, Running: 2,
		CurrentSpecDigest: "new", TargetSpecDigest: "new",
	}})
	if result.State == model.Succeeded {
		t.Fatalf("the state = %q, want a failure", result.State)
	}
	if result.Code != dockerops.CodeSwarmUpdatePaused {
		t.Fatalf("the code = %q, want %q", result.Code, dockerops.CodeSwarmUpdatePaused)
	}
}
