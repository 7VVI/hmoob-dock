package dockerops

import (
	"context"
	"io"

	"hmoob-dock/internal/model"
)

// Executor runs a deployment command on a node.
//
// The executor writes the output of the command into the given writer and
// reports the outcome of the task. An outcome that the administrator caused
// — a document that does not validate, an image that cannot be pulled, a
// service that does not become ready — is reported as a result and not as an
// error. An error means that the executor could not tell what happened to
// the target, and the task waits for a check.
type Executor interface {
	// Execute deploys the application of the command and reports the
	// outcome of the task.
	//
	// The context bounds the execution of the Docker command. The caller
	// hands in the context of the process of the agent, so a connection
	// that ends does not cancel a command that already runs.
	Execute(ctx context.Context, cmd model.Command, output io.Writer) (model.Result, error)
}
