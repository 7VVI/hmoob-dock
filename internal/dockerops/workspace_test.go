package dockerops_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"hmoob-dock/internal/dockerops"
)

// fakeInspector is the inspector of the tests.
//
// The inspector holds the mounts of the container of the agent, so a test can
// prove what the platform does with them.
type fakeInspector struct {
	mounts []dockerops.Mount
	err    error
}

// ContainerMounts returns the mounts of the container of the agent.
func (f *fakeInspector) ContainerMounts(ctx context.Context, id string) ([]dockerops.Mount, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.mounts, nil
}

// TestTheWorkspaceOfAnApplicationIsFixed proves that the directory of an
// application is fixed by the platform: it lies under the work root of the
// agent and it is named after the application.
func TestTheWorkspaceOfAnApplicationIsFixed(t *testing.T) {
	root := t.TempDir()

	workspace, err := dockerops.PrepareWorkspace(root, "app-1")
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	want := filepath.Join(root, "apps", "app-1")
	if workspace.Dir != want {
		t.Fatalf("the directory = %q, want %q", workspace.Dir, want)
	}
	if workspace.File != filepath.Join(want, "deploy.yaml") {
		t.Fatalf("the file = %q, want the configuration of the application", workspace.File)
	}
}

// TestTheWorkspaceRefusesAnUnsafeApplicationID proves that the identifier
// of an application can never name a path of the host.
func TestTheWorkspaceRefusesAnUnsafeApplicationID(t *testing.T) {
	root := t.TempDir()
	for _, appID := range []string{"", "..", ".", "a/b", `a\b`, "a b", "/etc"} {
		if _, err := dockerops.PrepareWorkspace(root, appID); err == nil {
			t.Fatalf("the identifier %q was accepted", appID)
		}
	}
}

// TestTheWorkspaceIsNotReadableByOtherUsers proves that the directory of an
// application belongs to the agent alone.
func TestTheWorkspaceIsNotReadableByOtherUsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the permissions of the files are not the same on every platform")
	}
	root := t.TempDir()

	workspace, err := dockerops.PrepareWorkspace(root, "app-1")
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}
	info, err := os.Stat(workspace.Dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("the permissions of the directory = %o, want 700", mode)
	}
}

// TestTheRenderedConfigurationIsWrittenForTheOwnerOnly proves that the
// configuration of an application, which holds the values of its variables,
// is written with the permissions of the owner alone.
func TestTheRenderedConfigurationIsWrittenForTheOwnerOnly(t *testing.T) {
	root := t.TempDir()
	workspace, err := dockerops.PrepareWorkspace(root, "app-1")
	if err != nil {
		t.Fatalf("prepare the workspace: %v", err)
	}

	payload := []byte("services:\n  api:\n    image: nginx:alpine\n")
	if err := workspace.WriteConfig(payload); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}
	content, err := os.ReadFile(workspace.File)
	if err != nil {
		t.Fatalf("read the configuration: %v", err)
	}
	if string(content) != string(payload) {
		t.Fatalf("the configuration = %q, want %q", content, payload)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(workspace.File)
		if err != nil {
			t.Fatalf("read the file: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("the permissions of the file = %o, want 600", mode)
		}
	}
}

// TestTheMappingIsProvenByTheMountsOfTheOwnContainer proves that the
// platform only accepts a work root whose bind mount has the same source
// and the same destination.
func TestTheMappingIsProvenByTheMountsOfTheOwnContainer(t *testing.T) {
	root := t.TempDir()
	inspector := &fakeInspector{mounts: []dockerops.Mount{
		{Type: "bind", Source: root, Destination: root},
	}}

	if err := dockerops.ProveMapping(context.Background(), inspector, "container-1", root); err != nil {
		t.Fatalf("prove the mapping: %v", err)
	}
}

// TestTheMappingIsRefusedWhenThePathsDiffer proves that a work root that is
// mounted at another path is refused: the relative path of a configuration
// would point at another directory on the host.
func TestTheMappingIsRefusedWhenThePathsDiffer(t *testing.T) {
	root := t.TempDir()
	inspector := &fakeInspector{mounts: []dockerops.Mount{
		{Type: "bind", Source: "/srv/hmoob-dock", Destination: root},
	}}

	err := dockerops.ProveMapping(context.Background(), inspector, "container-1", root)
	if err == nil {
		t.Fatal("the mapping of another path was accepted")
	}
	if !strings.Contains(err.Error(), "变更操作已禁用") {
		t.Fatalf("the error = %q, want a report that the changes are disabled", err)
	}
}

