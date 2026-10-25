package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// ErrNoCredential reports that the agent has no credential yet.
var ErrNoCredential = errors.New("agent: the agent has no credential")

// Credential is the credential of the agent at the control端.
//
// The credential lives in a file with restricted permissions. It never
// travels in the address of a request, in the arguments of a command or in
// the log.
type Credential struct {
	// NodeID identifies the node in the control端.
	NodeID string `json:"node_id"`
	// Token is the credential of the node.
	Token string `json:"token"`
}

// LoadCredential reads the credential of the agent from path.
//
// The file must not be readable by the other users of the host: a
// credential that others can read is a credential that is lost.
func LoadCredential(path string) (Credential, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credential{}, ErrNoCredential
		}
		return Credential{}, fmt.Errorf("agent: inspect the credential file %s: %w", path, err)
	}
	if err := checkPermissions(path, info.Mode()); err != nil {
		return Credential{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, fmt.Errorf("agent: read the credential file %s: %w", path, err)
	}

	var credential Credential
	if err := json.Unmarshal(data, &credential); err != nil {
		return Credential{}, fmt.Errorf("agent: the credential file %s is not a valid credential: %w", path, err)
	}
	if credential.NodeID == "" || credential.Token == "" {
		return Credential{}, fmt.Errorf("agent: the credential file %s is incomplete", path)
	}
	return credential, nil
}

// SaveCredential writes the credential of the agent to path.
//
// The file is replaced atomically, so a crash never leaves a half written
// credential behind, and its permissions are 0600, so nobody else on the
// host can read it.
func SaveCredential(path string, credential Credential) error {
	data, err := json.Marshal(credential)
	if err != nil {
		return fmt.Errorf("agent: encode the credential: %w", err)
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("agent: create the directory %s: %w", directory, err)
	}

	temporary, err := os.CreateTemp(directory, ".credential-*")
	if err != nil {
		return fmt.Errorf("agent: create the credential file in %s: %w", directory, err)
	}
	defer func() { _ = os.Remove(temporary.Name()) }()

	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("agent: restrict the credential file %s: %w", temporary.Name(), err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("agent: write the credential file %s: %w", temporary.Name(), err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("agent: write the credential file %s: %w", temporary.Name(), err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("agent: write the credential file %s: %w", temporary.Name(), err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("agent: replace the credential file %s: %w", path, err)
	}
	return nil
}

// checkPermissions refuses a credential file that the other users of the
// host can read.
func checkPermissions(path string, mode fs.FileMode) error {
	if runtime.GOOS == "windows" {
		// The permissions of a file are not part of the file system of
		// Windows, so the check does not apply there.
		return nil
	}
	if mode.Perm()&0o077 != 0 {
		return fmt.Errorf("agent: the credential file %s is readable by others", path)
	}
	return nil
}
