// Package config resolves the a2a configuration directory, the shared Matrix
// environment file, per-agent credential files, and per-agent wait state.
//
// Layout (default base directory ~/.config/a2a-swarm, override with A2A_CONFIG_DIR):
//
//	matrix.env              A2A_MATRIX_URL, A2A_MATRIX_SERVER_NAME, A2A_MATRIX_REGISTRATION_TOKEN
//	host                    this machine's host slug (a2a host init), mode 0600
//	agents/<name>.json      credentials, mode 0600
//	state/<name>.json       delivery state (cursor, threads, hook sessions), mode 0600
//	state/<name>.mailbox.jsonl  durable mailbox of relevant events and receipts, mode 0600
package config

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
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
// Composite identities (<host>_<project>_<role>, ComposeIdentity) are names too.
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
	DaemonOwner    string // Immutable service-start policy; never loaded from State.
	Base           string
	Root           string // Original registry and host root in named mode.
	MetadataBase   string // Connection-scoped identity metadata root.
	AgentStateBase string // Resolver-selected agent State root for a named human.
	// stateDecode is the owning terminal consumer's private decode memo; nil
	// for every resolved Paths value. It changes no path or routing result.
	stateDecode *StateDecodeScope
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
	return filepath.Join(p.metadataRoot(), "agents", name+".json")
}

// HumanFile is the credential file of a HUMAN account (e.g. an operator's own
// login). It lives outside agents/ so that agents enumerating their identities
// never find it, and the CLI loads it only with the explicit --human flag.
func (p Paths) HumanFile(name string) string {
	return filepath.Join(p.metadataRoot(), "humans", name+".json")
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
	return loadMatrix(p, getenv, false)
}

// LoadMatrixRouting reads only public routing keys. In particular it neither
// retains registration-token file values nor asks getenv for any secret.
func LoadMatrixRouting(p Paths, getenv Getenv) (Matrix, error) {
	return loadMatrix(p, getenv, true)
}

func loadMatrix(p Paths, getenv Getenv, routingOnly bool) (Matrix, error) {
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
			k = strings.TrimSpace(k)
			if routingOnly && k != EnvMatrixURL && k != EnvServerName {
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
		URL:        strings.TrimRight(get(EnvMatrixURL), "/"),
		ServerName: get(EnvServerName),
	}
	if !routingOnly {
		m.RegistrationToken = get(EnvRegistrationToken)
	}
	return m, nil
}

// Credentials is the per-agent credential file.
type Credentials struct {
	AdmissionPolicy string `json:"admission_policy,omitempty"`
	// waitKeywordsRaw is immutable optional configuration, including invalid data.
	waitKeywordsRaw waitKeywordValues
	WaitKeywords    WaitKeywords `json:"wait_keywords,omitempty"`
	// OwnRoom pins the operator-declared own project/lane route; no room-type discovery.
	OwnRoom           string             `json:"own_room,omitempty"`
	ConnectionBinding *ConnectionBinding `json:"connection_binding,omitempty"`
	UserID            string             `json:"user_id"`
	AccessToken       string             `json:"access_token,omitempty"`
	DeviceID          string             `json:"device_id"`
	Password          string             `json:"password,omitempty"`
	Homeserver        string             `json:"homeserver"`
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
	if err := CheckIdentityMigration(p, name); err != nil {
		return Credentials{}, err
	}
	if err := ValidateName(name); err != nil {
		return Credentials{}, err
	}
	path := p.AgentFile(name)
	if err := checkSecretFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, herr := os.Lstat(p.HumanFile(name)); herr == nil {
				return Credentials{}, fmt.Errorf("%q is a human's identity; an agent must never act as a human (a human operator passes --human)", name)
			}
			return Credentials{}, fmt.Errorf("no credentials for agent %q (expected %s); never borrow another identity: after your owner confirms, run a2a identity ensure --role <your-role>", name, path)
		}
		return Credentials{}, err
	}
	return readCredentials(path)
}