// TestTheMappingIsRefusedWithoutAMountOfTheWorkRoot proves that a work root
// without a bind mount is refused: the existence of a directory proves
// nothing.
func TestTheMappingIsRefusedWithoutAMountOfTheWorkRoot(t *testing.T) {
	root := t.TempDir()
	inspector := &fakeInspector{mounts: []dockerops.Mount{
		{Type: "volume", Source: "data", Destination: filepath.Join(root, "other")},
	}}

	if err := dockerops.ProveMapping(context.Background(), inspector, "container-1", root); err == nil {
		t.Fatal("a work root without a bind mount was accepted")
	}
}

// TestTheMappingIsRefusedWithoutTheContainerOfTheAgent proves that the
// platform does not guess the container of the agent.
func TestTheMappingIsRefusedWithoutTheContainerOfTheAgent(t *testing.T) {
	root := t.TempDir()
	inspector := &fakeInspector{}

	if err := dockerops.ProveMapping(context.Background(), inspector, "", root); err == nil {
		t.Fatal("a mapping without the container of the agent was accepted")
	}
}

// TestExternalFilesStayInsideTheApplicationDirectory proves that the files
// the Docker CLI reads from the file system of the agent have to live in the
// directory of the application.
func TestExternalFilesStayInsideTheApplicationDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "app-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}

	inside := []byte("services:\n  api:\n    image: nginx:alpine\n    env_file:\n      - app.env\n")
	if err := dockerops.CheckExternalFiles(inside, dir); err != nil {
		t.Fatalf("the file of the application was refused: %v", err)
	}

	outside := []byte("services:\n  api:\n    image: nginx:alpine\n    env_file:\n      - ../../secret.env\n")
	if err := dockerops.CheckExternalFiles(outside, dir); err == nil {
		t.Fatal("a file outside of the directory of the application was accepted")
	}

	missing := []byte("services:\n  api:\n    image: nginx:alpine\n    env_file:\n      - missing.env\n")
	if err := dockerops.CheckExternalFiles(missing, dir); err == nil {
		t.Fatal("a file that does not exist was accepted")
	}
}

// TestASymbolicLinkMayNotLeadOutOfTheApplicationDirectory proves that a
// symbolic link inside the directory of the application may not point out
// of it.
func TestASymbolicLinkMayNotLeadOutOfTheApplicationDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a symbolic link needs a privilege on Windows")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "app-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	secret := filepath.Join(root, "secret.env")
	if err := os.WriteFile(secret, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "app.env")); err != nil {
		t.Fatalf("create the symbolic link: %v", err)
	}

	payload := []byte("services:\n  api:\n    image: nginx:alpine\n    env_file:\n      - app.env\n")
	if err := dockerops.CheckExternalFiles(payload, dir); err == nil {
		t.Fatal("a symbolic link out of the directory of the application was accepted")
	}
}

// TestTheLongSyntaxOfAnEnvFileStaysInsideTheApplicationDirectory proves that
// the long syntax of an env_file is checked like the short one: the Docker
// CLI reads the file from the file system of the agent, so the file may not
// lie outside of the directory of the application.
func TestTheLongSyntaxOfAnEnvFileStaysInsideTheApplicationDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "app-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.env"), []byte("A=1\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}

	inside := []byte("services:\n  api:\n    image: nginx:alpine\n" +
		"    env_file:\n      - path: app.env\n        required: false\n")
	if err := dockerops.CheckExternalFiles(inside, dir); err != nil {
		t.Fatalf("the file of the application was refused: %v", err)
	}

	outside := []byte("services:\n  api:\n    image: nginx:alpine\n" +
		"    env_file:\n      - path: ../../secret.env\n        required: false\n")
	if err := dockerops.CheckExternalFiles(outside, dir); err == nil {
		t.Fatal("a file outside of the directory of the application was accepted")
	}

	absolute := []byte("services:\n  api:\n    image: nginx:alpine\n" +
		"    env_file:\n      - path: /etc/passwd\n")
	if err := dockerops.CheckExternalFiles(absolute, dir); err == nil {
		t.Fatal("a file of the host was accepted")
	}
}

