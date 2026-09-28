// Package config resolves the a2a configuration directory, the shared Matrix
// environment file, per-agent credential files, and per-agent wait state.
//
// Layout (default base directory ~/.config/a2a-swarm, override with A2A_CONFIG_DIR):
//
//	matrix.env              A2A_MATRIX_URL, A2A_MATRIX_SERVER_NAME, A2A_MATRIX_REGISTRATION_TOKEN
//	agents/<name>.json      credentials, mode 0600
//	state/<name>.json       wait state (next_batch, threads), mode 0600
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Environment variable names.
const (
	EnvConfigDir         = "A2A_CONFIG_DIR"
	EnvAgent             = "A2A_AGENT"
	EnvMatrixURL         = "A2A_MATRIX_URL"
	EnvServerName        = "A2A_MATRIX_SERVER_NAME"
	EnvRegistrationToken = "A2A_MATRIX_REGISTRATION_TOKEN"
)

// SecretFileMode is the only accepted permission for credential and state files.
const SecretFileMode fs.FileMode = 0o600

// ErrExists is returned when a credential file would be overwritten.
var ErrExists = errors.New("credential file already exists")

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._=-]{0,63}$`)

// ValidateName checks an agent name. Names become Matrix localparts and file
// names, so they are restricted to lowercase letters, digits, and . _ = -.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid agent name %q: use lowercase letters, digits, and . _ = - (max 64, must start with a letter or digit)", name)
	}
	return nil
}

// Getenv abstracts os.Getenv so tests can inject an environment.
type Getenv func(string) string

// Paths resolves files under the base directory.
type Paths struct {
	Base string
}

// ResolvePaths returns the base directory from A2A_CONFIG_DIR or ~/.config/a2a-swarm.
func ResolvePaths(getenv Getenv) (Paths, error) {
	if d := getenv(EnvConfigDir); d != "" {
		return Paths{Base: d}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return Paths{Base: filepath.Join(home, ".config", "a2a-swarm")}, nil
}

// EnvFile is the shared matrix.env path.
func (p Paths) EnvFile() string { return filepath.Join(p.Base, "matrix.env") }

// AgentFile is the credential file for name.
func (p Paths) AgentFile(name string) string {
	return filepath.Join(p.Base, "agents", name+".json")
}

// StateFile is the wait state file for name.
func (p Paths) StateFile(name string) string {
	return filepath.Join(p.Base, "state", name+".json")
}

// Matrix holds the shared homeserver settings.
type Matrix struct {
	URL               string
	ServerName        string
	RegistrationToken string
}

// LoadMatrix reads matrix.env (if present) and lets process environment
// variables override each key. A missing file is not an error as long as the
// required values come from the environment.
func LoadMatrix(p Paths, getenv Getenv) (Matrix, error) {
	vals := map[string]string{}
	f, err := os.Open(p.EnvFile())
	switch {
	case err == nil:
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			v = strings.Trim(v, `"'`)
			vals[strings.TrimSpace(k)] = v
		}
		if err := sc.Err(); err != nil {
			return Matrix{}, fmt.Errorf("read %s: %w", p.EnvFile(), err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return Matrix{}, fmt.Errorf("open %s: %w", p.EnvFile(), err)
	}
	get := func(k string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return vals[k]
	}
	m := Matrix{
		URL:               strings.TrimRight(get(EnvMatrixURL), "/"),
		ServerName:        get(EnvServerName),
		RegistrationToken: get(EnvRegistrationToken),
	}
	return m, nil
}

// Credentials is the per-agent credential file.
type Credentials struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
	Password    string `json:"password"`
	Homeserver  string `json:"homeserver"`
}

// ServerName returns the server part of the user id.
func (c Credentials) ServerName() string {
	if i := strings.Index(c.UserID, ":"); i >= 0 {
		return c.UserID[i+1:]
	}
	return ""
}

