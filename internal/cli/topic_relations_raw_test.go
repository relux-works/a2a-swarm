package cli

import (
	"encoding/json"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"strings"
	"testing"
)

// Raw policy refusals must be atomic; adjacent valid cases retain ordinary
// peer body compatibility and native reply-to-reply targets.
func TestTopicRelationsRawPolicy(t *testing.T) {
	cases := []struct {
		name, content, unsigned string
		reject                  bool
	}{
		{"ordinary-reply-to-reply", `{"msgtype":"m.text","body":"ok","m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","m.in_reply_to":{"event_id":"$request"}}}`, ``, false},
		{"legacy-duplicate-body", `{"msgtype":"m.text","body":"first","body":"last","m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root"}}`, ``, false},
		{"unrelated-root-control", `{"msgtype":"m.text","body":"bad","m.relates_to":{"rel_type":"m.thread","event_id":"$foreign"}}`, ``, true},
		{"missing-root-control", `{"msgtype":"m.text","body":"bad","m.relates_to":{"rel_type":"m.thread"}}`, ``, true},
		{"duplicate-root", `{"msgtype":"m.text","body":"bad","m.relates_to":{"rel_type":"m.thread","event_id":"$foreign","event_id":"$archive-root"}}`, ``, true},
		{"duplicate-relation", `{"msgtype":"m.text","body":"bad","m.relates_to":{"rel_type":"m.thread","event_id":"$foreign"},"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root"}}`, ``, true},
		{"malformed-native-parent", `{"msgtype":"m.text","body":"bad","m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","m.in_reply_to":{"event_id":123}}}`, ``, true},
		{"redacted-missing-relation-control", `{}`, `{"redacted_because":{}}`, false},
		{"redacted-foreign-root", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$foreign"}}`, `{"redacted_because":{}}`, true},
		{"redacted-selected-root", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root"}}`, `{"redacted_because":{}}`, false},
		{"ordinary-body-type", `{"body":123,"future":{"x":1,"x":2},"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root"}}`, ``, false},
		{"ordinary-body-unicode", `{"body":"legacy\ud800","m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root"}}`, ``, false},
		{"relation-case-alias", `{"M.RELATES_TO":{"rel_type":"m.thread","event_id":"$archive-root"}}`, ``, true},
		{"root-case-alias", `{"m.relates_to":{"rel_type":"m.thread","EVENT_ID":"$archive-root"}}`, ``, true},
		{"root-null", `{"m.relates_to":{"rel_type":"m.thread","event_id":null}}`, ``, true},
		{"root-unicode", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root\ud800"}}`, ``, true},
		{"relation-null", `{"m.relates_to":null}`, `{"redacted_because":{}}`, true},
		{"relation-array", `{"m.relates_to":[]}`, ``, true},
		{"relation-type-duplicate", `{"m.relates_to":{"rel_type":"m.replace","rel_type":"m.thread","event_id":"$archive-root"}}`, ``, true},
		{"native-parent-null", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","m.in_reply_to":null}}`, ``, true},
		{"native-parent-missing", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","m.in_reply_to":{}}}`, ``, true},
		{"native-parent-case-alias", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","M.IN_REPLY_TO":{"event_id":"$request"}}}`, ``, true},
		{"native-parent-duplicate", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","m.in_reply_to":{"event_id":"$request","event_id":"$other"}}}`, ``, true},
		{"native-parent-self", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","m.in_reply_to":{"event_id":"$probe"}}}`, ``, true},
		{"falling-back-valid", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","is_falling_back":true}}`, ``, false},
		{"falling-back-malformed", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root","is_falling_back":"true"}}`, ``, true},
		{"redaction-duplicate", `{}`, `{"redacted_because":null,"redacted_because":{}}`, true},
		{"redaction-case-alias", `{}`, `{"REDACTED_BECAUSE":{}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTopicRelationsFixture(t, true)
			ev := relationEvent("$probe", relationsRoot, "$request", 300001)
			ev.Content, ev.Unsigned = json.RawMessage(tc.content), json.RawMessage(tc.unsigned)
			f.pages[""] = relationsResponse{relationsJSON([]matrix.Event{ev}, ""), 200}
			r := f.read(relationsRoot, 10, true)
			t.Logf("exit=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
			if tc.reject {
				if r.code == ExitOK || r.stdout != "" {
					t.Fatal("policy-invalid relation reached successful event output")
				}
			} else if r.code != ExitOK || !strings.Contains(r.stdout, `"event_id":"$probe"`) {
				t.Fatal("adjacent valid control refused")
			}
		})
	}
}

func TestTopicRelationsRawIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		valid               bool
	}{
		{"duplicate-room", `"room_id":"!archive:fixture"`, `"room_id":"!foreign:fixture","room_id":"!archive:fixture"`, false},
		{"room-case-alias", `"room_id"`, `"ROOM_ID"`, false},
		{"room-null", `"room_id":"!archive:fixture"`, `"room_id":null`, false},
		{"room-empty", `"room_id":"!archive:fixture"`, `"room_id":""`, false},
		{"room-absent", `,"room_id":"!archive:fixture"`, ``, true},
		{"duplicate-event", `"event_id":"$probe"`, `"event_id":"$foreign","event_id":"$probe"`, false},
		{"event-case-alias", `"event_id":"$probe"`, `"EVENT_ID":"$probe"`, false},
		{"event-number", `"event_id":"$probe"`, `"event_id":123`, false},
		{"event-null", `"event_id":"$probe"`, `"event_id":null`, false},
		{"event-unicode", `"event_id":"$probe"`, `"event_id":"$probe\ud800"`, false},
		{"event-escaped-key", `"event_id":"$probe"`, `"event\u005fid":"$probe"`, true},
		{"duplicate-type", `"type":"m.room.message"`, `"type":"m.room.member","type":"m.room.message"`, false},
		{"type-case-alias", `"type":"m.room.message"`, `"TYPE":"m.room.message"`, false},
		{"duplicate-content", `"content":`, `"content":{},"content":`, false},
		{"content-case-alias", `"content":`, `"CONTENT":`, false},
		{"duplicate-sender", `"sender":"@reader:fixture"`, `"sender":"@other:fixture","sender":"@reader:fixture"`, false},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%v", tc.name, asJSON), func(t *testing.T) {
				f := newTopicRelationsFixture(t, true)
				ev := relationEvent("$probe", relationsRoot, "$request", 300001)
				original := relationsJSON([]matrix.Event{ev}, "")
				raw := strings.Replace(original, tc.before, tc.after, 1)
				if raw == original {
					t.Fatal("control did not change fixture")
				}
				f.pages[""] = relationsResponse{raw, 200}
				r := f.read(relationsRoot, 10, asJSON)
				t.Logf("exit=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
				if tc.valid {
					if r.code != ExitOK || !strings.Contains(r.stdout, "$probe") {
						t.Fatal("valid identity refused")
					}
				} else if r.code == ExitOK || r.stdout != "" {
					t.Fatal("invalid identity reached event output")
				}
			})
		}
	}
}
