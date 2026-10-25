package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"hmoob-dock/internal/config"
)

func TestLoadReadsEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMOOB_DB_PATH", filepath.Join(dir, "data", "control.db"))
	t.Setenv("HMOOB_KEY_FILE", filepath.Join(dir, "secrets", "master.key"))
	t.Setenv("HMOOB_BOOTSTRAP_FILE", filepath.Join(dir, "bootstrap.token"))
	t.Setenv("HMOOB_PUBLIC_URL", "https://hmoob.example.com")
	t.Setenv("HMOOB_TLS_CERT", filepath.Join(dir, "cert.pem"))
	t.Setenv("HMOOB_TLS_KEY", filepath.Join(dir, "key.pem"))
	t.Setenv("HMOOB_WEB_DIR", filepath.Join(dir, "web"))

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != filepath.Join(dir, "data", "control.db") {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
	if cfg.KeyFile != filepath.Join(dir, "secrets", "master.key") {
		t.Fatalf("KeyFile = %q", cfg.KeyFile)
	}
	if cfg.BootstrapFile != filepath.Join(dir, "bootstrap.token") {
		t.Fatalf("BootstrapFile = %q", cfg.BootstrapFile)
	}
	if cfg.PublicURL != "https://hmoob.example.com" {
		t.Fatalf("PublicURL = %q", cfg.PublicURL)
	}
	if cfg.TLSCert != filepath.Join(dir, "cert.pem") || cfg.TLSKey != filepath.Join(dir, "key.pem") {
		t.Fatalf("TLS configuration = %q/%q", cfg.TLSCert, cfg.TLSKey)
	}
	if cfg.WebDir != filepath.Join(dir, "web") {
		t.Fatalf("WebDir = %q", cfg.WebDir)
	}
}

func TestLoadReadsTheTrustedProxyNetworks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMOOB_DB_PATH", filepath.Join(dir, "data", "control.db"))
	t.Setenv("HMOOB_KEY_FILE", filepath.Join(dir, "secrets", "master.key"))
	t.Setenv("HMOOB_TRUSTED_PROXY_CIDRS", "10.0.0.0/8, 2001:db8::/32")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"10.0.0.0/8", "2001:db8::/32"}
	if len(cfg.TrustedProxyCIDRs) != len(want) {
		t.Fatalf("TrustedProxyCIDRs = %v, want %v", cfg.TrustedProxyCIDRs, want)
	}
	for index, network := range want {
		if cfg.TrustedProxyCIDRs[index] != network {
			t.Fatalf("TrustedProxyCIDRs = %v, want %v", cfg.TrustedProxyCIDRs, want)
		}
	}
}

func TestLoadTrustsNoProxyByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMOOB_DB_PATH", filepath.Join(dir, "data", "control.db"))
	t.Setenv("HMOOB_KEY_FILE", filepath.Join(dir, "secrets", "master.key"))
	t.Setenv("HMOOB_TRUSTED_PROXY_CIDRS", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TrustedProxyCIDRs) != 0 {
		t.Fatalf("TrustedProxyCIDRs = %v, want no trusted proxy", cfg.TrustedProxyCIDRs)
	}
}

func TestLoadKeepsKeyFileOutsideDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMOOB_DB_PATH", filepath.Join(dir, "data", "control.db"))
	t.Setenv("HMOOB_KEY_FILE", filepath.Join(dir, "data", "master.key"))

	if _, err := config.Load(); err == nil {
		t.Fatal("Load accepted a master key stored inside the database directory")
	}
}

func TestValidateRejectsKeyFileInsideDatabaseDirectoryWithMixedCase(t *testing.T) {
	dir := t.TempDir()

	// The configuration is validated before the database directory and the
	// master key file are created, so neither path exists yet.
	cfg := config.Control{
		DBPath:  filepath.Join(dir, "Data", "control.db"),
		KeyFile: filepath.Join(dir, "dAtA", "master.key"),
	}
	err := cfg.Validate()
	if caseInsensitiveFilesystem() {
		if err == nil {
			t.Fatal("Validate accepted a master key stored inside the database directory")
		}
		return
	}
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// caseInsensitiveFilesystem reports whether the platform compares path names
// with letter case.
func caseInsensitiveFilesystem() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

func TestValidateRejectsIncompleteTLSConfiguration(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Control{
		DBPath:  filepath.Join(dir, "data", "control.db"),
		KeyFile: filepath.Join(dir, "secrets", "master.key"),
		TLSCert: filepath.Join(dir, "cert.pem"),
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a TLS certificate without a TLS key")
	}
}

func TestValidateAcceptsKeyFileOutsideDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Control{
		DBPath:  filepath.Join(dir, "data", "control.db"),
		KeyFile: filepath.Join(dir, "secrets", "master.key"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestLoadMasterKeyReadsExactlyOneKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	key := bytes.Repeat([]byte{5}, config.MasterKeySize)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	got, err := config.LoadMasterKey(path)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("LoadMasterKey returned different bytes")
	}
}

func TestLoadMasterKeyRejectsMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.key")
	if _, err := config.LoadMasterKey(path); err == nil {
		t.Fatal("LoadMasterKey accepted a missing key file")
	}
}

func TestLoadMasterKeyRejectsMalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if _, err := config.LoadMasterKey(path); err == nil {
		t.Fatal("LoadMasterKey accepted a malformed key file")
	}
}

