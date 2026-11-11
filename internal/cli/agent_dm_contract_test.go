package cli

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func TestAgentDMPairContract(t *testing.T) {
	// Fixed cross-implementation vectors pin the byte transcript and full alias;
	// reverse-direction agreement alone would also accept two matching mistakes.
	vectors := map[string][2]string{
		"@alice:one.invalid": {"4d86cb7cb1d40cf521f34c2f54341454ee9df0e9fd0eaf77161160583811ed83", "#a2a-dm-v1-jwdmw7fr2qgpkiptjqxvinauktxj34hj7uhk65ywcfqfqoar5wbq:one.invalid"},
		"@A:one.invalid":     {"50305f5d6bcdda42bd62f3b0547d1fe2d722756f1ef3e6c1fb5bc617e009c5d6", "#a2a-dm-v1-kayf6xllzxnefplc6oyfi7i74llse5lpd3z6nqp3lpdbpyajyxla:one.invalid"},
		"@a:[::1]:8448":      {"4fdc56dfae641cccd60c500108a4ce854e59370991d8d95b6296ce6bfc774717", "#a2a-dm-v1-j7ofnx5omqomzvqmkaaqrjgoqvhfsnyjshmnsw3cs3hgx7dxi4lq:[::1]:8448"},
	}
	for _, ids := range [][2]string{{"@alice:one.invalid", "@bob:two.invalid"}, {"@A:one.invalid", "@a:one.invalid"}, {"@a:[::1]:8448", "@b:two.invalid"}} {
		r, err := agentPair(ids[0], ids[1])
		if err != nil {
			t.Fatal(err)
		}
		if r.PairKey != vectors[ids[0]][0] || r.Alias != vectors[ids[0]][1] {
			t.Fatalf("fixed transcript vector mismatch: %+v", r)
		}
		a, b := ids[0], ids[1]
		if a > b {
			a, b = b, a
		}
		// Independent transcript construction witnesses the specified byte framing.
		buf := append([]byte("a2a-agent-dm/v1"), 0)
		for _, id := range []string{a, b} {
			buf = binary.BigEndian.AppendUint32(buf, uint32(len(id)))
			buf = append(buf, []byte(id)...)
		}
		h := sha256.Sum256(buf)
		if r.PairKey != hex.EncodeToString(h[:]) || r.Creator != a || len(strings.Split(r.Alias, ":")[0]) != len("#a2a-dm-v1-")+52 {
			t.Fatalf("wrong route %+v", r)
		}
		reverse, _ := agentPair(ids[1], ids[0])
		if r.PairKey != reverse.PairKey || r.Alias != reverse.Alias {
			t.Fatal("direction changed pair identity")
		}
		if r.Sender != ids[0] || r.Recipient != ids[1] {
			t.Fatal("lost direction")
		}
	}
	for _, peer := range []string{"bob", "@bob", "@bob:", "@bob:bad/server", "@b ob:one.invalid", "@bob:one.invalid\x00", "@alice:one.invalid"} {
		if _, err := agentPair("@alice:one.invalid", peer); err == nil {
			t.Fatalf("accepted %q", peer)
		}
	}
}

func admissionJSON() string {
	return `{"contract":"a2a-agent-dm/v1","caller_mxid":"@alice:one.invalid","connection_id":"fixture","caller_role":"keeper","caller_admission_ref":"host-decision-1","peers":[{"mxid":"@bob:two.invalid","role":"orchestrator","admission_ref":"host-decision-2"}]}`
}