// TestASymbolicLinkOfTheLongSyntaxMayNotLeadOutOfTheApplicationDirectory
// proves that a symbolic link of the long syntax of an env_file may not point
// out of the directory of the application.
func TestASymbolicLinkOfTheLongSyntaxMayNotLeadOutOfTheApplicationDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a symbolic link needs a privilege on Windows")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "app-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	secret := filepath.Join(root, "secret.env")
	if err := os.WriteFile(secret, []byte("SECRET=1\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "app.env")); err != nil {
		t.Fatalf("create the symbolic link: %v", err)
	}

	payload := []byte("services:\n  api:\n    image: nginx:alpine\n" +
		"    env_file:\n      - path: app.env\n        required: false\n")
	if err := dockerops.CheckExternalFiles(payload, dir); err == nil {
		t.Fatal("a symbolic link out of the directory of the application was accepted")
	}
}

// TestTheMappingAssertionNeedsThePathOfTheHost proves that an assertion
// without the path of the work root proves nothing and is refused.
func TestTheMappingAssertionNeedsThePathOfTheHost(t *testing.T) {
	if _, _, err := dockerops.ParseMappingAssertion(dockerops.MappingAsserted); err == nil {
		t.Fatal("an assertion without a path was accepted")
	}
	if _, _, err := dockerops.ParseMappingAssertion(dockerops.MappingAsserted + ":relative/path"); err == nil {
		t.Fatal("an assertion with a relative path was accepted")
	}
	if _, asserted, err := dockerops.ParseMappingAssertion(""); err != nil || asserted {
		t.Fatalf("an empty value = (%v, %v), want no assertion", err, asserted)
	}

	host := filepath.Join(t.TempDir(), "hmoob-dock")
	assertion, asserted, err := dockerops.ParseMappingAssertion(dockerops.MappingAsserted + ":" + host)
	if err != nil {
		t.Fatalf("read the assertion: %v", err)
	}
	if !asserted {
		t.Fatal("the assertion was not recognized")
	}
	if assertion.HostPath != host {
		t.Fatalf("the path = %q, want %q", assertion.HostPath, host)
	}
}

// TestARelativeBindIsFound proves that a bind of a relative path of the host
// is told apart from the name of a volume.
func TestARelativeBindIsFound(t *testing.T) {
	relative := []byte("services:\n  api:\n    image: nginx:alpine\n    volumes:\n      - ./data:/data\n")
	if err := dockerops.CheckRelativeBinds(relative); err == nil {
		t.Fatal("a relative bind was accepted")
	}

	long := []byte("services:\n  api:\n    image: nginx:alpine\n" +
		"    volumes:\n      - type: bind\n        source: ../data\n        target: /data\n")
	if err := dockerops.CheckRelativeBinds(long); err == nil {
		t.Fatal("a relative bind of the long syntax was accepted")
	}

	named := []byte("services:\n  api:\n    image: nginx:alpine\n    volumes:\n      - data:/data\n")
	if err := dockerops.CheckRelativeBinds(named); err != nil {
		t.Fatalf("a named volume was refused: %v", err)
	}

	absolute := []byte("services:\n  api:\n    image: nginx:alpine\n    volumes:\n      - /var/lib/data:/data\n")
	if err := dockerops.CheckRelativeBinds(absolute); err != nil {
		t.Fatalf("an absolute bind of the host was refused: %v", err)
	}
}

// TestAnAbsoluteBindOfTheHostIsNotAFileOfTheAgent proves that a bind mount
// of an absolute path of the host is not a file the agent reads: the daemon
// reads it.
func TestAnAbsoluteBindOfTheHostIsNotAFileOfTheAgent(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "app-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}

	payload := []byte("services:\n  api:\n    image: nginx:alpine\n    volumes:\n      - /var/lib/data:/data\n")
	if err := dockerops.CheckExternalFiles(payload, dir); err != nil {
		t.Fatalf("the bind of the host was refused: %v", err)
	}
}

// TestARelativeBindMayNotLeaveTheApplicationDirectory proves that a
// relative bind of the configuration may not reach another application.
func TestARelativeBindMayNotLeaveTheApplicationDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "apps", "app-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}

	payload := []byte("services:\n  api:\n    image: nginx:alpine\n    volumes:\n      - ../other/data:/data\n")
	if err := dockerops.CheckExternalFiles(payload, dir); err == nil {
		t.Fatal("a bind out of the directory of the application was accepted")
	}
}
