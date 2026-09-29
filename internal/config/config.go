// Package config resolves the a2a configuration directory, the shared Matrix
// environment file, per-agent credential files, and per-agent wait state.
//
// Layout (default base directory ~/.config/a2a-swarm, override with A2A_CONFIG_DIR):
//
//	matrix.env              A2A_MATRIX_URL, A2A_MATRIX_SERVER_NAME, A2A_MATRIX_REGISTRATION_TOKEN
//	agents/<name>.json      credentials, mode 0600
//	state/<name>.json       delivery state (cursor, pending batch, acked ring, threads), mode 0600
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
	"time"
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

// HumanFile is the credential file of a HUMAN account (e.g. an operator's own
// login). It lives outside agents/ so that agents enumerating their identities
// never find it, and the CLI loads it only with the explicit --human flag.
func (p Paths) HumanFile(name string) string {
	return filepath.Join(p.Base, "humans", name+".json")
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
			if _, herr := os.Lstat(p.HumanFile(name)); herr == nil {
				return Credentials{}, fmt.Errorf("%q is a human's identity; an agent must never act as a human (a human operator passes --human)", name)
			}
			return Credentials{}, fmt.Errorf("no credentials for agent %q (expected %s); never borrow another identity: after your owner confirms, run a2a register <your-session-name>", name, path)
		}
		return Credentials{}, err
	}
	return readCredentials(path)
}

// LoadHumanCredentials reads humans/<name>.json with the same checks.
func LoadHumanCredentials(p Paths, name string) (Credentials, error) {
	if err := ValidateName(name); err != nil {
		return Credentials{}, err
	}
	path := p.HumanFile(name)
	if err := checkSecretFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credentials{}, fmt.Errorf("no human credentials for %q (expected %s)", name, path)
		}
		return Credentials{}, err
	}
	return readCredentials(path)
}

