package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

// AgentPresence is a closed, secret-free local snapshot, not credential,
// remote-account, caller-authority, member or runtime qualification.
type AgentPresence struct {
	Schema   string `json:"schema"`
	State    string `json:"state"`
	Evidence string `json:"evidence"`
	Reason   string `json:"reason"`
}

func presence(state, evidence, reason string) AgentPresence {
	return AgentPresence{"a2a.local-identity-presence/1", state, evidence, reason}
}

// UnavailableAgentPresence reports failure to resolve the local config root.
func UnavailableAgentPresence() AgentPresence {
	return inconclusivePresence("unsafe_or_unreadable")
}

func inconclusivePresence(reason string) AgentPresence {
	return presence("inconclusive", "none", reason)
}

// These dedicated public records intentionally cannot decode Credentials.
// Even empty access_token/password fields and case aliases are unknown fields.
type presenceMetadata struct {
	Binding    ConnectionBinding `json:"connection_binding"`
	UserID     string            `json:"user_id"`
	DeviceID   string            `json:"device_id"`
	Homeserver string            `json:"homeserver"`
}

type presenceJournal struct {
	Binding ConnectionBinding `json:"binding"`
	Stage   string            `json:"stage"`
}

// Scan the closed public shape before decoding values into a public record.
// Unknown/secret-bearing keys are rejected before interpreting their values;
// duplicates, aliases, nulls and missing required fields cannot hide them.
func scanPresencePublic(d *json.Decoder, t reflect.Type, depth int) error {
	if depth > 32 {
		return errors.New("public nesting limit")
	}
	if t.Kind() == reflect.Pointer {
		return scanPresencePublic(d, t.Elem(), depth)
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		if token != json.Delim('{') {
			return errors.New("invalid public object")
		}
		known := map[string]reflect.Type{}
		required := map[string]bool{}
		if t.Kind() == reflect.Struct {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				tag := strings.Split(f.Tag.Get("json"), ",")
				known[tag[0]] = f.Type
				if len(tag) == 1 || tag[1] != "omitempty" {
					required[tag[0]] = true
				}
			}
		}
		seen := map[string]bool{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return errors.New("duplicate public key")
			}
			seen[key] = true
			field, ok := known[key]
			if t.Kind() == reflect.Map {
				field, ok = t.Elem(), true
			}
			if !ok {
				return errors.New("unknown public field")
			}
			if err := scanPresencePublic(d, field, depth+1); err != nil {
				return err
			}
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return errors.New("invalid public object end")
		}
		for key := range required {
			if !seen[key] {
				return errors.New("missing public field")
			}
		}
	case reflect.String:
		if _, ok := token.(string); !ok {
			return errors.New("invalid public string")
		}
	case reflect.Int:
		number, ok := token.(json.Number)
		if !ok {
			return errors.New("invalid public number")
		}
		if _, err := number.Int64(); err != nil {
			return errors.New("invalid public integer")
		}
	default:
		return errors.New("unsupported public shape")
	}
	return nil
}

func decodePresencePublic(raw []byte, dst any) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid public JSON encoding")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := scanPresencePublic(d, reflect.TypeOf(dst).Elem(), 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing public JSON")
	}
	return json.Unmarshal(raw, dst)
}

type presenceHooks struct {
	beforeRead    func(kind string) error // private tests only, never environment-selected
	beforeRecheck func()
}

type presenceSnapshot struct {
	rootPath string
	root     *os.Root
	watched  map[string]os.FileInfo // nil records an observed absence
	hooks    presenceHooks
}

func privatePresenceInfo(info os.FileInfo, directory bool) bool {
	if !presenceOwned(info) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode().Perm() == 0700
	}
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600
}

func samePresenceInfo(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() &&
		a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && presenceOwned(b)
}

func openPresenceSnapshot(path string, hooks presenceHooks) (*presenceSnapshot, error) {
	info, err := os.Lstat(path)
	if err != nil || !privatePresenceInfo(info, true) {
		return nil, errors.New("unsafe config root")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !samePresenceInfo(info, opened) {
		root.Close()
		return nil, errors.New("config root changed")
	}
	return &presenceSnapshot{path, root, map[string]os.FileInfo{".": info}, hooks}, nil
}

// stat checks all components below the configured root. Parents above that
// root, ACLs and malicious same-UID ABA changes are not an isolation boundary.
func (s *presenceSnapshot) stat(path string) (os.FileInfo, error) {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := range parts {
		p := filepath.Join(parts[:i+1]...)
		info, err := s.root.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			if before, seen := s.watched[p]; seen && before != nil {
				return nil, errors.New("public path disappeared")
			}
			s.watched[p] = nil
			return nil, nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("unsafe or unreadable public path")
		}
		if i < len(parts)-1 && !privatePresenceInfo(info, true) {
			return nil, errors.New("unsafe public parent")
		}
		if before, ok := s.watched[p]; ok && !samePresenceInfo(before, info) {
			return nil, errors.New("public path changed")
		}
		s.watched[p] = info
		if i == len(parts)-1 {
			return info, nil
		}
	}
	return nil, errors.New("invalid public path")
}

