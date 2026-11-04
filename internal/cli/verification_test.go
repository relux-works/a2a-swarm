package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const verificationCLIContent = `{ "msgtype":"m.text", "body":"raw\r\ntrailing\n\u001b" }`

// Proves named selection uses its actual stored full receiver and one original
// event GET, preserving raw body bytes instead of ordinary display transforms.
func TestReadVerificationObservation(t *testing.T) {
	for _, label := range []string{"alpha", "beta"} {
		for _, absent := range []bool{false, true} {
			t.Run(label+map[bool]string{true: "/absent", false: "/present"}[absent], func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				before := memberFiles(t, f.root)
				calls := 0
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != "GET" || r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles[label].URL || r.URL.Path != "/_matrix/client/v3/rooms/!selected:fixture/event/$selected" || r.Header.Get("Authorization") != "Bearer isolated-"+label {
						t.Fatalf("wrong selected endpoint %s %s", r.Method, r.URL)
					}
					room := `,"room_id":"!selected:fixture"`
					if absent {
						room = ""
					}
					body := `{"event_id":"$selected","sender":"@Historical:elsewhere.invalid","type":"m.room.message"` + room + `,"content":` + verificationCLIContent + `,"unsigned":{"private":"not-exported"}}`
					return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				args := []string{"--profile", label, "--as", "alice", "read", "!selected:fixture", "--event", "$selected", "--verification-input", "--json"}
				if code := a.Run(context.Background(), args); code != 0 {
					t.Fatalf("exit=%d %s", code, f.err.String())
				}
				var out map[string]json.RawMessage
				if err := json.Unmarshal(f.out.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if len(out) != 13-map[bool]int{true: 1, false: 0}[absent] {
					t.Fatalf("schema fields %d %s", len(out), f.out.String())
				}
				want := map[string]string{"type": "a2a.event-verification-input/1", "observation_status": "observed", "requested_room_id": "!selected:fixture", "requested_event_id": "$selected", "returned_event_id": "$selected", "sender_mxid": "@Historical:elsewhere.invalid", "receiver_mxid": "@alice:" + label + ".invalid", "receiver_identity_source": "selected-stored-session", "event_type": "m.room.message"}
				for key, value := range want {
					var got string
					json.Unmarshal(out[key], &got)
					if got != value {
						t.Fatalf("%s=%q want %q", key, got, value)
					}
				}
				var encoded string
				json.Unmarshal(out["content_base64"], &encoded)
				raw, err := base64.RawURLEncoding.DecodeString(encoded)
				if err != nil || string(raw) != verificationCLIContent {
					t.Fatalf("content transformed: %s %v", raw, err)
				}
				var present bool
				json.Unmarshal(out["returned_room_id_present"], &present)
				_, roomExists := out["returned_room_id"]
				if present == absent || roomExists == absent {
					t.Fatal("room absence synthesized")
				}
				var source string
				json.Unmarshal(out["room_context_source"], &source)
				wantSource := "returned-room-and-original-endpoint"
				if absent {
					wantSource = "authenticated-room-scoped-endpoint"
				}
				if source != wantSource {
					t.Fatal("wrong room source")
				}
				if calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
					t.Fatal("read made extra request or write")
				}
			})
		}
	}
}

// Proves prohibited selections and an identity override fail before HTTP and
// emit no success bytes; nearby valid explicit selections are exercised above.
func TestReadVerificationSelectionRefusals(t *testing.T) {
	for _, tail := range [][]string{
		{"#alias:fixture", "--event", "$selected", "--json"},
		{"!selected:fixture", "--json"},
		{"!selected:fixture", "--event", "$selected"},
		{"!selected:fixture", "--event", "$selected", "--last", "1", "--json"},
		{"!selected:fixture", "--event", "$selected", "--topic=", "--json"},
		{"!selected:fixture", "--event", "$selected", "--event", "$selected", "--json"},
		{"!selected:fixture", "--event", "$selected", "--json", "--json"},
		{"!selected:fixture", "--event", "$selected", "--json", "--verification-input"},
		{"!selected:fixture", "--event", "$selected", "--json", "--as", "bob"},
		{"!selected:fixture", "--event", "$selected", "--json", "--as", "alice", "--as", "alice"},
		{"!selected:fixture", "--event", "$selected", "--json", "--profile", "alpha", "--profile", "alpha"},
	} {
		t.Run(strings.Join(tail, " "), func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })}
			args := append([]string{"read", "--verification-input"}, tail...)
			code := a.Run(context.Background(), args)
			if code == 0 || calls != 0 || f.out.Len() != 0 {
				t.Fatalf("invalid selection passed: exit%d calls%d out%q err%q", code, calls, f.out.String(), f.err.String())
			}
		})
	}
}

