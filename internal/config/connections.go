package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// ConnectionRegistry contains public routing metadata, never credentials.
type ConnectionRegistry struct {
	SchemaVersion int                   `json:"schema_version"`
	Current       string                `json:"current"`
	Profiles      map[string]Connection `json:"profiles"`
}
type CredentialStore struct {
	Backend            string `json:"backend"`
	AccessTokenService string `json:"access_token_service"`
	PasswordService    string `json:"password_service"`
}
type SecretReference struct {
	Service string `json:"service"`
	Account string `json:"account"`
}
type Connection struct {
	ID                   string             `json:"connection_id"`
	URL                  string             `json:"homeserver_url"`
	ServerName           string             `json:"server_name"`
	CardRoom             string             `json:"card_room,omitempty"`
	Store                CredentialStore    `json:"credential_store"`
	RegistrationTokenRef *SecretReference   `json:"registration_token_ref,omitempty"`
	AdminRegistration    *AdminRegistration `json:"admin_registration,omitempty"`
}

// AdminRegistration is public, explicit admission policy for a pinned Tuwunel dialect.
// It never contains a registration token or secret-store reference.
type AdminRegistration struct {
	Method               string   `json:"method"`
	AdminUserID          string   `json:"admin_user_id"`
	ServerBotUserID      string   `json:"server_bot_user_id"`
	RoomID               string   `json:"room_id"`
	ExpectedMembers      []string `json:"expected_members"`
	ReplyDeadlineSeconds int      `json:"reply_deadline_seconds"`
}

func (r AdminRegistration) validate(server string) error {
	localUser := func(s string) bool {
		local, suffix, ok := strings.Cut(strings.TrimPrefix(s, "@"), ":")
		return len(s) <= 255 && strings.HasPrefix(s, "@") && ok && local != "" && !strings.Contains(local, "@") && suffix == server && !strings.ContainsAny(s, " \t\r\n\x00")
	}
	if r.Method != "tuwunel-v1.9.4-admin-room" || !localUser(r.AdminUserID) || !localUser(r.ServerBotUserID) || r.AdminUserID == r.ServerBotUserID || r.ReplyDeadlineSeconds < 1 || r.ReplyDeadlineSeconds > 30 {
		return errors.New("invalid explicit admin registration prerequisites")
	}
	// This pinned Tuwunel creates its admin room with a server-qualified v11 ID.
	room, suffix, ok := strings.Cut(strings.TrimPrefix(r.RoomID, "!"), ":")
	if len(r.RoomID) > 255 || !strings.HasPrefix(r.RoomID, "!") || !ok || room == "" || suffix != server || strings.ContainsAny(r.RoomID, " \t\r\n\x00") || len(r.ExpectedMembers) < 2 || len(r.ExpectedMembers) > 64 {
		return errors.New("admin registration requires a local room and exact members")
	}
	seen := map[string]bool{}
	for _, m := range r.ExpectedMembers {
		if !localUser(m) || seen[m] {
			return errors.New("invalid expected admin room membership")
		}
		seen[m] = true
	}
	if !seen[r.AdminUserID] || !seen[r.ServerBotUserID] {
		return errors.New("expected members must include admin and server bot")
	}
	return nil
}

// ConnectionBinding is the complete, nonsecret operational identity snapshot.
// Its digest excludes current, labels, unrelated profiles and file formatting.
type ConnectionBinding struct {
	Connection Connection `json:"connection"`
	UserID     string     `json:"user_id"`
	Class      string     `json:"class"`
}

var connectionSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var serviceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)
var cardAliasLocal = regexp.MustCompile(`^[A-Za-z0-9._=\-]+$`)

