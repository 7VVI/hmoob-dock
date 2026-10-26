package dockerops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// The layout of the work directory of the agent.
const (
	// AppsDirName is the name of the directory that holds the applications.
	AppsDirName = "apps"
	// ComposeFileName is the name of the rendered configuration of an
	// application inside of its directory.
	ComposeFileName = "deploy.yaml"
	// AppDirMode is the permission of the directory of an application. The
	// directory belongs to the agent alone.
	AppDirMode = 0o700
	// SecretFileMode is the permission of the files that hold the values of
	// the variables of an application.
	SecretFileMode = 0o600
)

// The variables that describe the environment of the agent.
const (
	// ContainerIDEnv names the variable that holds the ID of the container
	// the agent runs in.
	//
	// The ID is known to the agent, so it can inspect itself and prove
	// that the work root is the same path on the host as it is in the
	// container.
	ContainerIDEnv = "HMOOB_CONTAINER_ID"
	// MappingEnv names the variable that holds the assertion of the
	// operator of the node about the work root of the agent.
	//
	// The value MappingAsserted tells the platform that the operator
	// verified that the work root of the agent is the same absolute path on
	// the host as it is in the container of the agent. The assertion is the
	// decision of the operator of the node and not of the administrator of
	// the platform: the variable belongs to the environment of the agent and
	// cannot be set by a configuration or by an API.
	//
	// The assertion is only needed when the agent does not run in a
	// container, because then there is no container to inspect and no
	// mapping to prove.
	MappingEnv = "HMOOB_WORK_ROOT_MAPPING"
	// MappingAsserted is the value of MappingEnv that accepts the work
	// root without a proof.
	MappingAsserted = "asserted"
)

// appIDPattern matches the identifiers of the applications.
//
// The identifier of an application is a random name, so it holds no path
// separator and no point. The directory of an application is named after it,
// so an identifier that holds a path would let a configuration write
// outside of the work root.
var appIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Workspace is the directory of an application.
//
// The directory is fixed by the platform: it lives under the work root of
// the agent and it is named after the application. Neither a configuration
// nor an API can name another directory.
type Workspace struct {
	// Root is the work root of the agent.
	Root string
	// Dir is the directory of the application.
	Dir string
	// File is the path of the rendered configuration of the application.
	File string
}

// PrepareWorkspace prepares the directory of an application.
//
// The directory is created with the permissions of the agent alone, so no
// other user of the node may read the configuration of an application.
func PrepareWorkspace(root, appID string) (Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return Workspace{}, errors.New("dockerops: 缺少 Agent 的工作目录")
	}
	if !appIDPattern.MatchString(appID) {
		return Workspace{}, fmt.Errorf("dockerops: 应用标识 %q 不是合法的目录名", appID)
	}

	dir := filepath.Join(root, AppsDirName, appID)
	if err := os.MkdirAll(dir, AppDirMode); err != nil {
		return Workspace{}, fmt.Errorf("dockerops: 创建目录 %s 失败: %w", dir, err)
	}
	// The permissions of a directory that exists already are not changed by
	// MkdirAll, so they are set again.
	if err := os.Chmod(dir, AppDirMode); err != nil {
		return Workspace{}, fmt.Errorf("dockerops: 设置目录 %s 的权限失败: %w", dir, err)
	}

	return Workspace{
		Root: root,
		Dir:  dir,
		File: filepath.Join(dir, ComposeFileName),
	}, nil
}

// WriteConfig writes the rendered configuration of the application.
//
// The configuration holds the values of the variables of the application, so
// it is written with the permissions of the owner alone. The values never
// reach the command line of the Docker command, where every user of the
// node could read them from the list of the processes.
func (w Workspace) WriteConfig(payload []byte) error {
	if err := os.WriteFile(w.File, payload, SecretFileMode); err != nil {
		return fmt.Errorf("dockerops: 写入配置 %s 失败: %w", w.File, err)
	}
	// The permissions of a file that exists already are not changed by
	// WriteFile, so they are set again.
	if err := os.Chmod(w.File, SecretFileMode); err != nil {
		return fmt.Errorf("dockerops: 设置文件 %s 的权限失败: %w", w.File, err)
	}
	return nil
}