// Proves wrong returned IDs, empty/redacted content, and changed selected
// binding leave stdout empty, including after an otherwise valid HTTP read.
func TestReadVerificationResponseRefusals(t *testing.T) {
	for _, kind := range []string{"wrong-id", "empty-redaction", "binding-changed", "sender-null", "content-trailing"} {
		t.Run(kind, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body := `{"event_id":"$selected","sender":"@bob:alpha.invalid","type":"m.room.message","content":` + verificationCLIContent + `}`
				switch kind {
				case "wrong-id":
					body = strings.Replace(body, "$selected", "$wrong", 1)
				case "empty-redaction":
					body = strings.Replace(body, verificationCLIContent, `{}`, 1)
				case "sender-null":
					body = strings.Replace(body, `"@bob:alpha.invalid"`, `null`, 1)
				case "content-trailing":
					body += "{}"
				case "binding-changed":
					c := f.r.Profiles["alpha"]
					c.ID = "changed"
					f.r.Profiles["alpha"] = c
					f.save()
				}
				return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			code := a.Run(context.Background(), []string{"read", "!selected:fixture", "--event", "$selected", "--verification-input", "--json"})
			if code == 0 || calls != 1 || f.out.Len() != 0 {
				t.Fatalf("invalid response escaped: exit%d calls%d out%q", code, calls, f.out.String())
			}
		})
	}
}

// Proves real post emits the closed five-key wrapper, exact human prefix and
// explicit/no-re thread forms. The inert envelope is extraction data only:
// this Source test does not verify signatures or authorize an effect.
func TestReadVerificationActualPostFixture(t *testing.T) {
	const envelope = `{"payload":"e30","request":"e30","signature":"AA","type":"grants-a2a-envelope-1"}`
	for _, reply := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-re", true: "explicit-re"}[reply], func(t *testing.T) {
			e, room := world(t)
			root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
			args := []string{"post", "proj", "--topic", "alpha", "--kind", "request", "--to", "alice"}
			if reply {
				args = append(args, "--re", root)
			}
			args = append(args, envelope)
			id := strings.TrimSpace(e.as("bob", args...).ok(t).stdout)
			out := e.as("alice", "read", room, "--event", id, "--verification-input", "--json").ok(t).stdout
			var record struct {
				Content string `json:"content_base64"`
			}
			if err := json.Unmarshal([]byte(out), &record); err != nil {
				t.Fatal(err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(record.Content)
			if err != nil {
				t.Fatal(err)
			}
			var content map[string]json.RawMessage
			json.Unmarshal(raw, &content)
			if len(content) != 5 {
				t.Fatalf("unexpected post defaults %s", raw)
			}
			var header Header
			json.Unmarshal(content[ContentKey], &header)
			var fields map[string]json.RawMessage
			json.Unmarshal(content[ContentKey], &fields)
			wantFields := 8
			if reply {
				wantFields++
			}
			if len(fields) != wantFields || header.V != 1 || header.Type != "message" || header.From != "bob" || header.Kind != "request" || header.Slug != "alpha" || header.Topic != root || !reflect.DeepEqual(header.To, []string{"alice"}) {
				t.Fatalf("unexpected actual header %s", content[ContentKey])
			}
			if _, err := time.Parse(time.RFC3339, header.At); err != nil {
				t.Fatalf("actual at format: %v", err)
			}
			var body, msgtype string
			json.Unmarshal(content["body"], &body)
			json.Unmarshal(content["msgtype"], &msgtype)
			re := "-"
			if reply {
				re = root
			}
			prefix := "- from: bob\n- to: alice\n- at: " + header.At + "\n- re: " + re + "\n- kind: request\n\n"
			if msgtype != "m.text" || body != prefix+envelope || strings.Index(body, "\n\n") != len(prefix)-2 {
				t.Fatal("actual formatter/extraction mismatch")
			}
			var relation map[string]json.RawMessage
			json.Unmarshal(content["m.relates_to"], &relation)
			_, fallbackPresent := relation["is_falling_back"]
			if fallbackPresent == reply {
				t.Fatal("actual falling-back presence mismatch")
			}
			if !reply && string(relation["is_falling_back"]) != "true" {
				t.Fatal("no-re fallback is not true")
			}
			var rootValue, relType string
			json.Unmarshal(relation["event_id"], &rootValue)
			json.Unmarshal(relation["rel_type"], &relType)
			var replyTo map[string]string
			json.Unmarshal(relation["m.in_reply_to"], &replyTo)
			if rootValue != root || relType != "m.thread" || len(replyTo) != 1 || replyTo["event_id"] != root || len(relation) != 4-map[bool]int{true: 1, false: 0}[reply] {
				t.Fatal("actual thread relation changed")
			}
			var mentions struct {
				UserIDs []string `json:"user_ids"`
			}
			json.Unmarshal(content["m.mentions"], &mentions)
			if !reflect.DeepEqual(mentions.UserIDs, []string{"@alice:test.local"}) {
				t.Fatal("actual mentions changed")
			}
			if dir := os.Getenv("A2A_VERIFICATION_FIXTURE_DIR"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				name := "no-re.json"
				if reply {
					name = "explicit-re.json"
				}
				if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
				t.Logf("actual post fixture %s bytes=%d sha256=%x", name, len(raw), sha256.Sum256(raw))
			}
		})
	}
}