// LoadHumanCredentials reads humans/<name>.json with the same checks.
func LoadHumanCredentials(p Paths, name string) (Credentials, error) {
	if err := CheckIdentityMigration(p, name); err != nil {
		return Credentials{}, err
	}
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
	guard, err := LockCredentialMutation(p, name, false)
	if err != nil {
		return "", err
	}
	defer guard.Close()
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
	c, err := readCredentialMetadata(path)
	if err != nil {
		return Credentials{}, err
	}
	if c.AccessToken == "" {
		return Credentials{}, fmt.Errorf("%s is missing access_token", path)
	}
	return c, nil
}

func readCredentialMetadata(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		// Never include the file content: it holds secrets.
		return Credentials{}, fmt.Errorf("%s is not valid credential JSON", path)
	}
	if c.UserID == "" {
		return Credentials{}, fmt.Errorf("%s is missing user_id", path)
	}
	return c, nil
}

// CreateCredentialFile reserves agents/<name>.json with O_EXCL and mode 0600,
// before any network call, so an existing file is never overwritten.
// The returned reservation retains the credential lock. WriteCredentials closes
// it on success; callers use Abort on failure while the lock is still held.
func CreateCredentialFile(p Paths, name string) (_ *CredentialFile, err error) {
	guard, err := LockCredentialMutation(p, name, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			guard.Close()
		}
	}()
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
	return &CredentialFile{File: f, guard: guard}, nil
}

// WriteCredentials writes c into a file created by CreateCredentialFile and closes it.
func WriteCredentials(f *CredentialFile, c Credentials) error {
	if f.guard.release == nil {
		return ErrMigrationStateUnavailable
	}
	c.Password = ""
	data, err := marshalCredentials(c)
	if err != nil {
		f.Abort()
		return err
	}
	data = append(data, '\n')
	if _, err := f.Write(data); err != nil {
		f.Abort()
		return err
	}
	if err := f.File.Close(); err != nil {
		f.Abort()
		return err
	}
	f.guard.Close()
	return nil
}

// State is the per-agent delivery state. The events themselves live in the
// durable mailbox (MailboxFile), which is written before NextBatch moves.
type State struct {
	// opaque retains unknown top-level values as data, never as authority.
	// It is immutable after loading; saving a State copy must not mutate it.
	opaque map[string]json.RawMessage
	// rawState is the immutable disk input, not a serialization of edited fields.
	// The writer seal must be checked against this input before callbacks/effects.
	rawState      []byte
	Migration     *IdentityMigration `json:"migration,omitempty"`
	Writer        *StateWriter       `json:"writer,omitempty"`
	Monitor       *Monitor           `json:"monitor,omitempty"`
	MonitorOwner  *MonitorOwner      `json:"monitor_owner,omitempty"`
	MonitorConfig *MonitorConfig     `json:"monitor_config,omitempty"`

	MailAlarmConfig *MailAlarmConfig `json:"mail_alarm_config,omitempty"`
	MailAlarm       MailAlarm        `json:"mail_alarm,omitempty"`
	// HostEnrollment is opt-in, source-owned custody; it does not enable delivery.
	HostEnrollment *HostEnrollment `json:"host_enrollment,omitempty"`
	// Notification spaces fixed doorbells; it never receipts or owns channel safety.
	Notification Notification `json:"notification,omitempty"`
	// NextBatch is the sync cursor: every relevant event before it is in the
	// mailbox (appended and fsynced before the cursor was saved).
	NextBatch string `json:"next_batch,omitempty"`
	// LastInbox names the last explicitly saved inbox frame; it grants no delivery authority.
	LastInbox string `json:"last_inbox,omitempty"`
	// AgentDMContexts is conservative room classification, never authority.
	// Keep its JSON opaque here: a feature-local shape fault must not prevent
	// otherwise valid principal delivery or erase the original recovery input.
	AgentDMContexts json.RawMessage `json:"agent_dm_contexts,omitempty"`
	// LegacyPending is the pre-mailbox pending wait batch. It is dropped at the
	// first sync: the cursor never moved past it, so its events are synced
	// into the mailbox again.
	LegacyPending json.RawMessage `json:"pending,omitempty"`
	// LegacyAcked is the pre-mailbox acknowledged-id ring; the first sync moves
	// it into the mailbox receipt index and drops it.
	LegacyAcked []string `json:"acked,omitempty"`
	// Threads holds thread root event ids where this agent posted or which it created.
	Threads []string `json:"threads,omitempty"`
	// ThreadEvents retains bounded room-scoped membership learned from server relations.
	// It grants no follow status and is independent of mute/follow changes.
	ThreadEvents []ThreadEvent `json:"thread_events,omitempty"`
	// Muted holds thread roots the agent muted explicitly; posting in them or
	// seeing its own events there never re-follows them (only Follow does).
	Muted []string `json:"muted,omitempty"`
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
	// Lease is the one live session that holds this identity (Acquire).
	Lease *Lease `json:"lease,omitempty"`
	// Runtime is a launcher-owned native binding; Wake is its durable submission
	// fence. Neither field is a mailbox receipt or authorization from a peer.
	Runtime  *codexwake.Binding `json:"runtime,omitempty"`
	Terminal *TerminalBinding   `json:"terminal,omitempty"`
	Wake     codexwake.Ledger   `json:"wake,omitempty"`
	Liveness *AgentStatus       `json:"liveness,omitempty"`
	// RuntimeProfilePending retains the desired bind profile until an ACK for
	// this generation, independently of whether any liveness record exists.
	RuntimeProfilePending *RuntimeProfilePending `json:"runtime_profile_pending,omitempty"`
	// Knowledge of the identity's status event survives terminal replacement.
	// NoticeStatus acknowledges notice-only publication without enabling liveness.
	NoticeStatus         *AgentStatus             `json:"notice_status,omitempty"`
	NoticeWriteUncertain bool                     `json:"notice_write_uncertain,omitempty"`
	NoticeRetry          *NoticeRetry             `json:"notice_retry,omitempty"`
	OwnStatusWrite       *OwnStatusWrite          `json:"own_status_write,omitempty"`
	LivenessSource       string                   `json:"liveness_source,omitempty"`
	LivenessAlerts       map[string]LivenessAlert `json:"liveness_alerts,omitempty"`
}

