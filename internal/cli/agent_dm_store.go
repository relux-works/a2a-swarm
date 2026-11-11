package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type agentDMRecord struct {
	Contract      string        `json:"contract"`
	Caller        string        `json:"caller_mxid"`
	Connection    string        `json:"connection_id"`
	Peer          string        `json:"peer_mxid"`
	PairKey       string        `json:"pair_key"`
	Room          *string       `json:"room_id"`
	Enabled       bool          `json:"enabled"`
	Qualified     bool          `json:"qualified"`
	CreateAttempt string        `json:"create_attempt"`
	InviteAttempt string        `json:"invite_attempt"`
	LeaveAttempt  string        `json:"leave_attempt"`
	SendAttempts  []agentDMSend `json:"send_attempts"`
}
type agentDMSend struct {
	Transaction string `json:"transaction_id"`
	State       string `json:"state"`
	Event       string `json:"event_id"`
}
type agentDMRecords struct {
	Contract   string                   `json:"contract"`
	Caller     string                   `json:"caller_mxid"`
	Connection string                   `json:"connection_id"`
	Pairs      map[string]agentDMRecord `json:"pairs"`
}

func agentDMDir(p config.Paths, name string) string {
	if p.Root != "" {
		return filepath.Join(p.Base, "agent-dm")
	}
	return filepath.Join(p.Base, "state", name, "agent-dm")
}
func (s *session) agentConnection() (string, error) {
	c, err := s.app.selectedConnection()
	if err != nil {
		return "", err
	}
	if c != nil {
		return c.ID, nil
	}
	h := sha256.Sum256([]byte(s.client.BaseURL + "\x00" + s.server))
	return "legacy-" + hex.EncodeToString(h[:]), nil
}