// DecodeConnectionJSON refuses duplicate keys at every depth, unknown fields,
// trailing values and excessive nesting. Diagnostics never reproduce input.
func DecodeConnectionJSON(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return errors.New("connection JSON nesting limit")
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				keys := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					s, ok := k.(string)
					// encoding/json accepts case aliases of struct fields. A
					// shadow card_room must not silently select another store.
					_, registry := dst.(*ConnectionRegistry)
					_, connection := dst.(*Connection)
					if (registry && depth == 2 || connection && depth == 0) && strings.EqualFold(s, "card_room") {
						s = "card_room"
					}
					// Credentials folds this optional field's spelling. Refuse
					// aliases up front so a rewrite cannot create duplicate keys
					// that brick the next strict named-metadata load. Opaque
					// nested object members retain their existing semantics.
					if _, credentials := dst.(*Credentials); credentials && depth == 0 && strings.EqualFold(s, "wait_keywords") {
						s = "wait_keywords"
					}
					if !ok || keys[s] {
						return errors.New("duplicate or invalid JSON key")
					}
					keys[s] = true
					if e = value(depth + 1); e != nil {
						return e
					}
				}
			case '[':
				for d.More() {
					if e = value(depth + 1); e != nil {
						return e
					}
				}
			default:
				return errors.New("invalid JSON delimiter")
			}
			_, e = d.Token()
			return e
		}
		return nil
	}
	if err := value(0); err != nil {
		return errors.New("invalid connection JSON (including duplicate keys)")
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing connection JSON")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var err error
	if c, ok := dst.(*Credentials); ok {
		// Credentials.UnmarshalJSON bypasses DisallowUnknownFields; keep
		// strict decoding explicit here, including nested binding fields.
		err = c.decodeJSON(data, true)
	} else {
		err = d.Decode(dst)
	}
	if err != nil {
		return errors.New("invalid or unknown connection fields")
	}
	return nil
}
func (c Connection) Validate() error {
	if !connectionSlug.MatchString(c.ID) {
		return errors.New("invalid connection_id")
	}
	if err := ValidateMatrixRouting(c.URL, c.ServerName); err != nil {
		return err
	}
	if _, err := c.CardRoomRef(); err != nil {
		return err
	}
	switch c.Store.Backend {
	case "keychain":
		if !serviceName.MatchString(c.Store.AccessTokenService) || !serviceName.MatchString(c.Store.PasswordService) || c.Store.AccessTokenService == c.Store.PasswordService {
			return errors.New("invalid or unsupported credential_store")
		}
	case "file":
		if c.Store.AccessTokenService != "" || c.Store.PasswordService != "" {
			return errors.New("file credential_store must not declare keychain service names")
		}
		if c.RegistrationTokenRef != nil {
			return errors.New("file credential_store cannot use keychain registration_token_ref")
		}
	default:
		return errors.New("invalid or unsupported credential_store")
	}
	if r := c.RegistrationTokenRef; r != nil {
		if !serviceName.MatchString(r.Service) || r.Account == "" || len(r.Account) > 255 || strings.ContainsAny(r.Account, "\x00\r\n\t") || r.Service == c.Store.AccessTokenService || r.Service == c.Store.PasswordService {
			return errors.New("invalid independent registration_token_ref")
		}
	}
	if r := c.AdminRegistration; r != nil {
		if e := r.validate(c.ServerName); e != nil {
			return e
		}
	}
	return nil
}

// CardRoomRef qualifies a configured alias against its connection's identity
// server, never the caller's server or the endpoint hostname. Room IDs are
// opaque: their suffix (if any) does not identify the card's home server.
func (c Connection) CardRoomRef() (string, error) {
	r := c.CardRoom
	if r == "" {
		r = "#lobby"
	}
	if len(r) > 255 || strings.IndexFunc(r, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) }) >= 0 {
		return "", errors.New("invalid card_room: use a Matrix alias or room ID")
	}
	if strings.HasPrefix(r, "!") && len(r) > 1 {
		return r, nil
	}
	if strings.HasPrefix(r, "#") {
		local, server, qualified := strings.Cut(r[1:], ":")
		if !qualified {
			server = c.ServerName
		}
		if cardAliasLocal.MatchString(local) && validServerName(server) {
			return "#" + local + ":" + server, nil
		}
	}
	return "", errors.New("invalid card_room: use a Matrix alias or room ID")
}

