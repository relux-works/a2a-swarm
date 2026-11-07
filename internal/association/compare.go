package association

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

const Schema = "a2a.terminal-association.proposed/1"
const maxTime = int64(4102444800000)

type target struct {
	Socket, SocketID, Pane, PaneBirth, OwnerBirth, Runtime string
	PanePID, OwnerPID                                      int
}
type member struct {
	Generation, ConsumerBirth, OwnerBirth, Runtime, SessionID string
	ConsumerPID, OwnerPID                                     int
	Target                                                    target
}
type request struct {
	Identity, Connection, Digest string
	Issued, Expires              int64
	Expected                     member
}

func decodeMember(m map[string]any, stored bool) (member, bool) {
	if m == nil {
		return member{}, false
	}
	cPID, cBirth := "consumer_pid", "consumer_birth"
	if stored {
		cPID, cBirth = "daemon_pid", "daemon_birth"
	} else if !closed(m, "generation", cPID, cBirth, "owner_pid", "owner_birth", "runtime", "session_id", "target") {
		return member{}, false
	}
	t, ok := m["target"].(map[string]any)
	if !ok {
		return member{}, false
	}
	if !stored && !closed(t, "socket", "socket_id", "pane", "pane_pid", "pane_birth", "owner_pid", "owner_birth", "runtime") {
		return member{}, false
	}
	v := member{Generation: text(m["generation"], 32), ConsumerPID: pid(m[cPID]), ConsumerBirth: text(m[cBirth], 128), OwnerPID: pid(m["owner_pid"]), OwnerBirth: text(m["owner_birth"], 128), SessionID: text(m["session_id"], 200)}
	v.Target = target{Socket: text(t["socket"], 512), SocketID: text(t["socket_id"], 128), Pane: text(t["pane"], 11), PanePID: pid(t["pane_pid"]), PaneBirth: text(t["pane_birth"], 128), OwnerPID: pid(t["owner_pid"]), OwnerBirth: text(t["owner_birth"], 128), Runtime: text(t["runtime"], 6)}
	v.Runtime = v.Target.Runtime
	valid := generation.MatchString(v.Generation) && v.ConsumerPID > 0 && v.ConsumerBirth != "" && v.OwnerPID > 0 && v.OwnerBirth != "" && v.SessionID != "" && v.Target.Socket != "" && v.Target.SocketID != "" && pane.MatchString(v.Target.Pane) && v.Target.PanePID > 0 && v.Target.PaneBirth != "" && v.Target.OwnerPID == v.OwnerPID && v.Target.OwnerBirth == v.OwnerBirth && (v.Runtime == "claude" || v.Runtime == "codex")
	if !stored {
		valid = valid && m["runtime"] == v.Runtime
	}
	return v, valid
}

func decodeRequest(b []byte) (request, error) {
	m, e := parse(b, 5, 512)
	if e != nil {
		return request{}, invalid
	}
	x, _ := m["expected"].(map[string]any)
	v, ok := decodeMember(x, false)
	r := request{Identity: text(m["source_identity"], 200), Connection: text(m["source_connection"], 128), Digest: text(m["launch_intent_sha256"], 64), Expected: v}
	if !closed(m, "schema", "source_identity", "source_connection", "launch_intent_sha256", "issued_at_ms", "expires_at_ms", "expected") || m["schema"] != Schema {
		return r, invalid
	}
	r.Issued, e = number(m["issued_at_ms"])
	if e != nil {
		return r, e
	}
	r.Expires, e = number(m["expires_at_ms"])
	if e != nil || !ok || r.Identity == "" || r.Connection == "" || !digest.MatchString(r.Digest) || r.Expires <= r.Issued || r.Expires-r.Issued > 30000 {
		return r, invalid
	}
	return r, nil
}
func number(v any) (int64, error) {
	n, ok := integer(v, maxTime)
	if !ok {
		return 0, invalid
	}
	return n, nil
}

type projection struct {
	Generation    string  `json:"generation"`
	ConsumerPID   int     `json:"consumer_pid"`
	ConsumerBirth string  `json:"consumer_birth_sha256"`
	OwnerPID      int     `json:"owner_pid"`
	OwnerBirth    string  `json:"owner_birth_sha256"`
	Runtime       string  `json:"runtime"`
	Stopped       bool    `json:"stopped"`
	StoppedReason *string `json:"stopped_reason"`
}
type output struct {
	Schema   string      `json:"schema"`
	Scope    string      `json:"scope"`
	Source   string      `json:"source"`
	Observed *int64      `json:"observed_at_ms"`
	Digest   *string     `json:"launch_intent_sha256"`
	Result   string      `json:"result"`
	Reason   string      `json:"reason"`
	Stored   *projection `json:"stored"`
	Native   string      `json:"native_evidence"`
}

func refusal(reason string, now *int64) output {
	return output{Schema: Schema, Scope: "stored-own-binding", Source: "selected-own-state-snapshot", Observed: now, Result: "unavailable", Reason: reason, Native: "not-observed-by-this-interface"}
}
func hash(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func project(m member, stopped bool) *projection {
	p := &projection{Generation: m.Generation, ConsumerPID: m.ConsumerPID, ConsumerBirth: hash(m.ConsumerBirth), OwnerPID: m.OwnerPID, OwnerBirth: hash(m.OwnerBirth), Runtime: m.Runtime, Stopped: stopped}
	if stopped {
		s := "recorded-stop-cause-ambiguous"
		p.StoppedReason = &s
	}
	return p
}
func private(m member) []string {
	return []string{m.SessionID, m.ConsumerBirth, m.OwnerBirth, m.Target.Socket, m.Target.SocketID, m.Target.Pane, m.Target.PaneBirth}
}

// Only variable scalars are checked. Public consumer/owner PIDs retain their
// specified roles; unrelated pane PID declarations are not public PID fields.
func collides(o output, members ...member) bool {
	var all, nonPID []string
	if o.Observed != nil {
		all = append(all, strconv.FormatInt(*o.Observed, 10))
	}
	if o.Digest != nil {
		all = append(all, *o.Digest)
	}
	if p := o.Stored; p != nil {
		all = append(all, p.Generation, p.ConsumerBirth, p.OwnerBirth, p.Runtime)
		nonPID = append(nonPID, all...)
		all = append(all, strconv.Itoa(p.ConsumerPID), strconv.Itoa(p.OwnerPID))
	} else {
		nonPID = append(nonPID, all...)
	}
	for _, m := range members {
		for _, secret := range private(m) {
			if secret != "" {
				for _, s := range all {
					if strings.Contains(s, secret) {
						return true
					}
				}
			}
		}
		// Integer leaves are canonical decimal text, never float64/exponents.
		for _, s := range nonPID {
			if m.Target.PanePID > 0 && strings.Contains(s, strconv.Itoa(m.Target.PanePID)) {
				return true
			}
		}
		if o.Stored != nil && m.Target.PanePID != m.ConsumerPID && m.Target.PanePID != m.OwnerPID && (o.Stored.ConsumerPID == m.Target.PanePID || o.Stored.OwnerPID == m.Target.PanePID) {
			return true
		}
	}
	return false
}
func encode(o output) ([]byte, int) {
	b, e := json.Marshal(o)
	if e != nil || len(b)+1 > 2048 {
		return nil, 3
	}
	code := 2
	if o.Result == "match" {
		code = 0
	}
	if o.Result == "mismatch" {
		code = 1
	}
	return append(b, '\n'), code
}