func (s *presenceSnapshot) read(path, kind string, limit int64) ([]byte, bool, error) {
	info, err := s.stat(path)
	if err != nil || info == nil {
		return nil, false, err
	}
	if !privatePresenceInfo(info, false) || info.Size() <= 0 || info.Size() > limit {
		return nil, false, errors.New("unsafe public file")
	}
	if s.hooks.beforeRead != nil {
		if err := s.hooks.beforeRead(kind); err != nil {
			return nil, false, err
		}
	}
	file, err := s.root.OpenFile(path, presenceOpenFlags, 0)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !samePresenceInfo(info, opened) {
		return nil, false, errors.New("public file changed before read")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || int64(len(raw)) != info.Size() || !samePresenceInfo(info, after) {
		return nil, false, errors.New("public file changed during read")
	}
	return raw, true, nil
}

func (s *presenceSnapshot) recheck() bool {
	if s.hooks.beforeRecheck != nil {
		s.hooks.beforeRecheck()
	}
	for path, before := range s.watched {
		var after os.FileInfo
		var err error
		if path == "." {
			after, err = os.Lstat(s.rootPath)
		} else {
			after, err = s.root.Lstat(path)
		}
		if before == nil {
			if !errors.Is(err, os.ErrNotExist) {
				return false
			}
		} else if err != nil || !samePresenceInfo(before, after) {
			return false
		}
	}
	return true
}

func (s *presenceSnapshot) stateEntries(path string) (bool, error) {
	info, err := s.stat(path)
	if err != nil || info == nil {
		return false, err
	}
	if !privatePresenceInfo(info, true) {
		return false, errors.New("unsafe state namespace")
	}
	dir, err := s.root.OpenFile(path, presenceOpenFlags, 0)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	opened, err := dir.Stat()
	if err != nil || !samePresenceInfo(info, opened) {
		return false, errors.New("state namespace changed")
	}
	entries, err := dir.ReadDir(1025)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) > 1024 {
		return false, errors.New("unreadable or excessive state namespace")
	}
	return len(entries) > 0, nil
}

// InspectAgentPresence never loads credentials or secrets, sends HTTP, acquires
// writer locks or creates directories. It does not authenticate its caller.
func InspectAgentPresence(p Paths, name, profile, fingerprint string, getenv Getenv) AgentPresence {
	return inspectAgentPresence(p, name, profile, fingerprint, getenv, presenceHooks{})
}

func inspectAgentPresence(p Paths, name, profile, fingerprint string, getenv Getenv, hooks presenceHooks) AgentPresence {
	if ValidateName(name) != nil {
		return inconclusivePresence("invalid_target")
	}
	snapshot, err := openPresenceSnapshot(p.ConfigRoot(), hooks)
	if err != nil {
		return inconclusivePresence("unsafe_or_unreadable")
	}
	defer snapshot.root.Close()
	result := snapshot.inspect(name, profile, fingerprint, getenv)
	if !snapshot.recheck() {
		return inconclusivePresence("snapshot_changed")
	}
	return result
}

func (s *presenceSnapshot) inspect(name, profile, fingerprint string, getenv Getenv) AgentPresence {
	raw, exists, err := s.read("connections.json", "registry", 1<<20)
	if err != nil {
		return inconclusivePresence("invalid_registry")
	}
	if !exists {
		if profile != "" || fingerprint != "" {
			return inconclusivePresence("invalid_selection")
		}
		return s.legacy(name)
	}
	var registry ConnectionRegistry
	if decodePresencePublic(raw, &registry) != nil || registry.Validate() != nil {
		return inconclusivePresence("invalid_registry")
	}
	connection, ok := registry.Profiles[profile]
	if profile == "" || !ok {
		return inconclusivePresence("invalid_selection")
	}
	binding, err := connection.Binding(name, false)
	if err != nil || fingerprint != "" && fingerprint != binding.Fingerprint() {
		return inconclusivePresence("binding_mismatch")
	}
	if !s.routing(connection, getenv) {
		return inconclusivePresence("routing_conflict")
	}
	base := filepath.Join("connections", connection.ID)
	human, err := s.stat(filepath.Join(base, "humans", name+".json"))
	if err != nil || human != nil {
		return inconclusivePresence("identity_class")
	}
	raw, metadataExists, err := s.read(filepath.Join(base, "agents", name+".json"), "metadata", 64<<10)
	if err != nil {
		return inconclusivePresence("invalid_metadata")
	}
	if metadataExists {
		var metadata presenceMetadata
		if decodePresencePublic(raw, &metadata) != nil || !validPresenceBinding(metadata.Binding, binding) ||
			metadata.UserID != binding.UserID || metadata.Homeserver != connection.URL || !safePresenceText(metadata.DeviceID, 255) {
			return inconclusivePresence("invalid_metadata")
		}
	}
	sum := sha256.Sum256([]byte(binding.UserID))
	state := filepath.Join(base, "identities", "agents", hex.EncodeToString(sum[:]))
	raw, journalExists, err := s.read(filepath.Join(state, "registration.pending.json"), "journal", 64<<10)
	if err != nil {
		return inconclusivePresence("invalid_journal")
	}
	stage := ""
	if journalExists {
		var journal presenceJournal
		if decodePresencePublic(raw, &journal) != nil || !validPresenceBinding(journal.Binding, binding) || !knownPresenceStage(journal.Stage) {
			return inconclusivePresence("invalid_journal")
		}
		stage = journal.Stage
	}
	residue, err := s.stateEntries(state)
	if err != nil {
		return inconclusivePresence("unsafe_or_unreadable")
	}
	if journalExists && stage != "complete" {
		return presence("pending", "registration_journal", "registration_incomplete")
	}
	if metadataExists {
		return presence("present", "named_bound_metadata", "validated")
	}
	if residue {
		return inconclusivePresence("unqualified_state")
	}
	return presence("absent", "none", "local_absence")
}