func TestGenerateMasterKeyCreatesOneKeyAndRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "master.key")

	if err := config.GenerateMasterKey(path); err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	key, err := config.LoadMasterKey(path)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if len(key) != config.MasterKeySize {
		t.Fatalf("key length = %d, want %d", len(key), config.MasterKeySize)
	}

	if err := config.GenerateMasterKey(path); err == nil {
		t.Fatal("GenerateMasterKey overwrote an existing key")
	}
	again, err := config.LoadMasterKey(path)
	if err != nil {
		t.Fatalf("LoadMasterKey: %v", err)
	}
	if !bytes.Equal(key, again) {
		t.Fatal("the existing master key was replaced")
	}
}

func TestLoadBootstrapToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.token")
	if err := os.WriteFile(path, []byte("deployment-secret\n"), 0o600); err != nil {
		t.Fatalf("write bootstrap file: %v", err)
	}

	token, err := config.LoadBootstrapToken(path)
	if err != nil {
		t.Fatalf("LoadBootstrapToken: %v", err)
	}
	if token != "deployment-secret" {
		t.Fatalf("token = %q, want %q", token, "deployment-secret")
	}

	if token, err := config.LoadBootstrapToken(""); err != nil || token != "" {
		t.Fatalf("LoadBootstrapToken(\"\") = %q, %v", token, err)
	}
}

func TestLoadAgentReadsEnvironment(t *testing.T) {
	dir := t.TempDir()
	authority := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(authority, []byte("a certificate"), 0o600); err != nil {
		t.Fatalf("write the certificate authority: %v", err)
	}
	t.Setenv("HMOOB_SERVER_URL", "https://hmoob.example.com")
	t.Setenv("HMOOB_CA_FILE", authority)
	t.Setenv("HMOOB_CREDENTIAL_FILE", filepath.Join(dir, "credential.json"))
	t.Setenv("HMOOB_ENROLLMENT_FILE", filepath.Join(dir, "enrollment.token"))
	t.Setenv("HMOOB_DOCKER_SOCKET", "/var/run/docker.sock")
	t.Setenv("HMOOB_WORK_ROOT", filepath.Join(dir, "work"))

	cfg, err := config.LoadAgent()
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if cfg.ServerURL != "https://hmoob.example.com" {
		t.Fatalf("ServerURL = %q", cfg.ServerURL)
	}
	if cfg.CAFile != authority {
		t.Fatalf("CAFile = %q", cfg.CAFile)
	}
	if cfg.CredentialFile != filepath.Join(dir, "credential.json") {
		t.Fatalf("CredentialFile = %q", cfg.CredentialFile)
	}
	if cfg.EnrollmentFile != filepath.Join(dir, "enrollment.token") {
		t.Fatalf("EnrollmentFile = %q", cfg.EnrollmentFile)
	}
	if cfg.DockerSocket != "/var/run/docker.sock" {
		t.Fatalf("DockerSocket = %q", cfg.DockerSocket)
	}
	if cfg.WorkRoot != filepath.Join(dir, "work") {
		t.Fatalf("WorkRoot = %q", cfg.WorkRoot)
	}
}

func TestLoadAgentRefusesAnAddressWithoutTLS(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMOOB_SERVER_URL", "http://hmoob.example.com")
	t.Setenv("HMOOB_CA_FILE", "")
	t.Setenv("HMOOB_CREDENTIAL_FILE", filepath.Join(dir, "credential.json"))
	t.Setenv("HMOOB_ENROLLMENT_FILE", "")
	t.Setenv("HMOOB_DOCKER_SOCKET", "/var/run/docker.sock")
	t.Setenv("HMOOB_WORK_ROOT", filepath.Join(dir, "work"))

	if _, err := config.LoadAgent(); err == nil {
		t.Fatal("LoadAgent accepted a control端 without TLS")
	}
}

func TestLoadAgentRequiresTheAddressOfTheControl端(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMOOB_SERVER_URL", "")
	t.Setenv("HMOOB_CA_FILE", "")
	t.Setenv("HMOOB_CREDENTIAL_FILE", filepath.Join(dir, "credential.json"))
	t.Setenv("HMOOB_ENROLLMENT_FILE", "")
	t.Setenv("HMOOB_DOCKER_SOCKET", "/var/run/docker.sock")
	t.Setenv("HMOOB_WORK_ROOT", filepath.Join(dir, "work"))

	if _, err := config.LoadAgent(); err == nil {
		t.Fatal("LoadAgent accepted a configuration without the address of the control端")
	}
}

func TestLoadAgentRefusesAMissingCertificateAuthority(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HMOOB_SERVER_URL", "https://hmoob.example.com")
	t.Setenv("HMOOB_CA_FILE", filepath.Join(dir, "missing-ca.pem"))
	t.Setenv("HMOOB_CREDENTIAL_FILE", filepath.Join(dir, "credential.json"))
	t.Setenv("HMOOB_ENROLLMENT_FILE", "")
	t.Setenv("HMOOB_DOCKER_SOCKET", "/var/run/docker.sock")
	t.Setenv("HMOOB_WORK_ROOT", filepath.Join(dir, "work"))

	if _, err := config.LoadAgent(); err == nil {
		t.Fatal("LoadAgent accepted a missing certificate authority")
	}
}