type RuntimeProfilePending struct {
	Generation string      `json:"generation"`
	Profile    AgentStatus `json:"profile"`
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

// LegacyAllRooms is the LegacySubscriptions key that matched every joined room.
const LegacyAllRooms = "*"

// ThreadEvent identifies an observed event's thread in one room.
type ThreadEvent struct {
	Room    string `json:"room"`
	EventID string `json:"event_id"`
	Root    string `json:"root"`
}

// ThreadEventRoot never infers membership from the unscoped follow/mute lists.
func (s State) ThreadEventRoot(room, id string) string {
	for i := len(s.ThreadEvents) - 1; i >= 0; i-- {
		e := s.ThreadEvents[i]
		if e.Room == room && e.EventID == id {
			return e.Root
		}
	}
	return ""
}

// RememberThreadEvent retains server-derived membership without following it.
func (s *State) RememberThreadEvent(e ThreadEvent) {
	if e.Room == "" || e.EventID == "" || e.Root == "" || s.ThreadEventRoot(e.Room, e.EventID) != "" {
		return
	}
	s.ThreadEvents = append(s.ThreadEvents, e)
	const max = 2000
	if len(s.ThreadEvents) > max {
		s.ThreadEvents = s.ThreadEvents[len(s.ThreadEvents)-max:]
	}
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

// IsMuted reports whether root was muted explicitly.
func (s State) IsMuted(root string) bool {
	for _, t := range s.Muted {
		if t == root {
			return true
		}
	}
	return false
}

// Mute forgets root and keeps it muted until Follow; it reports whether the
// state changed.
func (s *State) Mute(root string) bool {
	changed := s.RemoveThread(root)
	if root != "" && !s.IsMuted(root) {
		s.Muted = append(s.Muted, root)
		const max = 500
		if len(s.Muted) > max {
			s.Muted = s.Muted[len(s.Muted)-max:]
		}
		changed = true
	}
	return changed
}

// Follow lifts a mute and follows root again; it reports whether the state changed.
func (s *State) Follow(root string) bool {
	changed := false
	for i, t := range s.Muted {
		if t == root {
			s.Muted = append(s.Muted[:i], s.Muted[i+1:]...)
			changed = true
			break
		}
	}
	return s.AddThread(root) || changed
}

// AddThread records root (learned from the agent's own posts or topics); it
// never re-follows an explicitly muted thread. It reports whether the state changed.
func (s *State) AddThread(root string) bool {
	if root == "" || s.HasThread(root) || s.IsMuted(root) {
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
	globalStateStats.loadStateCalls.Add(1)
	path := p.StateFile(name)
	if err := checkSecretFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, nil
		}
		return State{}, err
	}
	globalStateStats.stateReadAttempts.Add(1)
	data, err := os.ReadFile(path)
	if err != nil {
		globalStateStats.stateReadErrors.Add(1)
		return State{}, err
	}
	globalStateStats.stateReadBytes.Add(int64(len(data)))
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) == nil {
		// Match every spelling accepted by the typed JSON decoder. A later
		// non-null alias must not hide a null under another matching key.
		for key, raw := range envelope {
			if strings.EqualFold(key, "host_enrollment") && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return State{}, ErrHostEnrollment
			}
		}
	}
	// The scoped decode reuses parsed data only for these exact fresh bytes and
	// returns an owned copy; the supplemental checks above and below still run.
	s, err := decodeFreshState(p, name, path, data)
	if err != nil {
		return State{}, fmt.Errorf("%s is not valid state JSON: %w", path, err)
	}
	if err := s.validateHostNotifications(); err != nil {
		return State{}, err
	}
	return s, nil
}