type verificationBrokenWriter struct{ partial bool }

func (w verificationBrokenWriter) Write(p []byte) (int, error) {
	if w.partial {
		return len(p) / 2, nil
	}
	return 0, io.ErrClosedPipe
}

// Proves stdout failures cannot report success; a short external writer may
// already hold a prefix, which consumers must reject rather than interpret.
func TestReadVerificationOutputFailure(t *testing.T) {
	for _, partial := range []bool{false, true} {
		f := newNamedFixture(t, true)
		a := f.app()
		a.Stdout = verificationBrokenWriter{partial: partial}
		a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
			body := `{"event_id":"$selected","sender":"@bob:alpha.invalid","type":"m.room.message","content":` + verificationCLIContent + `}`
			return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if code := a.Run(context.Background(), []string{"read", "!selected:fixture", "--event", "$selected", "--verification-input", "--json"}); code == 0 {
			t.Fatal("broken writer returned success")
		}
	}
}

// Proves missing/mismatched stored receiver identity is not reconstructed or
// repaired, and no network/store-create effect precedes the refusal.
func TestReadVerificationReceiverIdentityRefusals(t *testing.T) {
	for _, kind := range []string{"missing-metadata", "mismatched-metadata", "missing-selector"} {
		t.Run(kind, func(t *testing.T) {
			f := newNamedFixture(t, kind != "missing-metadata")
			a := f.app()
			before := memberFiles(t, f.root)
			if kind == "mismatched-metadata" {
				path := f.paths("alpha", false).AgentFile("alice")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err = json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				fields["user_id"] = json.RawMessage(`"@somebody:alpha.invalid"`)
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				before = memberFiles(t, f.root)
			}
			if kind == "missing-selector" {
				old := a.Getenv
				a.Getenv = func(key string) string {
					if key == config.EnvAgent {
						return ""
					}
					return old(key)
				}
			}
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })}
			code := a.Run(context.Background(), []string{"read", "!selected:fixture", "--event", "$selected", "--verification-input", "--json"})
			if code == 0 || calls != 0 || f.out.Len() != 0 || len(f.store.creates) != 0 || !reflect.DeepEqual(before, memberFiles(t, f.root)) {
				t.Fatalf("receiver refusal failed: exit%d calls%d out%q", code, calls, f.out.String())
			}
		})
	}
}
