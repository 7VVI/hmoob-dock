// Package model defines the domain contract of a deployment.
//
// The types of the package are shared by the control端, the agent and the
// deployment services. They are the single definition of the application,
// of its configuration, of a deployment task and of the receipt an agent
// sends back, so a later stage never introduces a second set of fields for
// the same concept.
package model

import "time"

// App is an application that the control端 deploys.
type App struct {
	// ID is the identifier of the application.
	ID string `json:"id"`
	// Name is the name the administrator gave to the application.
	Name string `json:"name"`
	// Mode selects the runtime of the application: "compose" or "swarm".
	Mode string `json:"mode"`
	// TargetID is the node or the cluster the application runs on.
	TargetID string `json:"target_id"`
	// Namespace is the fixed namespace of the application. It is the name
	// of the Compose project and of the Swarm stack.
	Namespace string `json:"namespace"`
	// LatestVersionID is the version that was submitted last.
	LatestVersionID string `json:"latest_version_id"`
	// SuccessfulVersionID is the version that was deployed successfully
	// last.
	SuccessfulVersionID string `json:"successful_version_id"`
}

// Config is the configuration of an application.
//
// The variables belong to the configuration and not to the machine that runs
// the control端, so the same configuration produces the same deployment on
// every node.
type Config struct {
	// YAML is the Compose document of the application.
	YAML string `json:"yaml"`
	// Variables are the explicit variables of the application. They are
	// the only source of the values a configuration refers to.
	Variables map[string]string `json:"variables"`
}

// CheckedConfig is a configuration that passed the validation.
//
// It holds the payload of the deployment, the services it defines and the
// risks the administrator has to confirm.
type CheckedConfig struct {
	// RenderedYAML is the document that is handed to the Docker CLI.
	RenderedYAML []byte
	// Services are the names of the services of the application.
	Services []string
	// OneShotServices are the services that run once and exit.
	OneShotServices []string
	// Warnings are the risks of the configuration. Every one of them
	// reaches the preview of the deployment.
	Warnings []string
	// Digest identifies the semantics of the configuration.
	Digest string
}

// TaskState is the state of a deployment task.
type TaskState string

const (
	// Queued means that the task waits for an executioner.
	Queued TaskState = "queued"
	// Preflight means that the task checks the target before it changes
	// anything.
	Preflight TaskState = "preflight"
	// Running means that the task changes the target.
	Running TaskState = "running"
	// Verifying means that the task waits for the services to become
	// ready.
	Verifying TaskState = "verifying"
	// Succeeded means that the deployment reached the expected state.
	Succeeded TaskState = "succeeded"
	// Failed means that the deployment failed.
	Failed TaskState = "failed"
	// Uncertain means that the result of the deployment could not be
	// proven.
	Uncertain TaskState = "uncertain"
)

// Task is a deployment task of an application.
type Task struct {
	// ID is the identifier of the task.
	ID string `json:"id"`
	// AppID is the application the task deploys.
	AppID string `json:"app_id"`
	// VersionID is the version the task deploys.
	VersionID string `json:"version_id"`
	// AgentID is the agent that executes the task.
	AgentID string `json:"agent_id"`
	// State is the state of the task.
	State TaskState `json:"state"`
	// LastSeq is the sequence number of the last receipt of the task.
	LastSeq uint64 `json:"last_seq"`
	// Digest identifies the configuration the task deploys.
	Digest string `json:"digest"`
}

// Command is the instruction an agent receives for a deployment task.
type Command struct {
	// Task is the task the command belongs to.
	Task Task `json:"task"`
	// App is the application the command deploys.
	App App `json:"app"`
	// Config is the configuration the command deploys.
	Config Config `json:"config"`
	// RemovedServices are the services the administrator confirmed the
	// removal of.
	RemovedServices []string `json:"removed_services"`
	// ExecutionTimeout bounds the execution of the Docker command.
	ExecutionTimeout time.Duration `json:"execution_timeout_ns"`
	// VerifyTimeout bounds the wait for the services to become ready.
	VerifyTimeout time.Duration `json:"verify_timeout_ns"`
}

// Result is the outcome of a deployment task.
type Result struct {
	// State is the state the task ended in.
	State TaskState `json:"state"`
	// Code names the kind of the outcome.
	Code string `json:"code"`
	// Message describes the outcome.
	Message string `json:"message"`
	// RolledBack reports whether the target rolled the change back.
	RolledBack bool `json:"rolled_back"`
	// PartialUpdate reports whether only part of the application changed.
	PartialUpdate bool `json:"partial_update"`
}

// Receipt is the report of an agent about a deployment task.
type Receipt struct {
	// TaskID is the task the receipt belongs to.
	TaskID string `json:"task_id"`
	// Digest identifies the configuration the receipt belongs to.
	Digest string `json:"digest"`
	// Seq counts the receipts of a task.
	Seq uint64 `json:"seq"`
	// Result is the outcome the receipt reports.
	Result Result `json:"result"`
}
