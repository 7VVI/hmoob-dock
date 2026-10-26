package dockerops

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The paths and the names of the command line of Docker.
const (
	// DefaultDockerProgram is the path of the Docker command line inside
	// the image of the agent.
	//
	// The path is fixed: the agent runs a command line it ships with and
	// not one that a configuration or an API names.
	DefaultDockerProgram = "/usr/local/bin/docker"
	// DockerProgramEnv names the variable that holds the path of the Docker
	// command line.
	//
	// The variable belongs to the environment of the agent, so an
	// administrator cannot change the program of the node with a
	// configuration.
	DockerProgramEnv = "HMOOB_DOCKER_PROGRAM"
	// DisableEnvFileEnv is the variable of Compose that turns the implicit
	// .env file of the project off.
	//
	// The values of a deployment come from the configuration of the
	// application and from nowhere else. A file that nobody declared must
	// not change the values of a container.
	DisableEnvFileEnv = "COMPOSE_DISABLE_ENV_FILE"
)

// toolPath is the search path of the command line of Docker.
//
// The path holds the directories of the image of the agent and nothing of
// the machine the agent runs on.
const toolPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// RunSpec is one command of the command line of Docker.
//
// The command is described by its parts and not by a line of a shell: the
// arguments never reach a shell, so a value of a variable cannot become a
// command.
type RunSpec struct {
	// Program is the path of the program to run.
	Program string
	// Dir is the directory the program runs in.
	Dir string
	// Args are the arguments of the program.
	Args []string
	// Env are the variables of the program. They are built explicitly: the
	// variables of the agent never reach the command line of Docker.
	Env []string
	// Stdin is the input of the program.
	Stdin []byte
}

// Runner runs a command with a fixed argument list.
//
// The runner is the only way the platform reaches the command line of
// Docker, so a test can replace it and prove which commands run and in
// which order they run.
type Runner interface {
	// Run runs the command and writes its output into the given writer.
	//
	// The error reports that the command failed. The output of the command
	// reaches the writer, so the administrator sees why it failed.
	Run(ctx context.Context, spec RunSpec, output io.Writer) error
}

// ExecRunner runs the commands with the process of the operating system.
//
// The runner runs the program of the image of the agent and nothing else: a
// program that is not a fixed path of the Docker command line is refused.
type ExecRunner struct{}

// Run runs the command of the specification.
//
// The command runs with the environment of the specification and with
// nothing else: the variables of the agent never reach the command line of
// Docker.
func (ExecRunner) Run(ctx context.Context, spec RunSpec, output io.Writer) error {
	if err := checkProgram(spec.Program); err != nil {
		return err
	}
	if output == nil {
		output = io.Discard
	}

	command := exec.CommandContext(ctx, spec.Program, spec.Args...)
	command.Dir = spec.Dir
	command.Env = spec.Env
	if len(command.Env) == 0 {
		// An empty environment would let the command inherit the
		// environment of the agent.
		command.Env = ToolEnv("", "", "")
	}
	command.Stdin = bytes.NewReader(spec.Stdin)
	command.Stdout = output
	command.Stderr = output
	return command.Run()
}

// DockerProgram returns the path of the Docker command line.
//
// The path of the image of the agent is the default. The environment of the
// agent may name another path, for example when the agent does not run in
// the image of the platform.
func DockerProgram() string {
	if value := strings.TrimSpace(os.Getenv(DockerProgramEnv)); value != "" {
		return value
	}
	return DefaultDockerProgram
}

// ToolEnv builds the environment of the command line of Docker.
//
// The program is the path of the command line, so its directory joins the
// search path of the command.
//
// The environment is built explicitly and never inherited as a whole: a
// variable of the agent belongs to the agent and not to the applications it
// deploys. The variables of the operating system are the only ones the
// command line takes over, because they describe the machine and not the
// agent.
//
// The implicit .env file of Compose is turned off, so the values of a
// deployment come from the configuration of the application and from nowhere
// else.
func ToolEnv(program, home, dockerHost string) []string {
	env := systemVariables()
	env = append(env,
		"PATH="+toolPathOf(program),
		DisableEnvFileEnv+"=1",
	)
	if home != "" {
		env = append(env, "HOME="+home)
	}
	if dockerHost != "" {
		env = append(env, "DOCKER_HOST="+dockerHost)
	}
	return env
}

// toolPathOf returns the search path of the command line of Docker.
//
// The path holds the directories of the image of the agent and the directory
// of the command line itself, so the command line finds its plugins and its
// credential helpers next to it. The directory comes from the path the
// platform chose, never from a configuration of an application.
func toolPathOf(program string) string {
	if program == "" {
		return toolPath
	}
	dir := filepath.Dir(program)
	if dir == "" || dir == "." {
		return toolPath
	}
	return toolPath + string(os.PathListSeparator) + dir
}

// systemEnvNames are the variables of the operating system that the command
// line of Docker needs.
//
// The variables of the agent are not part of them. The command line of
// Docker finds its plugins and its configuration with them, so they belong
// to the machine and not to the agent.
var systemEnvNames = []string{
	"SYSTEMROOT",
	"PROGRAMFILES",
	"PROGRAMDATA",
	"USERPROFILE",
	"HOMEDRIVE",
	"HOMEPATH",
	"APPDATA",
	"LOCALAPPDATA",
	"TEMP",
	"TMP",
}

// systemVariables returns the variables of the operating system of the
// process of the agent.
func systemVariables() []string {
	var env []string
	for _, name := range systemEnvNames {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// checkProgram reports whether the program may run.
//
// Only the fixed paths of the Docker command line may run: the program of a
// command never comes from a configuration or from an API.
func checkProgram(program string) error {
	if strings.TrimSpace(program) == "" {
		return fmt.Errorf("dockerops: 命令的程序缺失")
	}
	if !filepath.IsAbs(program) {
		return fmt.Errorf("dockerops: 程序 %q 不是绝对路径", program)
	}
	for _, allowed := range allowedPrograms() {
		if program == allowed {
			return nil
		}
	}
	return fmt.Errorf("dockerops: 程序 %q 不是 Docker 命令行", program)
}

// allowedPrograms returns the paths of the Docker command line the agent may
// run.
//
// The paths of the image of the agent are always allowed. The environment of
// the agent may name one more path, for example when the agent does not run
// in the image of the platform.
func allowedPrograms() []string {
	programs := []string{DefaultDockerProgram, "/usr/bin/docker"}
	if configured := strings.TrimSpace(os.Getenv(DockerProgramEnv)); configured != "" {
		programs = append(programs, configured)
	}
	return programs
}