// SaveState writes state/<name>.json atomically (temp file + rename) with mode 0600.
func SaveState(p Paths, name string, s State) error {
	if s.HostEnrollment != nil {
		if s.HostEnrollment.Validate() != nil {
			return ErrHostEnrollment
		}
		// Taking the new cooperative state lock must not provision a v2
		// Host namespace that the existing strict writer would refuse.
		if s.HostEnrollment.Version == 2 {
			base, err := os.OpenRoot(p.Base)
			if err != nil {
				return ErrHostStateScope
			}
			if err := base.Close(); err != nil {
				return ErrHostStateScope
			}
		}
	}
	_, err := withStateLock(p, name, lockFile, func(old State) (State, error) {
		if old.Migration != nil {
			return old, ErrIdentityMigrated
		}
		fence := ordinaryWriterFence(old, time.Now())
		return saveStateLocked(p, name, old, s, fence, false, false)
	})
	return err
}

// saveStateLocked never reacquires the state lock. Only monitor lifecycle code
// may alter the monitor record or explicitly clear the sticky writer fence.
func saveStateLocked(p Paths, name string, old, s State, fence time.Time, monitor, migration bool) (State, error) {
	return persistStateLocked(p, name, old, s, fence, monitor, migration, false)
}

// persistStateLocked is saveStateLocked with one ordinary-transaction option.
// inputSealed reports that the caller verified old's compatible-writer seal
// under this same lock (ordinaryWriterFenceSealed). After every refusal below
// has passed, an ordinary non-Host-v2 save whose sealed bytes would equal that
// verified input is not replaced when elideUnchangedOrdinaryState establishes
// the replacement's postconditions on the existing file: same path, mode and
// bytes, file content synced, directory entries synced. Any other outcome
// runs the ordinary replacement or returns its directory sync error taxonomy.
func persistStateLocked(p Paths, name string, old, s State, fence time.Time, monitor, migration, inputSealed bool) (State, error) {
	if !migration && (old.Migration != nil || s.Migration != nil) {
		return old, ErrIdentityMigrated
	}
	if s.HostEnrollment != nil && s.HostEnrollment.Validate() != nil {
		return old, ErrHostEnrollment
	}
	// Refuse lossy writes through this cooperative API once custody exists.
	if !validHostNotificationWrite(old, s) {
		return old, ErrHostEnrollment
	}
	if !monitor && (!sameMonitor(old.Monitor, s.Monitor) || !sameOwner(old.MonitorOwner, s.MonitorOwner)) {
		return old, ErrMonitorConflict
	}
	if s.MonitorOwner != nil && !s.MonitorOwner.valid() || s.MonitorConfig != nil && s.MonitorConfig.OwnerTTL <= 0 {
		return old, ErrMonitorOwner
	}
	if s.Monitor != nil && s.Monitor.Validate() != nil {
		return old, ErrMonitorState
	}
	if !monitor && old.RefuseMonitorConsumer() != nil && !sameMonitorCompanions(old, s) {
		return old, ErrMonitorConflict
	}
	path := p.StateFile(name)
	strict := s.HostEnrollment != nil && s.HostEnrollment.Version == 2
	if !strict {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return old, err
		}
	}
	// Caller-supplied writer metadata never erases an observed fence.
	s.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
	if inputSealed && !strict && unchangedSealedState(old, s) {
		elided, err := elideUnchangedOrdinaryState(path, old.rawState, ordinaryStateSync)
		if err != nil {
			return old, err
		}
		if elided {
			// old is the independent snapshot decoded from these exact bytes; the
			// callback only ever received a separately decoded working copy.
			return old, nil
		}
	}
	data, err := sealedState(s)
	if err != nil {
		return old, err
	}
	if strict {
		err = writeHostStateAtomic(p, name, append(data, '\n'))
	} else {
		err = writeOrdinaryStateAtomic(path, name, append(data, '\n'), syncStateDirectory)
	}
	if err != nil {
		return old, err
	}
	// Return only the representation that successfully persisted, including its
	// fresh seal/input. A failed save makes no durable-fence claim.
	saved, err := decodeFreshState(p, name, path, data)
	if err != nil {
		return old, err
	}
	return saved, nil
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
	guard, err := LockCredentialMutation(p, name, false)
	if err != nil {
		return Credentials{}, err
	}
	defer guard.Close()
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
	next.Password = ""
	data, err := marshalCredentials(next)
	if err != nil {
		return c, err
	}
	// Return the persisted representation, including cleared/updated optional
	// raw state, rather than stale opaque data copied before the callback.
	if err := json.Unmarshal(data, &next); err != nil {
		return c, errors.New("cannot decode rewritten credential JSON")
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

// UpdateStateUntil is UpdateStateBy for a set deadline and UpdateState (a
// blocking lock) for the zero deadline.
func UpdateStateUntil(p Paths, name string, deadline time.Time, fn func(*State) error) (State, error) {
	if deadline.IsZero() {
		return UpdateState(p, name, fn)
	}
	return UpdateStateBy(p, name, deadline, fn)
}

// UpdateStateContext bounds lock acquisition by cancellation and an optional
// earlier deadline. A deadline timeout retains UpdateStateBy's error contract.
func UpdateStateContext(ctx context.Context, p Paths, name string, deadline time.Time, fn func(*State) error) (State, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	st, err := updateState(p, name, func(path string) (func(), error) { return lockFileContext(ctx, path) }, func(st *State) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(st)
	})
	if errors.Is(err, context.DeadlineExceeded) && !deadline.IsZero() && !time.Now().Before(deadline) {
		err = ErrLockTimeout
	}
	return st, err
}

func updateState(p Paths, name string, lock func(string) (func(), error), fn func(*State) error) (State, error) {
	return withStateLock(p, name, lock, func(old State) (State, error) {
		if old.Migration != nil {
			return old, ErrIdentityMigrated
		}
		fence, sealed := ordinaryWriterFenceSealed(old, time.Now())
		// A callback must not mutate the comparison snapshot through shared
		// lease/runtime/terminal pointers or slices. Derive an independent
		// working copy of the same disk input before handing it to ordinary code.
		var s State
		if len(old.rawState) != 0 {
			var err error
			if s, err = writerWorkingCopy(p, name, &old); err != nil {
				return old, err
			}
		}
		s.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
		if err := fn(&s); err != nil {
			return old, err
		}
		return persistStateLocked(p, name, old, s, fence, false, false, sealed)
	})
}

func withStateLock(p Paths, name string, lock func(string) (func(), error), fn func(State) (State, error)) (State, error) {
	if err := ValidateName(name); err != nil {
		return State{}, err
	}
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
	return fn(s)
}