// MoveToHumans moves agents/<name>.json to humans/<name>.json (dir 0700,
// file 0600), refusing to overwrite an existing human file.
func MoveToHumans(p Paths, name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	src, dst := p.AgentFile(name), p.HumanFile(name)
	if err := checkSecretFile(src); err != nil {
		return "", err
	}
	if _, err := os.Lstat(dst); err == nil {
		return "", fmt.Errorf("%s already exists; refusing to overwrite", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func readCredentials(path string) (Credentials, error) {
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

// State is the per-agent delivery state.
type State struct {
	// NextBatch is the committed delivery cursor: `a2a wait` syncs from it, and
	// it moves past delivered events only when their batch is acknowledged.
	NextBatch string `json:"next_batch,omitempty"`
	// Pending is the batch the last `a2a wait` printed but nobody acknowledged yet.
	Pending *Pending `json:"pending,omitempty"`
	// Acked is a bounded ring of the last AckedMax acknowledged event ids; an
	// event in it is never delivered again.
	Acked []string `json:"acked,omitempty"`
	// Threads holds thread root event ids where this agent posted or which it created.
	Threads []string `json:"threads,omitempty"`
	// LegacySubscriptions is the pre-v2 kind subscription map (room id or "*"
	// to kinds). Subscriptions now live in the agent's Matrix account data; the
	// first command that touches them moves this map there and drops it.
	LegacySubscriptions map[string][]string `json:"subscriptions,omitempty"`
	// HookSessions is the per-session doorbell state of the hooks (Claude Code
	// and Codex): the Stop loop guard, the Stop rate limit and the rung set,
	// bounded to HookSessionsMax entries.
	HookSessions []HookSession `json:"hook_sessions,omitempty"`
	// HooksPaused is the hook kill switch (`a2a hook pause`): while set, every
	// hook event prints nothing.
	HooksPaused bool `json:"hooks_paused,omitempty"`
}

// HookSession is the doorbell state of one agent session.
type HookSession struct {
	ID string `json:"id"`
	// Blocks counts consecutive blocking Stop-hook deliveries (loop guard).
	Blocks int `json:"blocks,omitempty"`
	// LastBlock is when the Stop hook last blocked (Unix seconds; rate limit).
	LastBlock int64 `json:"last_block,omitempty"`
	// Rung holds the event ids (and "invite:<room id>" keys) the doorbell
	// announced in this session, at most HookRungMax of them.
	Rung []string `json:"rung,omitempty"`
}

func (h HookSession) empty() bool { return h.Blocks == 0 && h.LastBlock == 0 && len(h.Rung) == 0 }

// HookSessionsMax bounds the per-session map; the least recently updated
// session is dropped first.
const HookSessionsMax = 64

// HookRungMax bounds the rung set of one session.
const HookRungMax = 100

// HookSession returns the doorbell state recorded for id (zero when none).
func (s State) HookSession(id string) HookSession {
	for _, h := range s.HookSessions {
		if h.ID == id {
			return h
		}
	}
	return HookSession{ID: id}
}

// PutHookSession records h, which becomes the most recent entry; an empty
// entry is forgotten, and beyond HookSessionsMax the oldest entries are dropped.
func (s *State) PutHookSession(h HookSession) {
	kept := s.HookSessions[:0:0]
	for _, x := range s.HookSessions {
		if x.ID != h.ID {
			kept = append(kept, x)
		}
	}
	if len(h.Rung) > HookRungMax {
		h.Rung = append([]string(nil), h.Rung[:HookRungMax]...)
	}
	if !h.empty() {
		kept = append(kept, h)
	}
	if len(kept) > HookSessionsMax {
		kept = kept[len(kept)-HookSessionsMax:]
	}
	if len(kept) == 0 {
		kept = nil
	}
	s.HookSessions = kept
}

// HookBlocks returns the consecutive blocking deliveries recorded for id.
func (s State) HookBlocks(id string) int { return s.HookSession(id).Blocks }

// SetHookBlocks records n consecutive blocks for id, keeping the rest of its entry.
func (s *State) SetHookBlocks(id string, n int) {
	h := s.HookSession(id)
	h.Blocks = n
	s.PutHookSession(h)
}

// Pending is a printed but unacknowledged delivery batch.
type Pending struct {
	// NextBatch is where the cursor moves when the batch is acknowledged.
	NextBatch string `json:"next_batch"`
	// Events are the delivered event ids, added to Acked on acknowledgement.
	Events []string `json:"events,omitempty"`
	// CloseSubs are subscriptions whose closing event (until_kind) is in the
	// batch; they are removed on acknowledgement.
	CloseSubs []string `json:"close_subs,omitempty"`
}

// LegacyAllRooms is the LegacySubscriptions key that matched every joined room.
const LegacyAllRooms = "*"

// AckedMax bounds the acknowledged-id ring.
const AckedMax = 1000

// IsAcked reports whether id was acknowledged recently (it is in the ring).
func (s State) IsAcked(id string) bool {
	for _, a := range s.Acked {
		if a == id {
			return true
		}
	}
	return false
}

// AddAcked appends ids to the acknowledged ring, keeping the last AckedMax.
func (s *State) AddAcked(ids ...string) {
	for _, id := range ids {
		if id == "" || s.IsAcked(id) {
			continue
		}
		s.Acked = append(s.Acked, id)
	}
	if len(s.Acked) > AckedMax {
		s.Acked = append([]string(nil), s.Acked[len(s.Acked)-AckedMax:]...)
	}
}

// Ack commits the pending batch: the cursor moves to its next_batch and its
// events join the acknowledged ring. It returns the committed batch (nil when
// nothing was pending).
func (s *State) Ack() *Pending {
	p := s.Pending
	if p == nil {
		return nil
	}
	if p.NextBatch != "" {
		s.NextBatch = p.NextBatch
	}
	s.AddAcked(p.Events...)
	s.Pending = nil
	return p
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

// RemoveThread forgets root; it reports whether the state changed.
func (s *State) RemoveThread(root string) bool {
	for i, t := range s.Threads {
		if t == root {
			s.Threads = append(s.Threads[:i], s.Threads[i+1:]...)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeSecretAtomic(path, name, append(data, '\n'))
}

// writeSecretAtomic replaces path with data: a temporary file in the same
// directory, forced to mode 0600, synced, then renamed over path. A reader sees
// either the old or the new content, never a partial file, and the mode is never wider than 0600.
func writeSecretAtomic(path, name string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+name+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	if err := tmp.Chmod(SecretFileMode); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// ErrUnchanged is returned by an UpdateCredentials callback to skip the write.
var ErrUnchanged = errors.New("credentials unchanged")

// UpdateCredentials runs fn on the current agents/<name>.json under an
// exclusive lock (agents/.<name>.lock) and, unless fn returns ErrUnchanged,
// atomically rewrites the file with the result (temp file + rename in the same
// directory, mode 0600). The existing file must already pass the 0600
// regular-file check; its path never changes. It returns the credentials now on disk.
func UpdateCredentials(p Paths, name string, fn func(*Credentials) error) (Credentials, error) {
	if err := ValidateName(name); err != nil {
		return Credentials{}, err
	}
	path := p.AgentFile(name)
	unlock, err := lockFile(filepath.Join(filepath.Dir(path), "."+name+".lock"))
	if err != nil {
		return Credentials{}, err
	}
	defer unlock()
	c, err := LoadCredentials(p, name)
	if err != nil {
		return Credentials{}, err
	}
	next := c
	if err := fn(&next); err != nil {
		if errors.Is(err, ErrUnchanged) {
			return c, nil
		}
		return c, err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return c, err
	}
	if err := writeSecretAtomic(path, name, append(data, '\n')); err != nil {
		return c, fmt.Errorf("rewrite %s: %w", path, err)
	}
	return next, nil
}

// UpdateState runs fn on the current state under an exclusive lock
// (state/<name>.lock) and saves the result, so that a background `a2a wait`
// and a foreground `a2a post` never lose each other's updates.
func UpdateState(p Paths, name string, fn func(*State) error) (State, error) {
	return updateState(p, name, lockFile, fn)
}

// ErrLockTimeout is returned by UpdateStateBy when another a2a process held
// the state lock until the deadline.
var ErrLockTimeout = errors.New("the state file is locked by another a2a process")

// UpdateStateBy is UpdateState with a bounded wait for the lock: it retries a
// non-blocking lock until deadline and then returns ErrLockTimeout without
// running fn. Hooks use it so a held lock never outlives their budget.
func UpdateStateBy(p Paths, name string, deadline time.Time, fn func(*State) error) (State, error) {
	return updateState(p, name, func(path string) (func(), error) { return lockFileBy(path, deadline) }, fn)
}

func updateState(p Paths, name string, lock func(string) (func(), error), fn func(*State) error) (State, error) {
	dir := filepath.Dir(p.StateFile(name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return State{}, err
	}
	unlock, err := lock(filepath.Join(dir, name+".lock"))
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
