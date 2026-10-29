package tmuxwake

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func registryFixture(version, status string) map[string]any {
	return map[string]any{"cwd": "/synthetic", "entrypoint": "cli", "kind": "interactive", "messagingSocketPath": "/synthetic/not-opened", "name": "synthetic", "nameSince": int64(1), "nameSource": "user", "peerFeatures": []string{"notify_idle", "reply_across_default_dirs", "artifact_yield"}, "peerProtocol": 1, "pid": 1234, "pidDomain": "darwin", "procStart": "Mon Oct 26 12:00:00 2026", "sessionId": "11111111-1111-4111-8111-111111111111", "startedAt": int64(1793016001000), "status": status, "statusUpdatedAt": int64(1793016002000), "tmux": "synthetic:@0.%1", "updatedAt": int64(1793016002000), "version": version}
}

// Qualified fixtures establish schema/identity/status decisions, not provider
// responsiveness. Each enabled version pairs shell-empty eligibility with a
// busy record carrying the identical background-feature fields.
func TestRegistryQualifiedSchemas(t *testing.T) {
	id := registryIdentity{PID: 1234, Birth: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
	for _, version := range []string{"2.1.302", "2.1.309", "2.1.310", "2.1.311", "2.1.312"} {
		for _, status := range []string{"idle", "shell", "busy"} {
			t.Run(version+"/"+status, func(t *testing.T) {
				m := registryFixture(version, status)
				data, _ := json.Marshal(m)
				got := decodeRegistry(data, id)
				want := RuntimeReady
				if status == "busy" {
					want = RuntimeRefused
				}
				if got.State != want || got.Version != version || got.Status != status {
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
		{"extra", func(m map[string]any) { m["future"] = true }, RuntimeSchemaUnqualified},
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
		{"unknown-version", func(m map[string]any) { m["version"] = "2.1.313" }, RuntimeVersionUnqualified},
		{"startedAt-not-birth", func(m map[string]any) { m["startedAt"] = int64(2) }, RuntimeNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := registryFixture("2.1.310", "idle")
			c.change(m)
			b, _ := json.Marshal(m)
			if got := decodeRegistry(b, id); got.Reason != c.want {
				t.Fatalf("got %#v want %s", got, c.want)
			}
		})
	}
	for _, data := range []string{`{"version":"2.1.310","version":"2.1.310"}`, `{`, strings.Repeat(" ", maxRegistryBytes+1)} {
		if got := decodeRegistry([]byte(data), id); got.Reason != RuntimeRegistryUnavailable {
			t.Fatalf("malformed/duplicate/oversized: %#v", got)
		}
	}
}

// A record captured from a live 2.1.312 interactive session (derived name,
// identity values replaced; see testdata/README.md) is runtime-ready as
// shell and refused as busy. The same bytes with the next, unqualified
// version or one unknown key are refused, so the capture is not admitted by
// a permissive decoder. Limit: one capture with nameSource=derived; the
// user-named shape is covered by the synthetic fixture above.
func TestRegistryCapturedRecord(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "claude-code-2.1.312-registry-derived.json"))
	if err != nil {
		t.Fatal(err)
	}
	id := registryIdentity{PID: 1234, Birth: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
	if got := decodeRegistry(data, id); got.State != RuntimeReady || got.Reason != RuntimeNone || got.Version != "2.1.312" || got.Status != "shell" {
		t.Fatalf("captured shell: %#v", got)
	}
	for _, c := range []struct {
		name, old, next string
		want            RuntimeReason
	}{
		{"busy", `"status": "shell"`, `"status": "busy"`, RuntimeBusy},
		{"next-version", `"version": "2.1.312"`, `"version": "2.1.313"`, RuntimeVersionUnqualified},
		{"unknown-key", `"status": "shell"`, `"status": "shell", "hostSessionId": "synthetic"`, RuntimeSchemaUnqualified},
	} {
		t.Run(c.name, func(t *testing.T) {
			if bytes.Count(data, []byte(c.old)) != 1 {
				t.Fatalf("fixture anchor %q not unique", c.old)
			}
			if got := decodeRegistry(bytes.Replace(data, []byte(c.old), []byte(c.next), 1), id); got.Reason != c.want {
				t.Fatalf("got %#v want %s", got, c.want)
			}
		})
	}
}