func TestAgentDMAdmissionParser(t *testing.T) {
	for name, raw := range map[string]string{
		"valid":          admissionJSON(),
		"duplicate":      strings.Replace(admissionJSON(), `"caller_role":"keeper"`, `"caller_role":"human","caller_role":"keeper"`, 1),
		"alias":          strings.Replace(admissionJSON(), `"caller_role"`, `"Caller_Role"`, 1),
		"unknown":        strings.Replace(admissionJSON(), `"caller_role":"keeper"`, `"caller_role":"worker"`, 1),
		"null":           strings.Replace(admissionJSON(), `"peers":[`, `"peers":null,"x":[`, 1),
		"no-reference":   strings.Replace(admissionJSON(), `host-decision-2`, "", 1),
		"bad-scalar":     strings.Replace(admissionJSON(), `host-decision-2`, `\ud800`, 1),
		"trailing":       admissionJSON() + `{}`,
		"peer-duplicate": strings.Replace(admissionJSON(), `}]}`, `},{"mxid":"@bob:two.invalid","role":"keeper","admission_ref":"another"}]}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseAgentAdmission([]byte(raw), "@alice:one.invalid", "fixture", "@bob:two.invalid")
			if (err == nil) != (name == "valid") {
				t.Fatalf("parse err=%v", err)
			}
		})
	}
	for _, args := range [][3]string{{"@other:one.invalid", "fixture", "@bob:two.invalid"}, {"@alice:one.invalid", "other", "@bob:two.invalid"}, {"@alice:one.invalid", "fixture", "@stranger:two.invalid"}} {
		if _, err := parseAgentAdmission([]byte(admissionJSON()), args[0], args[1], args[2]); err == nil {
			t.Fatal("wrong binding admitted")
		}
	}
}

func agentPolicyFixture(t *testing.T, version string) []matrix.RoomPolicyEvent {
	t.Helper()
	r, _ := agentPair("@alice:one.invalid", "@bob:two.invalid")
	create := map[string]any{"room_version": version}
	if version != "11" && version != "12" {
		create["creator"] = r.Creator
	}
	return []matrix.RoomPolicyEvent{
		{Type: "m.room.create", Sender: r.Creator, Content: create},
		{Type: agentDMMarker, Sender: r.Creator, Content: map[string]any{"contract": agentDMVersion, "pair": []any{r.Creator, r.Recipient}, "creator_mxid": r.Creator}},
		{Type: "m.room.join_rules", Content: map[string]any{"join_rule": "invite"}},
		{Type: "m.room.history_visibility", Content: map[string]any{"history_visibility": "joined"}},
		{Type: "m.room.guest_access", Content: map[string]any{"guest_access": "forbidden"}},
		{Type: "m.room.member", StateKey: r.Creator, Sender: r.Creator, Content: map[string]any{"membership": "join"}},
		{Type: "m.room.member", StateKey: r.Recipient, Sender: r.Recipient, Content: map[string]any{"membership": "join"}},
	}
}

func TestAgentDMCompletePolicy(t *testing.T) {
	r, _ := agentPair("@alice:one.invalid", "@bob:two.invalid")
	for _, version := range []string{"1", "11", "12"} {
		if state, err := agentAudience(agentPolicyFixture(t, version), r, false); err != nil || state != "active" {
			t.Fatalf("version %s: %s %v", version, state, err)
		}
	}
	for _, bad := range []string{"missing", "duplicate", "encryption", "extra-join", "extra-invite", "extra-knock", "unknown-member", "wrong-creator", "wrong-marker", "public", "history", "guests", "shadow", "sender", "marker-extra"} {
		t.Run(bad, func(t *testing.T) {
			e := agentPolicyFixture(t, "12")
			switch bad {
			case "missing":
				e = e[1:]
			case "duplicate":
				e = append(e, e[0])
			case "encryption":
				e = append(e, matrix.RoomPolicyEvent{Type: "m.room.encryption", Content: map[string]any{}})
			case "extra-join", "extra-invite", "extra-knock":
				e = append(e, matrix.RoomPolicyEvent{Type: "m.room.member", StateKey: "@third:one.invalid", Content: map[string]any{"membership": strings.TrimPrefix(bad, "extra-")}})
			case "unknown-member":
				e[6].Content["membership"] = "mystery"
			case "wrong-creator":
				e[0].Sender = r.Recipient
			case "wrong-marker":
				e[1].Content["creator_mxid"] = r.Recipient
			case "public":
				e[2].Content["join_rule"] = "public"
			case "history":
				e[3].Content["history_visibility"] = "shared"
			case "guests":
				e[4].Content["guest_access"] = "can_join"
			case "shadow":
				e[2].Content["Join_Rule"] = "public"
			case "sender":
				e[6].Sender = r.Creator
			case "marker-extra":
				e[1].Content["other"] = true
			}
			if _, err := agentAudience(e, r, false); err == nil {
				raw, _ := json.Marshal(e)
				t.Fatalf("accepted invalid %s", raw)
			}
		})
	}
}