// ValidateMatrixRouting validates a configured endpoint and its separately
// declared Matrix server name. It performs no discovery or normalization.
func ValidateMatrixRouting(endpoint, server string) error {
	u, e := url.Parse(endpoint)
	if e != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" {
		return errors.New("invalid homeserver_url")
	}
	// HTTP is supported only for explicitly configured loopback test/development servers.
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())) {
		return errors.New("homeserver_url requires HTTPS (or loopback HTTP)")
	}
	if !validServerName(server) {
		return errors.New("invalid Matrix server_name")
	}
	return nil
}
func validServerName(s string) bool {
	if s == "" || len(s) > 255 || strings.ContainsAny(s, "/@?#\\ \t\r\n\x00") {
		return false
	}
	u, e := url.Parse("https://" + s)
	if e != nil || u.Host != s || u.User != nil || u.Hostname() == "" {
		return false
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return false
		}
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) > 253 || strings.Contains(s, ":") && u.Port() == "" {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if len(part) == 0 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}
func (r ConnectionRegistry) Validate() error {
	if r.SchemaVersion != 1 || len(r.Profiles) == 0 || !connectionSlug.MatchString(r.Current) {
		return errors.New("connection registry requires schema_version 1, profiles and explicit current")
	}
	ids := map[string]bool{}
	for n, c := range r.Profiles {
		if !connectionSlug.MatchString(n) {
			return errors.New("invalid connection profile name")
		}
		if err := c.Validate(); err != nil {
			return fmt.Errorf("profile %s: %w", n, err)
		}
		if ids[c.ID] {
			return errors.New("duplicate connection_id")
		}
		ids[c.ID] = true
	}
	if _, ok := r.Profiles[r.Current]; !ok {
		return errors.New("current connection profile does not exist")
	}
	return nil
}
func (p Paths) ConfigRoot() string {
	if p.Root != "" {
		return p.Root
	}
	return p.Base
}
func (p Paths) metadataRoot() string {
	if p.MetadataBase != "" {
		return p.MetadataBase
	}
	return p.Base
}
func (p Paths) ConnectionsFile() string { return filepath.Join(p.ConfigRoot(), "connections.json") }

// LoadConnections treats only an absent registry as legacy mode. Symlinks and
// other nonregular entries are refused, including broken symlinks.
func LoadConnections(p Paths) (*ConnectionRegistry, error) {
	path := p.ConnectionsFile()
	st, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("connections.json must be a regular file")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	if len(b) > 1<<20 {
		return nil, errors.New("connection registry too large")
	}
	var r ConnectionRegistry
	if e = DecodeConnectionJSON(b, &r); e != nil {
		return nil, e
	}
	if e = r.Validate(); e != nil {
		return nil, e
	}
	return &r, nil
}