// Mount is a mount of a container.
type Mount struct {
	// Type is the type of the mount, for example "bind".
	Type string
	// Source is the path the mount comes from.
	Source string
	// Destination is the path the mount is mounted at.
	Destination string
}

// Inspector reads the mounts of a container.
type Inspector interface {
	// ContainerMounts returns the mounts of the container with the given
	// ID.
	ContainerMounts(ctx context.Context, id string) ([]Mount, error)
}

// SelfContainerID returns the ID of the container the agent runs in.
//
// The ID comes from the environment of the agent, from the group of the
// processes of the container or from the mounts of the container. An empty
// string means that the agent does not know its own container, so it cannot
// prove the mapping of its work root.
func SelfContainerID() string {
	if id := strings.TrimSpace(os.Getenv(ContainerIDEnv)); id != "" {
		return id
	}
	for _, path := range []string{"/proc/self/cgroup", "/proc/self/mountinfo"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if id := containerIDPattern.FindString(string(data)); id != "" {
			return id
		}
	}
	return ""
}

// containerIDPattern matches the ID of a container in the paths of the
// processes of a container.
var containerIDPattern = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

// ProveMapping proves that the work root of the agent is the same absolute
// path on the host as it is in the container of the agent.
//
// The proof reads the mounts of the container of the agent: the bind mount
// of the work root has to have the same source and the same destination. A
// directory that exists is not a proof: the directory of the host and the
// directory of the container may be two different directories, and a
// relative path of a configuration would then point at another place.
//
// The error names the reason, so the administrator can fix the deployment of
// the agent.
func ProveMapping(ctx context.Context, inspector Inspector, containerID, workRoot string) error {
	if strings.TrimSpace(containerID) == "" {
		return errors.New("dockerops: 不知道 Agent 自己的容器，无法证明工作目录的路径映射，变更操作已禁用")
	}
	root := filepath.Clean(workRoot)
	if !filepath.IsAbs(root) {
		return fmt.Errorf("dockerops: 工作目录 %q 不是绝对路径", workRoot)
	}
	if inspector == nil {
		return errors.New("dockerops: 没有 Docker 引擎，无法证明工作目录的路径映射，变更操作已禁用")
	}

	mounts, err := inspector.ContainerMounts(ctx, containerID)
	if err != nil {
		return fmt.Errorf("dockerops: 读取容器 %s 的挂载失败: %w", containerID, err)
	}
	for _, mount := range mounts {
		if mount.Type != "bind" {
			continue
		}
		if !samePath(filepath.Clean(mount.Destination), root) {
			continue
		}
		if !samePath(filepath.Clean(mount.Source), root) {
			return fmt.Errorf(
				"dockerops: 工作目录 %q 在宿主机上是 %q，两个路径不一致，变更操作已禁用",
				root, mount.Source)
		}
		return nil
	}
	return fmt.Errorf(
		"dockerops: 容器 %s 没有挂载工作目录 %q，无法证明路径映射，变更操作已禁用",
		containerID, root)
}