func validPresenceBinding(got, want ConnectionBinding) bool {
	return got.Connection.Validate() == nil && got.Fingerprint() == want.Fingerprint() && got.UserID == want.UserID && got.Class == "agents"
}

func safePresenceText(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func knownPresenceStage(stage string) bool {
	switch stage {
	case "preflight", "secret-preflight-refused", "bootstrap-unavailable", "password-store-failed",
		"remote-outcome-pending", "remote-outcome-ambiguous", "remote-rejected", "remote-response-mismatch",
		"remote-created-token-storage-pending", "remote-created-token-store-failed", "remote-created-metadata-publication-pending", "complete":
		return true
	}
	return false
}

func (s *presenceSnapshot) legacy(name string) AgentPresence {
	human, err := s.stat(filepath.Join("humans", name+".json"))
	if err != nil || human != nil {
		return inconclusivePresence("identity_class")
	}
	journal, err := s.stat("registration.pending.json")
	if err != nil || journal != nil {
		return inconclusivePresence("unqualified_state")
	}
	info, err := s.stat(filepath.Join("agents", name+".json"))
	if err != nil {
		return inconclusivePresence("unsafe_or_unreadable")
	}
	if info != nil {
		if !privatePresenceInfo(info, false) || info.Size() <= 0 || info.Size() > 1<<20 {
			return inconclusivePresence("unsafe_or_unreadable")
		}
		// Deliberately stat-only: even arbitrary token-bearing bytes are unopened.
		return presence("present", "legacy_regular_file_only", "regular_file_only")
	}
	// Bounded directory entries identify only target-related residue. Never open
	// delivery/mail/owner/credential/cooldown contents or emit entry names.
	for _, directory := range []string{"state", "owner-dm"} {
		residue, err := s.legacyResidue(directory, name)
		if err != nil || residue {
			return inconclusivePresence("unqualified_state")
		}
	}
	return presence("absent", "none", "local_absence")
}

func (s *presenceSnapshot) legacyResidue(directory, name string) (bool, error) {
	info, err := s.stat(directory)
	if err != nil || info == nil {
		return false, err
	}
	if !privatePresenceInfo(info, true) {
		return false, errors.New("unsafe state directory")
	}
	file, err := s.root.OpenFile(directory, presenceOpenFlags, 0)
	if err != nil {
		return false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !samePresenceInfo(info, opened) {
		return false, errors.New("state directory changed")
	}
	entries, err := file.ReadDir(1025)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) > 1024 {
		return false, errors.New("unreadable or excessive state directory")
	}
	for _, entry := range entries {
		if entry.Name() == name || strings.HasPrefix(entry.Name(), name+".") {
			return true, nil
		}
	}
	return false, nil
}

func (s *presenceSnapshot) routing(connection Connection, getenv Getenv) bool {
	raw, _, err := s.read("matrix.env", "routing", 64<<10)
	if err != nil {
		return false
	}
	values := map[string]string{}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		key, value, ok := bytes.Cut(bytes.TrimSpace(line), []byte{'='})
		key = bytes.TrimSpace(bytes.TrimPrefix(key, []byte("export ")))
		// Do not decode registration-token/unknown values in this mixed file.
		if !ok || string(key) != EnvMatrixURL && string(key) != EnvServerName {
			continue
		}
		if _, duplicate := values[string(key)]; duplicate {
			return false
		}
		values[string(key)] = strings.Trim(strings.TrimSpace(string(value)), `"'`)
	}
	get := func(key string) string {
		if value := getenv(key); value != "" {
			return value
		}
		return values[key]
	}
	url, server, store := get(EnvMatrixURL), get(EnvServerName), getenv("A2A_SECRET_STORE")
	return (url == "" || strings.TrimRight(url, "/") == strings.TrimRight(connection.URL, "/")) &&
		(server == "" || server == connection.ServerName) && (store == "" || store == connection.Store.Backend)
}
