// Package config loads and validates the configuration of the control端.
//
// The master key is read here and never leaves this package in readable
// form: callers receive the raw key bytes and hand them to the secret
// package, which stays independent of the configuration.
package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"hmoob-dock/internal/secret"
)

// MasterKeySize is the length of the master key file in bytes.
const MasterKeySize = secret.KeySize

// Control holds the configuration of the control端.
type Control struct {
	// DBPath is the SQLite database file.
	DBPath string
	// KeyFile is the file holding the master key. It must live outside of
	// the database directory.
	KeyFile string
	// BootstrapFile is the file holding the one time administrator
	// bootstrap credential. It is empty when no credential is provided.
	BootstrapFile string
	// PublicURL is the externally reachable base URL of the control端.
	PublicURL string
	// TrustedProxyCIDRs are the networks of the reverse proxies whose
	// forwarding headers the control端 believes. The list is empty by
	// default, so no forwarding header of a peer is trusted.
	TrustedProxyCIDRs []string
	// TLSCert and TLSKey point at the server certificate and its key.
	TLSCert string
	TLSKey  string
	// WebDir is the directory holding the frontend build output.
	WebDir string
}

// Agent holds the configuration of the agent.
//
// The agent connects to the control端 over TLS and verifies the certificate
// of the control端. It never puts its credential into the address of a
// request or into the arguments of a command.
type Agent struct {
	// ServerURL is the base address of the control端. It has to be an
	// HTTPS address, because the agent verifies the certificate and the
	// host name of the control端.
	ServerURL string
	// CAFile is the file holding the certificate authority that signed the
	// certificate of the control端. An empty file means that the
	// certificates of the system are trusted.
	CAFile string
	// CredentialFile is the file holding the credential of the agent. The
	// file is written with the permissions 0600.
	CredentialFile string
	// EnrollmentFile is the file holding the one time enrollment token.
	// The file is only read when the agent has no credential yet.
	EnrollmentFile string
	// DockerSocket is the socket of the local Docker engine.
	DockerSocket string
	// WorkRoot is the directory the agent works in.
	WorkRoot string
}

// LoadAgent reads the configuration of the agent from the environment and
// validates it.
func LoadAgent() (Agent, error) {
	cfg := Agent{
		ServerURL:      strings.TrimSpace(os.Getenv("HMOOB_SERVER_URL")),
		CAFile:         strings.TrimSpace(os.Getenv("HMOOB_CA_FILE")),
		CredentialFile: envOrDefault("HMOOB_CREDENTIAL_FILE", filepath.Join("data", "agent", "credential.json")),
		EnrollmentFile: strings.TrimSpace(os.Getenv("HMOOB_ENROLLMENT_FILE")),
		DockerSocket:   envOrDefault("HMOOB_DOCKER_SOCKET", "/var/run/docker.sock"),
		WorkRoot:       envOrDefault("HMOOB_WORK_ROOT", filepath.Join("data", "agent")),
	}
	if err := cfg.Validate(); err != nil {
		return Agent{}, err
	}
	return cfg, nil
}

// Validate reports whether the agent can be started with.
func (a Agent) Validate() error {
	if strings.TrimSpace(a.ServerURL) == "" {
		return errors.New("config: the address of the control端 is required")
	}
	parsed, err := url.Parse(a.ServerURL)
	if err != nil {
		return fmt.Errorf("config: the address of the control端 %s is malformed: %w", a.ServerURL, err)
	}
	if parsed.Scheme != "https" {
		return errors.New("config: the address of the control端 has to use HTTPS")
	}
	if parsed.Host == "" {
		return errors.New("config: the address of the control端 names no host")
	}
	if strings.TrimSpace(a.CredentialFile) == "" {
		return errors.New("config: a credential file is required")
	}
	if a.CAFile != "" {
		if _, err := os.Stat(a.CAFile); err != nil {
			return fmt.Errorf("config: the certificate authority file %s is not readable: %w", a.CAFile, err)
		}
	}
	return nil
}

