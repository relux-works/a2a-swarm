package tmuxwake

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func registryFixture(version, status string) map[string]any {
	m := map[string]any{"cwd": "/synthetic", "entrypoint": "cli", "kind": "interactive", "messagingSocketPath": "/synthetic/not-opened", "name": "synthetic", "nameSince": int64(1), "nameSource": "user", "peerFeatures": []string{"notify_idle", "reply_across_default_dirs", "artifact_yield"}, "peerProtocol": 1, "pid": 1234, "pidDomain": "darwin", "procStart": "Mon Oct 26 12:00:00 2026", "sessionId": "11111111-1111-4111-8111-111111111111", "startedAt": int64(1793016001000), "status": status, "statusUpdatedAt": int64(1793016002000), "tmux": "synthetic:@0.%1", "updatedAt": int64(1793016002000), "version": version}
	switch version {
	case "2.1.312", "2.1.317", "2.1.318":
		m["bridgeSessionId"] = "synthetic"
	}
	if version == "2.1.317" || version == "2.1.318" {
		m["asksForConvertedSkills"] = false
	}
	return m
}

// Qualified fixtures establish schema/identity/status decisions, not provider
// responsiveness. Each observed version pairs shell-empty eligibility with a
// busy record carrying the identical background-feature fields.
func TestRegistryQualifiedSchemas(t *testing.T) {
	id := registryIdentity{PID: 1234, Domain: "darwin", Birth: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
	for _, version := range []string{"2.1.302", "2.1.309", "2.1.310", "2.1.312", "2.1.317", "2.1.318", "2.1.319", "3.0.0"} {
		for _, status := range []string{"idle", "shell", "busy"} {
			t.Run(version+"/"+status, func(t *testing.T) {
				m := registryFixture(version, status)
				data, _ := json.Marshal(m)
				got := decodeRegistry(data, id)
				want := RuntimeReady
				wantReason := RuntimeNone
				if status == "busy" {
					want = RuntimeRefused
					wantReason = RuntimeBusy
				}
				if got.State != want || got.Reason != wantReason || got.Version != version || got.Status != status {
					t.Fatalf("got %#v", got)
				}
			})
		}
	}
	cases := []struct {
		name   string
		change func(map[string]any)
		want   RuntimeReason
	}{
		{"renamed", func(m map[string]any) {
			m["formerNames"] = []any{map[string]any{"name": "old", "sessionId": id.SessionID, "until": 1}}
		}, RuntimeNone},
		{"bridge", func(m map[string]any) { m["bridgeSessionId"] = "synthetic" }, RuntimeNone},
		{"missing", func(m map[string]any) { delete(m, "cwd") }, RuntimeSchemaUnqualified},
		{"extra", func(m map[string]any) { m["future"] = "PRIVATE-VALUE" }, RuntimeNone},
		{"wrong-type", func(m map[string]any) { m["statusUpdatedAt"] = "private" }, RuntimeSchemaUnqualified},
		{"nested-extra", func(m map[string]any) {
			m["formerNames"] = []any{map[string]any{"name": "x", "sessionId": id.SessionID, "until": 1, "extra": true}}
		}, RuntimeSchemaUnqualified},
		{"reused-pid", func(m map[string]any) { m["procStart"] = "Sun Oct 25 12:00:00 2026" }, RuntimeIdentityMismatch},
		{"wrong-pid", func(m map[string]any) { m["pid"] = 1235 }, RuntimeIdentityMismatch},
		{"clear-session", func(m map[string]any) { m["sessionId"] = "22222222-2222-4222-8222-222222222222" }, RuntimeIdentityMismatch},
		{"wrong-pane", func(m map[string]any) { m["tmux"] = "synthetic:@0.%2" }, RuntimeIdentityMismatch},
		{"headless-kind-interactive", func(m map[string]any) { m["entrypoint"] = "sdk-cli" }, RuntimeHeadless},
		{"no-tmux", func(m map[string]any) { delete(m, "tmux") }, RuntimeHeadless},
		{"unknown-status", func(m map[string]any) { m["status"] = "future" }, RuntimeStatusUnqualified},
		{"unknown-version", func(m map[string]any) { m["version"] = "2.1.311" }, RuntimeNone},
		{"missing-version", func(m map[string]any) { delete(m, "version") }, RuntimeVersionUnqualified},
		{"malformed-version", func(m map[string]any) { m["version"] = "3.0.0-beta" }, RuntimeVersionUnqualified},
		{"version-type", func(m map[string]any) { m["version"] = 3 }, RuntimeSchemaUnqualified},
		{"startedAt-not-birth", func(m map[string]any) { m["startedAt"] = int64(2) }, RuntimeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := registryFixture("3.0.0", "idle")
			c.change(m)
			b, _ := json.Marshal(m)
			if got := decodeRegistry(b, id); got.Reason != c.want || (c.name == "extra" && got.UnknownKeys != "future") {
				t.Fatalf("got %#v want %s", got, c.want)
			}
		})
	}
	for _, data := range []string{`{"version":"3.0.0","version":"3.0.0"}`, `{`, strings.Repeat(" ", maxRegistryBytes+1)} {
		if got := decodeRegistry([]byte(data), id); got.Reason != RuntimeRegistryUnavailable {
			t.Fatalf("malformed/duplicate/oversized: %#v", got)
		}
	}
}