// UpdateConnections serializes cooperating writers; validation precedes atomic
// replacement. A callback failure leaves the original bytes unchanged.
func UpdateConnections(p Paths, fn func(*ConnectionRegistry) error) error {
	unlock, e := lockFile(p.ConnectionsFile() + ".lock")
	if e != nil {
		return e
	}
	defer unlock()
	r, e := LoadConnections(p)
	if e != nil {
		return e
	}
	if r == nil {
		return errors.New("no registry: provision an explicit initial current first")
	}
	if e = fn(r); e != nil {
		return e
	}
	if e = r.Validate(); e != nil {
		return e
	}
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	return writeSecretAtomic(p.ConnectionsFile(), "connections", append(b, '\n'))
}
func (c Connection) Binding(name string, human bool) (ConnectionBinding, error) {
	if e := ValidateName(name); e != nil {
		return ConnectionBinding{}, e
	}
	if e := c.Validate(); e != nil {
		return ConnectionBinding{}, e
	}
	class := "agents"
	if human {
		class = "humans"
	}
	return ConnectionBinding{Connection: c, UserID: "@" + name + ":" + c.ServerName, Class: class}, nil
}
func (b ConnectionBinding) Fingerprint() string {
	// Bootstrap admission is not a consumer routing input.
	b.Connection.RegistrationTokenRef = nil
	b.Connection.AdminRegistration = nil
	// Card-store policy does not change the authenticated identity or its secret
	// namespace. Keep existing credential bindings valid when configuring it.
	b.Connection.CardRoom = ""
	data, _ := json.Marshal(b)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func (c Connection) Paths(p Paths, name string, human bool) (Paths, error) {
	if e := c.Validate(); e != nil {
		return Paths{}, e
	}
	base := filepath.Join(p.ConfigRoot(), "connections", c.ID)
	out := Paths{Base: base, Root: p.ConfigRoot(), MetadataBase: base, WakeMuteVersion: p.WakeMuteVersion}
	for _, path := range []string{base, filepath.Join(base, "agents"), filepath.Join(base, "humans")} {
		if err := checkConnectionDirectories(p.ConfigRoot(), path); err != nil {
			return Paths{}, err
		}
	}
	if name == "" {
		return out, nil
	}
	b, e := c.Binding(name, human)
	if e != nil {
		return Paths{}, e
	}
	sum := sha256.Sum256([]byte(b.UserID))
	out.Base = filepath.Join(base, "identities", b.Class, hex.EncodeToString(sum[:]))
	if human {
		agent, err := c.Paths(p, name, false)
		if err != nil {
			return Paths{}, err
		}
		out.AgentStateBase = agent.Base
	}
	if err := checkConnectionDirectories(p.ConfigRoot(), out.Base); err != nil {
		return Paths{}, err
	}
	return out, nil
}

// ValidateBoundCredentials runs before secret-store lookup or client creation.
func ValidateBoundCredentials(c Credentials, want ConnectionBinding) error {
	if c.Password != "" || c.AccessToken != "" && want.Connection.Store.Backend != "file" {
		return errors.New("named metadata must not contain secrets")
	}
	if c.ConnectionBinding == nil || c.ConnectionBinding.Fingerprint() != want.Fingerprint() || c.UserID != want.UserID || c.Homeserver != want.Connection.URL {
		return errors.New("named credential binding mismatch; explicit migration required")
	}
	return nil
}

// PublishBoundCredentials never replaces an existing file. A hard link publishes
// the complete fsynced private file atomically, without an empty public reservation.
func PublishBoundCredentials(p Paths, name string, human bool, c Credentials) error {
	guard, err := LockCredentialMutation(p, name, human)
	if err != nil {
		return err
	}
	defer guard.Close()
	return guard.PublishBoundCredentials(c)
}

func publishBoundCredentials(p Paths, name string, human bool, c Credentials) error {
	path := p.AgentFile(name)
	if human {
		path = p.HumanFile(name)
	}
	if c.ConnectionBinding == nil {
		return errors.New("missing named credential binding")
	}
	if e := ValidateBoundCredentials(c, *c.ConnectionBinding); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".onboard-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if e = f.Chmod(0600); e != nil {
		return e
	}
	data, e := marshalCredentials(c)
	if e != nil {
		return e
	}
	if _, e = f.Write(append(data, '\n')); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Link(tmp, path); e != nil {
		return fmt.Errorf("publish named credentials without replacement: %w", e)
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

// Existing namespace components must be real directories. Same-UID external
// mutation is not a sandbox boundary, but symlinks never select another profile.
func checkConnectionDirectories(root, path string) error {
	rel, e := filepath.Rel(root, path)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("connection path escapes config root")
	}
	cur := root
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		if part != "" {
			cur = filepath.Join(cur, part)
		}
		st, e := os.Lstat(cur)
		if errors.Is(e, os.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("connection namespace must contain real directories")
		}
	}
	return nil
}
