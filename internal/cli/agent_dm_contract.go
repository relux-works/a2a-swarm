package cli

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const agentDMVersion = "a2a-agent-dm/v1"
const agentDMMarker = "ai.relux.a2a.agent_dm"
const agentDMMaxBody = 16000

type agentDMRoute struct {
	Contract  string  `json:"contract"`
	Sender    string  `json:"sender_mxid"`
	Recipient string  `json:"recipient_mxid"`
	PairKey   string  `json:"pair_key"`
	Room      *string `json:"room_id"`
	State     string  `json:"state"`
	Creator   string  `json:"creator_mxid"`
	Alias     string  `json:"alias"`
}

func agentFullID(id string) bool {
	if !validPrincipalID(id) {
		return false
	}
	local, _, ok := strings.Cut(id[1:], ":")
	return ok && aliasLocalRE.MatchString(local)
}

func agentPair(me, peer string) (agentDMRoute, error) {
	r := agentDMRoute{Contract: agentDMVersion, Sender: me, Recipient: peer, State: "absent-own-record"}
	if !agentFullID(me) || !agentFullID(peer) || me == peer {
		return r, errors.New("agent-dm: exact distinct full supported MXIDs required")
	}
	a, b := me, peer
	if a > b {
		a, b = b, a
	}
	buf := append([]byte(agentDMVersion), 0)
	for _, id := range []string{a, b} {
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(id)))
		buf = append(buf, []byte(id)...)
	}
	h := sha256.Sum256(buf)
	r.PairKey = hex.EncodeToString(h[:])
	r.Creator = a
	_, server, _ := strings.Cut(a[1:], ":")
	r.Alias = "#a2a-dm-v1-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h[:])) + ":" + server
	return r, nil
}

