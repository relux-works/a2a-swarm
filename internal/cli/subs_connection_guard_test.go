package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type subscriptionBindingTransport func(*http.Request) (*http.Response, error)

func (f subscriptionBindingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type subscriptionBindingBody struct {
	io.ReadCloser
	afterClose func()
}

func (b *subscriptionBindingBody) Close() error {
	err := b.ReadCloser.Close()
	if b.afterClose != nil {
		f := b.afterClose
		b.afterClose = nil
		f()
	}
	return err
}

// Synthetic named sessions exercise the real trim commands, sync command and
// onboarding helper. A registry edit after the final HTTP response is consumed
// must refuse local effects; unchanged/current-unrelated edits remain valid.
// This checks these boundaries, not atomicity against edits after their guards.
func TestSubscriptionRepairConnectionBoundary(t *testing.T) {
	for _, operation := range []string{"record", "clear", "sync", "onboarding"} {
		for _, change := range []string{"unchanged", "current-unrelated", "service", "endpoint", "identity-domain", "connection-id"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				a.profile = "alpha"
				paths := f.paths("alpha", false)
				const room = "!policy:alpha.invalid"
				policy := config.SubscriptionPolicy{V: 1, Trims: map[string]config.SubscriptionTrim{
					room: {Dropped: []string{"system"}, Reason: "original trim", RecordedAt: "2026-10-26T00:00:00Z"},
				}}
				if err := config.SaveSubscriptionPolicy(paths, "alice", policy); err != nil {
					t.Fatal(err)
				}
				if err := config.SaveState(paths, "alice", config.State{NextBatch: "original-cursor", LegacySubscriptions: map[string][]string{room: {"notice"}}}); err != nil {
					t.Fatal(err)
				}
				read := func(path string) []byte {
					t.Helper()
					b, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					return b
				}
				beforePolicy := read(paths.SubscriptionPolicyFile("alice"))
				beforeState := read(paths.StateFile("alice"))
				remote := []byte(`{"v":1,"subs":[]}`)
				beforeRemote := bytes.Clone(remote)
				puts, triggered := 0, false
				editRegistry := func() {
					triggered = true
					c := f.r.Profiles["alpha"]
					switch change {
					case "current-unrelated":
						f.r.Current = "beta"
						other := f.r.Profiles["beta"]
						other.Store.AccessTokenService = "test.unrelated.token"
						f.r.Profiles["beta"] = other
					case "service":
						c.Store.AccessTokenService = "test.changed.token"
					case "endpoint":
						c.URL = f.r.Profiles["beta"].URL
					case "identity-domain":
						c.ServerName = "changed.invalid"
					case "connection-id":
						c.ID = "changed-id"
					}
					f.r.Profiles["alpha"] = c
					f.save()
				}
				a.HTTP = &http.Client{Transport: subscriptionBindingTransport(func(r *http.Request) (*http.Response, error) {
					body := `{}`
					trigger := false
					switch {
					case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
						body = `{"joined_rooms":["` + room + `"]}`
						trigger = operation != "sync"
					case strings.Contains(r.URL.Path, "/directory/room/"):
						body = `{"room_id":"!exempt-other:alpha.invalid","servers":[]}`
						trigger = operation == "sync" && strings.Contains(r.URL.Path, "broadcast")
					case strings.Contains(r.URL.Path, "/account_data/"):
						if r.Method == http.MethodPut {
							puts++
							var err error
							remote, err = io.ReadAll(r.Body)
							if err != nil {
								return nil, err
							}
						} else {
							body = string(remote)
						}
					case strings.HasSuffix(r.URL.Path, "/m.room.canonical_alias/"):
						body = `{"alias":"#policy:alpha.invalid"}`
					case strings.Contains(r.URL.Path, "/m.room.create/"):
					default:
						t.Errorf("unexpected fixture request: %s %s", r.Method, r.URL.Path)
					}
					rc := &subscriptionBindingBody{ReadCloser: io.NopCloser(strings.NewReader(body))}
					if trigger && !triggered {
						rc.afterClose = editRegistry
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: rc, ContentLength: int64(len(body)), Request: r}, nil
				})}
				ctx := context.Background()
				var err error
				switch operation {
				case "record", "clear":
					args := []string{"--room", room, "--drop", "decision", "--reason", "updated trim"}
					if operation == "clear" {
						args = []string{"--room", room, "--clear"}
					}
					err = cmdSubsTrim(ctx, a, args)
				case "sync":
					err = cmdSubsSync(ctx, a, nil)
				case "onboarding":
					var s *session
					s, err = a.session()
					if err == nil {
						_, err = s.client.JoinedRooms(ctx)
					}
					if err == nil {
						_, _, err = s.ensureOnboardSub(ctx, SubSelector{Room: room, Kinds: onboardKinds}, time.Now())
					}
				}
				if !triggered {
					t.Fatal("registry edit witness was not applied")
				}
				valid := change == "unchanged" || change == "current-unrelated"
				if !valid {
					if err == nil || !strings.Contains(err.Error(), "selected connection changed") {
						t.Errorf("expected selected-binding refusal, got %v", err)
					}
					if !bytes.Equal(beforePolicy, read(paths.SubscriptionPolicyFile("alice"))) || !bytes.Equal(beforeState, read(paths.StateFile("alice"))) || !bytes.Equal(beforeRemote, remote) || puts != 0 {
						t.Error("stale subscription operation changed policy/state/account data")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := config.LoadSubscriptionPolicy(paths, "alice")
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "record":
					if strings.Join(got.Trims[room].Dropped, ",") != "decision" || puts != 0 || !bytes.Equal(beforeState, read(paths.StateFile("alice"))) {
						t.Fatal("valid record did not update only its trim policy")
					}
				case "clear":
					if _, exists := got.Trims[room]; exists || puts != 0 || !bytes.Equal(beforeState, read(paths.StateFile("alice"))) {
						t.Fatal("valid clear did not remove only its trim policy")
					}
				default:
					var list SubList
					if err := json.Unmarshal(remote, &list); err != nil {
						t.Fatal(err)
					}
					kinds := map[string]bool{}
					for _, sub := range list.Subs {
						for _, kind := range sub.Kinds {
							kinds[kind] = true
						}
					}
					if puts != 1 || kinds["system"] || !kinds["decision"] || !kinds["owner-needed"] || !kinds["notice"] || !bytes.Equal(beforePolicy, read(paths.SubscriptionPolicyFile("alice"))) {
						t.Fatal("valid repair changed trim or legacy-migration semantics")
					}
				}
			})
		}
	}
}