// Load reads the control端 configuration from the environment and validates
// it.
func Load() (Control, error) {
	cfg := Control{
		DBPath:            envOrDefault("HMOOB_DB_PATH", filepath.Join("data", "hmoob.db")),
		KeyFile:           envOrDefault("HMOOB_KEY_FILE", filepath.Join("secrets", "master.key")),
		BootstrapFile:     os.Getenv("HMOOB_BOOTSTRAP_FILE"),
		PublicURL:         os.Getenv("HMOOB_PUBLIC_URL"),
		TrustedProxyCIDRs: envList("HMOOB_TRUSTED_PROXY_CIDRS"),
		TLSCert:           os.Getenv("HMOOB_TLS_CERT"),
		TLSKey:            os.Getenv("HMOOB_TLS_KEY"),
		WebDir:            envOrDefault("HMOOB_WEB_DIR", filepath.Join("web", "dist")),
	}
	if err := cfg.Validate(); err != nil {
		return Control{}, err
	}
	return cfg, nil
}

// Validate reports whether the configuration can be started with.
func (c Control) Validate() error {
	if strings.TrimSpace(c.DBPath) == "" {
		return errors.New("config: a database path is required")
	}
	if strings.TrimSpace(c.KeyFile) == "" {
		return errors.New("config: a master key file is required")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("config: the TLS certificate and the TLS key must be configured together")
	}
	return c.checkKeyOutsideDatabaseDirectory()
}

// checkKeyOutsideDatabaseDirectory makes sure the master key is not stored
// next to the database it protects, because losing the database must not
// mean losing the key as well.
func (c Control) checkKeyOutsideDatabaseDirectory() error {
	keyPath, err := filepath.Abs(c.KeyFile)
	if err != nil {
		return fmt.Errorf("config: resolve master key file %s: %w", c.KeyFile, err)
	}
	if resolved, err := filepath.EvalSymlinks(keyPath); err == nil {
		keyPath = resolved
	}
	dbDir, err := filepath.Abs(filepath.Dir(c.DBPath))
	if err != nil {
		return fmt.Errorf("config: resolve database directory of %s: %w", c.DBPath, err)
	}
	if resolved, err := filepath.EvalSymlinks(dbDir); err == nil {
		dbDir = resolved
	}

	// Windows and macOS ignore the letter case of a path, so the
	// comparison has to ignore it as well. The symbolic link resolution
	// above only works for paths that exist already, and the
	// configuration is validated before the database directory and the
	// master key file are created.
	foldedKey := foldPathCase(keyPath)
	foldedDir := foldPathCase(dbDir)
	if foldedKey == foldedDir || strings.HasPrefix(foldedKey, foldedDir+string(os.PathSeparator)) {
		return fmt.Errorf(
			"config: the master key file %s must not be stored inside the database directory %s",
			c.KeyFile, dbDir,
		)
	}
	return nil
}

// foldPathCase returns the path in the form used to compare it. Platforms
// that ignore the letter case of a path compare the lower case form.
func foldPathCase(path string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(path)
	}
	return path
}

// LoadMasterKey reads the master key from path. The file must hold exactly
// MasterKeySize bytes; a missing or malformed file is an error instead of a
// silently replaced key.
func LoadMasterKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config: the master key file %s does not exist", path)
		}
		return nil, fmt.Errorf("config: read the master key file %s: %w", path, err)
	}
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf(
			"config: the master key file %s must hold exactly %d bytes, got %d",
			path, MasterKeySize, len(key),
		)
	}
	return key, nil
}

// GenerateMasterKey writes a new random master key to path. An existing key
// is never replaced: replacing it would make the stored configuration
// unreadable.
func GenerateMasterKey(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config: the master key file %s already exists", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("config: inspect the master key file %s: %w", path, err)
	}

	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("config: generate the master key: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("config: create the master key directory %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return fmt.Errorf("config: write the master key file %s: %w", path, err)
	}
	return nil
}

// LoadBootstrapToken reads the one time administrator bootstrap credential
// from path. The credential is only returned to the caller, it is never
// logged.
func LoadBootstrapToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("config: the bootstrap file %s does not exist", path)
		}
		return "", fmt.Errorf("config: read the bootstrap file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("config: the bootstrap file %s is empty", path)
	}
	return token, nil
}

// envOrDefault returns the value of the environment variable or the given
// fallback when it is unset or empty.
func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// envList returns the comma separated values of the environment variable.
// Empty values are skipped, so a trailing comma does not add an empty
// entry.
func envList(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}