// Host files are OS-user protected. They are deliberately not a security
// boundary against another process running as that same user.
func agentDirectory(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 || !agentOwned(st) {
		return errors.New("agent-dm: expected owned private 0700 directory")
	}
	// A symlink anywhere below the configured identity root is refused by the
	// resolver and these checks; do not traverse a staged namespace redirect.
	for cur := filepath.Dir(dir); ; cur = filepath.Dir(cur) {
		st, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return errors.New("agent-dm: symlink/non-directory ancestor")
		}
		if cur == filepath.Dir(cur) {
			break
		}
	}
	return nil
}
func agentRead(dir, name string) ([]byte, error) {
	if err := agentDirectory(dir); err != nil {
		return nil, err
	}
	f, err := agentOpenPrivate(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || !agentOwned(st) || st.Size() > matrix.VerificationEntityLimit {
		return nil, errors.New("agent-dm: expected owned regular private 0600 file")
	}
	b, err := io.ReadAll(io.LimitReader(f, matrix.VerificationEntityLimit+1))
	if err != nil || int64(len(b)) != st.Size() {
		return nil, errors.New("agent-dm: incomplete private file")
	}
	current, err := os.Lstat(filepath.Join(dir, name))
	if err != nil || !os.SameFile(st, current) {
		return nil, errors.New("agent-dm: private file changed during read")
	}
	return b, nil
}
func agentWrite(ctx context.Context, dir, name string, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := agentDirectory(dir); err != nil {
		return err
	}
	if _, err := agentRead(dir, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > matrix.VerificationEntityLimit {
		return errors.New("agent-dm: retained evidence bound reached")
	}
	if err = writeSettings(filepath.Join(dir, name), append(b, '\n'), 0600); err != nil {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func agentLock(ctx context.Context, dir string) (func(), error) {
	if err := agentDirectory(dir); err != nil {
		return nil, err
	}
	if _, err := agentRead(dir, "operations.lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return (ownerDMFiles{base: filepath.Join(dir, "operations")}).lock(ctx)
}

// Exact reflected shapes also reject encoding/json's case-insensitive aliases.
func agentDecode(raw []byte, out any) error {
	v, err := matrix.ParseStrictJSON(raw)
	if err != nil {
		return err
	}
	if err = agentJSONShape(v, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func agentJSONShape(v any, t reflect.Type) error {
	if t == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if t.Kind() == reflect.Pointer {
		if v == nil {
			return nil
		}
		return agentJSONShape(v, t.Elem())
	}
	bad := errors.New("agent-dm: invalid exact JSON shape")
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok || len(m) != t.NumField() {
			return bad
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			x, ok := m[name]
			if !ok {
				return bad
			}
			if err := agentJSONShape(x, f.Type); err != nil {
				return err
			}
		}
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return bad
		}
		for _, x := range m {
			if err := agentJSONShape(x, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if v == nil {
			return bad
		}
		a, ok := v.([]any)
		if !ok {
			return bad
		}
		for _, x := range a {
			if err := agentJSONShape(x, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.String:
		if _, ok := v.(string); !ok {
			return bad
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			return bad
		}
	case reflect.Int, reflect.Int64:
		if _, ok := v.(json.Number); !ok {
			return bad
		}
	default:
		return bad
	}
	return nil
}

func (s *session) agentRecords() (agentDMRecords, []byte, error) {
	return s.agentRecordsWithReader(agentRead)
}

func (s *session) agentRecordsWithReader(read func(string, string) ([]byte, error)) (agentDMRecords, []byte, error) {
	out := agentDMRecords{Contract: agentDMVersion, Caller: s.creds.UserID, Pairs: map[string]agentDMRecord{}}
	raw, err := read(agentDMDir(s.paths, s.name), "records.json")
	// Pure ordinary-delivery fixtures need no connection resolver when this
	// feature has never been configured. Actual pair records always require it.
	if s.app == nil || s.app.Getenv == nil || s.client == nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil, nil
		}
		return out, raw, errors.New("agent-dm: selected connection unavailable")
	}
	connection, connectionErr := s.agentConnection()
	if connectionErr != nil {
		return out, raw, connectionErr
	}
	out.Connection = connection
	if errors.Is(err, os.ErrNotExist) {
		return out, nil, nil
	}
	if err != nil {
		return out, nil, err
	}
	if err = agentDecode(raw, &out); err != nil {
		return out, raw, errors.New("agent-dm: malformed pair records")
	}
	if out.Contract != agentDMVersion || out.Caller != s.creds.UserID || out.Connection != connection || len(out.Pairs) > 64 {
		return out, raw, errors.New("agent-dm: pair records binding mismatch")
	}
	rooms := map[string]bool{}
	for peer, d := range out.Pairs {
		r, err := agentPair(s.creds.UserID, peer)
		if err != nil || d.Contract != r.Contract || d.Caller != r.Sender || d.Peer != peer || d.Connection != connection || d.PairKey != r.PairKey || len(d.SendAttempts) > 256 {
			return out, raw, errors.New("agent-dm: invalid pair record")
		}
		if d.Room != nil {
			if !matrix.ValidVerificationID(*d.Room, '!') || rooms[*d.Room] {
				return out, raw, errors.New("agent-dm: conflicting room mapping")
			}
			rooms[*d.Room] = true
		}
		if d.Qualified && (d.Room == nil || !d.Enabled) {
			return out, raw, errors.New("agent-dm: invalid active mapping")
		}
		for _, attempt := range []string{d.CreateAttempt, d.InviteAttempt, d.LeaveAttempt} {
			if attempt != "" && attempt != "started" && attempt != "confirmed" {
				return out, raw, errors.New("agent-dm: invalid recovery fence")
			}
		}
		for _, send := range d.SendAttempts {
			if !strings.HasPrefix(send.Transaction, "a2a-adm-") || (send.State != "started" && send.State != "confirmed") {
				return out, raw, errors.New("agent-dm: invalid send evidence")
			}
		}
	}
	return out, raw, nil
}
func (s *session) saveAgentRecords(ctx context.Context, st agentDMRecords) error {
	rooms := []string{}
	for _, pair := range st.Pairs {
		if pair.Room != nil {
			rooms = append(rooms, *pair.Room)
		}
	}
	if err := s.retainAgentContexts(ctx, rooms); err != nil {
		return err
	}
	return agentWrite(ctx, agentDMDir(s.paths, s.name), "records.json", st)
}

func (s *session) agentAdmitted(peer string) (agentDMAdmission, error) {
	var out agentDMAdmission
	if s.app.human {
		return out, errors.New("agent-dm: human credentials refused")
	}
	// A locally known human cannot be relabelled by this admission input.
	if localpart(peer) != "" && config.ValidateName(localpart(peer)) == nil {
		if meta, err := config.LoadCredentialMetadata(s.paths, localpart(peer), true); err == nil && meta.UserID == peer {
			return out, errors.New("agent-dm: known human target refused")
		}
	}
	if _, err := agentPair(s.creds.UserID, peer); err != nil {
		return out, err
	}
	if _, err := os.Lstat(s.paths.HumanFile(s.name)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return out, errors.New("agent-dm: human identity refused")
	}
	// Host-pinned authority stays outside this peer feature, including malformed
	// existing pins: an unreadable authority input cannot become a peer grant.
	of := ownerFiles(s.paths, s.name)
	principals, err := of.principals()
	if err != nil {
		return out, err
	}
	if of.daemonOwner != "" {
		principals = append(principals, of.daemonOwner)
	}
	if raw, _, err := ownerRead(of.path("pin")); err != nil {
		return out, err
	} else if len(raw) > 0 {
		var pin ownerDMPin
		if err := agentDecode(raw, &pin); err != nil || !validPrincipalID(pin.Owner) || (pin.Mode != "host" && pin.Mode != "owner-channel") {
			return out, errors.New("agent-dm: invalid owner pin")
		}
		principals = append(principals, pin.Owner)
	}
	if raw, _, err := ownerRead(principalFiles(s.paths, s.name).path("pin")); err != nil {
		return out, err
	} else if len(raw) > 0 {
		var pin principalDMPin
		if err := agentDecode(raw, &pin); err != nil {
			return out, errors.New("agent-dm: invalid principal pin")
		}
		principals = append(principals, pin.Principals...)
	}
	for _, id := range principals {
		if id == peer || id == s.creds.UserID {
			return out, errors.New("agent-dm: owner/principal target or caller refused")
		}
	}
	connection, err := s.agentConnection()
	if err != nil {
		return out, err
	}
	raw, err := agentRead(agentDMDir(s.paths, s.name), "admission.json")
	if err != nil {
		return out, fmt.Errorf("agent-dm: host admission unavailable: %w", err)
	}
	return parseAgentAdmission(raw, s.creds.UserID, connection, peer)
}