// checkSecretFile refuses anything but a regular file with mode 0600.
func checkSecretFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; refusing to use it", path)
	}
	if perm := st.Mode().Perm(); perm != SecretFileMode {
		return fmt.Errorf("%s has mode %04o, want 0600; refusing to use it (fix with: chmod 600 %s)", path, perm, path)
	}
	return nil
}

// LoadCredentials reads agents/<name>.json. The file must be a regular file
// with mode 0600 and must contain a user id and an access token.
func LoadCredentials(p Paths, name string) (Credentials, error) {
	if err := ValidateName(name); err != nil {
		return Credentials{}, err
	}
	path := p.AgentFile(name)
	if err := checkSecretFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credentials{}, fmt.Errorf("no credentials for agent %q (expected %s); run: a2a register %s", name, path, name)
		}
		return Credentials{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		// Never include the file content: it holds secrets.
		return Credentials{}, fmt.Errorf("%s is not valid credential JSON", path)
	}
	if c.UserID == "" || c.AccessToken == "" {
		return Credentials{}, fmt.Errorf("%s is missing user_id or access_token", path)
	}
	return c, nil
}

// CreateCredentialFile reserves agents/<name>.json with O_EXCL and mode 0600,
// before any network call, so an existing file is never overwritten.
// The caller writes the content with WriteCredentials and removes the file on failure.
func CreateCredentialFile(p Paths, name string) (*os.File, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	path := p.AgentFile(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, SecretFileMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w: %s; refusing to overwrite (remove it by hand if the account is really gone)", ErrExists, path)
		}
		return nil, err
	}
	// The umask may have narrowed the mode further; force exactly 0600.
	if err := f.Chmod(SecretFileMode); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return f, nil
}

// WriteCredentials writes c into a file created by CreateCredentialFile and closes it.
func WriteCredentials(f *os.File, c Credentials) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		f.Close()
		return err
	}
	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// State is the per-agent wait state.
type State struct {
	NextBatch string `json:"next_batch,omitempty"`
	// Threads holds thread root event ids where this agent posted or which it created.
	Threads []string `json:"threads,omitempty"`
}

// HasThread reports whether root is a thread this agent participates in.
func (s State) HasThread(root string) bool {
	for _, t := range s.Threads {
		if t == root {
			return true
		}
	}
	return false
}

// AddThread records root; it reports whether the state changed.
func (s *State) AddThread(root string) bool {
	if root == "" || s.HasThread(root) {
		return false
	}
	s.Threads = append(s.Threads, root)
	// Bound the list so that the state file stays small.
	const max = 500
	if len(s.Threads) > max {
		s.Threads = s.Threads[len(s.Threads)-max:]
	}
	return true
}

// LoadState reads state/<name>.json. A missing file yields an empty state.
func LoadState(p Paths, name string) (State, error) {
	path := p.StateFile(name)
	if err := checkSecretFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, nil
		}
		return State{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("%s is not valid state JSON: %w", path, err)
	}
	return s, nil
}

// SaveState writes state/<name>.json atomically (temp file + rename) with mode 0600.
func SaveState(p Paths, name string, s State) error {
	path := p.StateFile(name)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	if err := tmp.Chmod(SecretFileMode); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// UpdateState runs fn on the current state under an exclusive lock
// (state/<name>.lock) and saves the result, so that a background `a2a wait`
// and a foreground `a2a post` never lose each other's updates.
func UpdateState(p Paths, name string, fn func(*State) error) (State, error) {
	dir := filepath.Dir(p.StateFile(name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return State{}, err
	}
	unlock, err := lockFile(filepath.Join(dir, name+".lock"))
	if err != nil {
		return State{}, err
	}
	defer unlock()
	s, err := LoadState(p, name)
	if err != nil {
		return State{}, err
	}
	if err := fn(&s); err != nil {
		return s, err
	}
	return s, SaveState(p, name, s)
}