// CheckExternalFiles checks the files the configuration refers to.
//
// The Docker CLI reads the files of a configuration from the file system of
// the agent: an env_file, a config or a secret. Only the files of the
// directory of the application may be read, and a symbolic link may not lead
// out of it.
//
// A bind mount of an absolute path of the host is a different thing: the
// daemon reads it, not the agent. The risk of such a bind reaches the
// administrator as a warning of the configuration.
func CheckExternalFiles(payload []byte, dir string) error {
	document := map[string]any{}
	if err := yaml.Unmarshal(payload, &document); err != nil {
		return fmt.Errorf("dockerops: 读取配置失败: %w", err)
	}

	services, _ := document["services"].(map[string]any)
	for name, definition := range services {
		service, ok := definition.(map[string]any)
		if !ok {
			continue
		}
		for _, path := range envFiles(service["env_file"]) {
			if err := checkReadableFile(dir, path); err != nil {
				return fmt.Errorf("dockerops: 服务 %s 的 env_file %q 不可读: %w", name, path, err)
			}
		}
		for _, field := range []string{"configs", "secrets"} {
			for _, path := range filePaths(service[field]) {
				if err := checkReadableFile(dir, path); err != nil {
					return fmt.Errorf("dockerops: 服务 %s 的 %s 文件 %q 不可读: %w", name, field, path, err)
				}
			}
		}
		for _, source := range bindSources(service["volumes"]) {
			if err := checkBindSource(dir, source); err != nil {
				return fmt.Errorf("dockerops: 服务 %s 的绑定 %q 不被允许: %w", name, source, err)
			}
		}
	}

	for _, field := range []string{"configs", "secrets"} {
		definitions, _ := document[field].(map[string]any)
		for _, definition := range definitions {
			entry, ok := definition.(map[string]any)
			if !ok {
				continue
			}
			path, _ := entry["file"].(string)
			if path == "" {
				continue
			}
			if err := checkReadableFile(dir, path); err != nil {
				return fmt.Errorf("dockerops: %s 文件 %q 不可读: %w", field, path, err)
			}
		}
	}
	return nil
}

// checkReadableFile checks a file the Docker CLI reads from the file system
// of the agent.
func checkReadableFile(dir, path string) error {
	target := path
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	if !insideDir(dir, target) {
		return fmt.Errorf("路径在应用目录 %s 之外", dir)
	}

	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("文件不可读: %w", err)
	}
	if !insideDir(resolvedDir(dir), real) {
		return fmt.Errorf("文件指向应用目录 %s 之外", dir)
	}
	return nil
}

// checkBindSource checks a bind of a service.
//
// A relative bind is resolved against the directory of the application, so it
// may not reach another application. An absolute bind of the host is read by
// the daemon and not by the agent, so it is left to the warning of the
// configuration.
func checkBindSource(dir, source string) error {
	if source == "" || filepath.IsAbs(source) {
		return nil
	}
	target := filepath.Join(dir, source)
	if !insideDir(dir, target) {
		return fmt.Errorf("路径指向应用目录 %s 之外", dir)
	}
	if _, err := os.Lstat(target); err != nil {
		// The daemon creates the directory of a bind that does not exist
		// yet, so a missing path is not a refusal.
		return nil
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("路径不可读: %w", err)
	}
	if !insideDir(resolvedDir(dir), real) {
		return fmt.Errorf("路径指向应用目录 %s 之外", dir)
	}
	return nil
}

// envFiles returns the files of an env_file field.
func envFiles(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		var paths []string
		for _, item := range typed {
			if path, ok := item.(string); ok {
				paths = append(paths, path)
			}
		}
		return paths
	default:
		return nil
	}
}

// filePaths returns the files of a configs or a secrets field.
func filePaths(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var paths []string
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if path, ok := entry["file"].(string); ok && path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// bindSources returns the sources of the volumes of a service.
func bindSources(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	var sources []string
	for _, item := range items {
		switch typed := item.(type) {
		case string:
			// The short syntax of a volume is "source:target:mode".
			source, _, _ := strings.Cut(typed, ":")
			sources = append(sources, source)
		case map[string]any:
			if source, ok := typed["source"].(string); ok {
				sources = append(sources, source)
			}
		}
	}
	return sources
}

// insideDir reports whether the path lies inside the directory.
func insideDir(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if samePath(dir, path) {
		return true
	}
	return strings.HasPrefix(path, dir+string(os.PathSeparator))
}

// resolvedDir returns the path of the directory with its symbolic links
// resolved.
func resolvedDir(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return resolved
}

// samePath reports whether the two paths are the same one.
//
// Windows and macOS ignore the letter case of a path, so the comparison has
// to ignore it as well.
func samePath(first, second string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(first, second)
	}
	return first == second
}