// All host shapes use exact names and consume the strict tree they validate.
func agentObject(v any, keys ...string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != len(keys) {
		return nil, errors.New("agent-dm: invalid object shape")
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return nil, errors.New("agent-dm: missing exact field")
		}
	}
	return m, nil
}
func agentString(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func agentReference(v string) bool {
	if len(v) == 0 || len(v) > 512 || !utf8.ValidString(v) || strings.TrimSpace(v) != v {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func agentRole(v string) bool { return v == "keeper" || v == "orchestrator" }

type agentDMAdmission struct {
	CallerRole      string `json:"caller_role"`
	CallerReference string `json:"caller_admission_ref"`
	PeerRole        string `json:"peer_role"`
	PeerReference   string `json:"peer_admission_ref"`
}

func parseAgentAdmission(raw []byte, me, connection, peer string) (agentDMAdmission, error) {
	var out agentDMAdmission
	v, err := matrix.ParseStrictJSON(raw)
	if err != nil {
		return out, errors.New("agent-dm: malformed admission")
	}
	m, err := agentObject(v, "contract", "caller_mxid", "connection_id", "caller_role", "caller_admission_ref", "peers")
	if err != nil {
		return out, err
	}
	if agentString(m, "contract") != agentDMVersion || agentString(m, "caller_mxid") != me || agentString(m, "connection_id") != connection || !agentFullID(me) || !agentRole(agentString(m, "caller_role")) || !agentReference(agentString(m, "caller_admission_ref")) {
		return out, errors.New("agent-dm: admission caller/connection/role/reference mismatch")
	}
	peers, ok := m["peers"].([]any)
	if !ok || len(peers) == 0 || len(peers) > 64 {
		return out, errors.New("agent-dm: invalid admitted peers")
	}
	seen := map[string]bool{}
	found := false
	for _, v := range peers {
		p, err := agentObject(v, "mxid", "role", "admission_ref")
		if err != nil {
			return out, err
		}
		id, role, ref := agentString(p, "mxid"), agentString(p, "role"), agentString(p, "admission_ref")
		if !agentFullID(id) || id == me || seen[id] || !agentRole(role) || !agentReference(ref) {
			return out, errors.New("agent-dm: invalid peer admission")
		}
		seen[id] = true
		if id == peer {
			found = true
			out = agentDMAdmission{agentString(m, "caller_role"), agentString(m, "caller_admission_ref"), role, ref}
		}
	}
	if !found {
		return out, errors.New("agent-dm: peer not admitted")
	}
	return out, nil
}

// agentAudience accepts exactly a complete current-state observation. preInvite
// is used only after this caller created the room, before its one invitation.
func agentAudience(events []matrix.RoomPolicyEvent, r agentDMRoute, preInvite bool) (string, error) {
	bad := func(reason string) (string, error) { return "invalid", fmt.Errorf("agent-dm: %s", reason) }
	seen := map[string]bool{}
	members := map[string]string{}
	required := map[string]bool{}
	for _, ev := range events {
		key := ev.Type + "\x00" + ev.StateKey
		if seen[key] {
			return bad("duplicate state")
		}
		seen[key] = true
		if ev.Type == "m.room.encryption" {
			return bad("encrypted room")
		}
		// Refuse case shadows of every field interpreted by this policy.
		for k := range ev.Content {
			for _, name := range []string{"creator", "room_version", "membership", "join_rule", "history_visibility", "guest_access", "contract", "pair", "creator_mxid"} {
				if k != name && strings.EqualFold(k, name) {
					return bad("ambiguous state field")
				}
			}
		}
		switch ev.Type {
		case "m.room.create":
			if ev.StateKey != "" {
				return bad("invalid creation state key")
			}
			version := agentString(ev.Content, "room_version")
			if value, present := ev.Content["room_version"]; present {
				if _, ok := value.(string); !ok || version == "" {
					return bad("invalid room version")
				}
			} else {
				version = "1"
			}
			// Supported legacy and v12 creator formats; unknown versions refuse.
			if version != "1" && version != "2" && version != "3" && version != "4" && version != "5" && version != "6" && version != "7" && version != "8" && version != "9" && version != "10" && version != "11" && version != "12" {
				return bad("unknown room version")
			}
			if ev.Sender != r.Creator {
				return bad("wrong_creator")
			}
			if version != "11" && version != "12" && agentString(ev.Content, "creator") != r.Creator {
				return bad("wrong_creator")
			}
			if creator, present := ev.Content["creator"]; present && creator != r.Creator {
				return bad("wrong_creator")
			}
			if additional, present := ev.Content["additional_creators"]; present {
				a, ok := additional.([]any)
				if !ok || len(a) != 0 {
					return bad("additional creators")
				}
			}
			required[ev.Type] = true
		case agentDMMarker:
			m, err := agentObject(ev.Content, "contract", "pair", "creator_mxid")
			if err != nil {
				return bad("wrong_marker")
			}
			a, b := r.Sender, r.Recipient
			if a > b {
				a, b = b, a
			}
			pair, ok := m["pair"].([]any)
			if ev.StateKey != "" || ev.Sender != r.Creator || agentString(m, "contract") != agentDMVersion || agentString(m, "creator_mxid") != r.Creator || !ok || len(pair) != 2 || pair[0] != a || pair[1] != b {
				return bad("wrong_marker")
			}
			required[ev.Type] = true
		case "m.room.join_rules", "m.room.history_visibility", "m.room.guest_access":
			field, want := "join_rule", "invite"
			if ev.Type == "m.room.history_visibility" {
				field, want = "history_visibility", "joined"
			}
			if ev.Type == "m.room.guest_access" {
				field, want = "guest_access", "forbidden"
			}
			if ev.StateKey != "" || agentString(ev.Content, field) != want {
				return bad("invalid private room policy")
			}
			required[ev.Type] = true
		case "m.room.member":
			if !agentFullID(ev.StateKey) {
				return bad("invalid member ID")
			}
			membership := agentString(ev.Content, "membership")
			switch membership {
			case "join", "invite", "knock":
				if ev.StateKey != r.Sender && ev.StateKey != r.Recipient {
					return bad("extra live member")
				}
				if membership == "knock" {
					return bad("knock membership")
				}
				if membership == "join" && ev.Sender != ev.StateKey {
					return bad("joined sender mismatch")
				}
				if membership == "invite" && ev.Sender != r.Creator {
					return bad("invitation is not from creator")
				}
				members[ev.StateKey] = membership
			case "leave", "ban":
			default:
				return bad("unknown membership")
			}
		}
	}
	for _, typ := range []string{"m.room.create", agentDMMarker, "m.room.join_rules", "m.room.history_visibility", "m.room.guest_access"} {
		if !required[typ] {
			return bad("incomplete current state")
		}
	}
	if members[r.Creator] != "join" {
		return bad("creator not joined")
	}
	peer := r.Sender
	if peer == r.Creator {
		peer = r.Recipient
	}
	if preInvite && len(members) == 1 && members[peer] == "" {
		return "pending_invite", nil
	}
	if len(members) != 2 {
		return bad("incomplete audience")
	}
	if members[peer] == "invite" {
		return "pending_invite", nil
	}
	if members[peer] == "join" {
		return "active", nil
	}
	return bad("invalid audience")
}