// The converted-skills flag accepts absence, either boolean and null (including
// on 2.1.318); other JSON types refuse. An additional hostSessionId is diagnostic only.
func TestRegistryConvertedSkillsSchema(t *testing.T) {
	id := registryIdentity{PID: 1234, Domain: "darwin", Birth: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
	for _, version := range []string{"2.1.312", "2.1.317", "2.1.318", "2.1.319", "3.0.0"} {
		for _, c := range []struct {
			name  string
			value any
			want  RuntimeReason
		}{
			{"absent", nil, RuntimeNone},
			{"false", false, RuntimeNone},
			{"true", true, RuntimeNone},
			{"null", nil, RuntimeNone},
			{"string", "false", RuntimeSchemaUnqualified},
			{"number", 0, RuntimeSchemaUnqualified},
			{"array", []any{}, RuntimeSchemaUnqualified},
			{"object", map[string]any{}, RuntimeSchemaUnqualified},
			{"hostSessionId", "desktop-session", RuntimeNone},
		} {
			t.Run(version+"/"+c.name, func(t *testing.T) {
				m := registryFixture(version, "idle")
				if c.name == "absent" {
					delete(m, "asksForConvertedSkills")
				} else if c.name == "hostSessionId" {
					m["hostSessionId"] = c.value
				} else {
					m["asksForConvertedSkills"] = c.value
				}
				data, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				got := decodeRegistry(data, id)
				if got.Reason != c.want || got.State != RuntimeStateFor(c.want) {
					t.Fatalf("got %#v want %s", got, c.want)
				}
			})
		}
	}
}

// Unlisted releases admit matching structure but still reject a missing required
// key. The same release is its own positive control.
func TestRegistryNeighborVersionsStructure(t *testing.T) {
	id := registryIdentity{PID: 1234, Domain: "darwin", Birth: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
	for _, version := range []string{"2.1.311", "2.1.313", "2.1.316", "2.1.319", "3.0.0"} {
		t.Run(version, func(t *testing.T) {
			m := registryFixture(version, "idle")
			data, _ := json.Marshal(m)
			if got := decodeRegistry(data, id); got.State != RuntimeReady || got.Version != version {
				t.Fatal("matching structure refused", got)
			}
			delete(m, "cwd")
			data, _ = json.Marshal(m)
			if got := decodeRegistry(data, id); got.Reason != RuntimeSchemaUnqualified {
				t.Fatal("missing key admitted", got)
			}
		})
	}
}

// Unknown values are ignored, while duplicate nested keys, trailing JSON,
// excessive depth and size still fail the unchanged decoder boundary.
func TestRegistryUnknownFieldBoundary(t *testing.T) {
	id := registryIdentity{PID: 1234, Domain: "darwin", Birth: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
	m := registryFixture("3.0.0", "idle")
	m["future"] = map[string]any{"secret": "DO-NOT-PROJECT"}
	data, _ := json.Marshal(m)
	if got := decodeRegistry(data, id); got.Reason != RuntimeNone || got.UnknownKeys != "future" {
		t.Fatal(got)
	}
	for _, bad := range [][]byte{append(append([]byte{}, data...), []byte(" {}")...), []byte(strings.Replace(string(data), `"future":{"secret":"DO-NOT-PROJECT"}`, `"future":{"x":1,"x":2}`, 1)), []byte(strings.Replace(string(data), `"future":{"secret":"DO-NOT-PROJECT"}`, `"future":`+strings.Repeat("[", 18)+"0"+strings.Repeat("]", 18), 1)), append(data, []byte(strings.Repeat(" ", maxRegistryBytes))...)} {
		if got := decodeRegistry(bad, id); got.Reason != RuntimeRegistryUnavailable {
			t.Fatal("decoder boundary weakened", got)
		}
	}
}

// Unknown releases retain every required type and required-key gate, plus
// known optional types and the recursive formerNames contract. An unchanged
// record and a valid formerNames record are positive controls.
func TestRegistryStructuralTypes(t *testing.T) {
	id := registryIdentity{PID: 1234, Domain: "darwin", Birth: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
	check := func(t *testing.T, m map[string]any, want RuntimeReason) {
		t.Helper()
		data, _ := json.Marshal(m)
		if got := decodeRegistry(data, id); got.Reason != want {
			t.Fatal("structural verdict", got, want)
		}
	}
	check(t, registryFixture("3.0.0", "idle"), RuntimeNone)
	for _, k := range []string{"cwd", "entrypoint", "kind", "messagingSocketPath", "name", "nameSince", "nameSource", "peerFeatures", "peerProtocol", "pid", "pidDomain", "procStart", "sessionId", "startedAt", "status", "statusUpdatedAt", "updatedAt", "version"} {
		t.Run(k, func(t *testing.T) {
			m := registryFixture("3.0.0", "idle")
			m[k] = nil
			check(t, m, RuntimeSchemaUnqualified)
			delete(m, k)
			want := RuntimeSchemaUnqualified
			if k == "version" {
				want = RuntimeVersionUnqualified
			}
			check(t, m, want)
		})
	}
	for _, k := range []string{"tmux", "bridgeSessionId", "asksForConvertedSkills", "formerNames"} {
		t.Run(k, func(t *testing.T) {
			m := registryFixture("3.0.0", "idle")
			m[k] = 42
			check(t, m, RuntimeSchemaUnqualified)
		})
	}
	for _, value := range []any{[]any{42}, []any{map[string]any{"name": 42, "sessionId": id.SessionID, "until": 1}}, []any{map[string]any{"name": "old", "sessionId": 42, "until": 1}}, []any{map[string]any{"name": "old", "sessionId": id.SessionID, "until": "1"}}, []any{map[string]any{"name": "old", "sessionId": id.SessionID, "until": 1}}} {
		m := registryFixture("3.0.0", "idle")
		m["formerNames"] = value
		want := RuntimeSchemaUnqualified
		if o, ok := value.([]any)[0].(map[string]any); ok && o["name"] == "old" && o["sessionId"] == id.SessionID && o["until"] == 1 {
			want = RuntimeNone
		}
		check(t, m, want)
	}
}
