package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A real, aliasless Space: the opaque, case-sensitive ID is deliberately
// different from the fake server's usual alias-derived IDs.
func productSelectorWorld(t *testing.T) (*env, string, string) {
	t.Helper()
	e := newEnv(t)
	alice := e.agent("alice")
	old := e.hs.addRoom("space", alice)
	space := "!Opaque/Space%2f:foreign.invalid"
	e.hs.mu.Lock()
	r := e.hs.rooms[old]
	delete(e.hs.rooms, old)
	delete(e.hs.aliases, r.alias)
	delete(r.state, "m.room.canonical_alias|")
	r.id, r.alias = space, ""
	r.state["m.room.create|"], _ = json.Marshal(map[string]string{"creator": alice, "type": "m.space"})
	e.hs.rooms[space] = r
	e.hs.mu.Unlock()
	child := e.hs.addRoom("target", alice)
	return e, space, child
}

func productSelectorArgs(command, selector string) []string {
	if command == "new" {
		return []string{"room", "new", "target", "--product", selector}
	}
	return []string{"product", "adopt", selector, "target"}
}

// Read both counterpart events through the actual Matrix client and fake HTTP
// server, rather than accepting CLI output or an ACK as evidence of storage.
func assertProductSelectorLinks(t *testing.T, e *env, space, child string) {
	t.Helper()
	s, err := eApp(e).sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	var down, up struct {
		Via       []string `json:"via"`
		Canonical bool     `json:"canonical"`
	}
	if err := s.client.GetState(context.Background(), space, "m.space.child", child, &down); err != nil {
		t.Fatal(err)
	}
	if err := s.client.GetState(context.Background(), child, "m.space.parent", space, &up); err != nil {
		t.Fatal(err)
	}
	if len(down.Via) != 1 || down.Via[0] != testServer || len(up.Via) != 1 || up.Via[0] != testServer || !up.Canonical {
		t.Fatalf("incorrect counterpart links: child=%+v parent=%+v", down, up)
	}
}

func TestProductSelectorAliaslessSpace(t *testing.T) {
	for _, command := range []string{"adopt", "new"} {
		for _, existing := range []bool{false, true} {
			if command == "adopt" && !existing {
				continue
			}
			label := command + "/existing"
			if !existing {
				label = command + "/created"
			}
			t.Run(label, func(t *testing.T) {
				e, space, child := productSelectorWorld(t)
				if !existing {
					e.hs.mu.Lock()
					delete(e.hs.rooms, child)
					delete(e.hs.aliases, "#target:"+testServer)
					e.hs.mu.Unlock()
				}
				reads, children, parents, unexpected := 0, 0, 0, 0
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
						if r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/rooms/"+space+"/state/m.room.create/" {
							reads++
						}
						if r.Method == "PUT" && strings.Contains(r.URL.Path, "/state/m.space.") {
							if reads != 1 {
								t.Errorf("link write before unique Space verification: %d", reads)
							}
							if strings.Contains(r.URL.Path, "/state/m.space.child/") {
								children++
								if r.URL.Path != "/_matrix/client/v3/rooms/"+space+"/state/m.space.child/"+child {
									t.Errorf("selected child target changed: %s", r.URL.Path)
								}
							} else {
								parents++
								if r.URL.Path != "/_matrix/client/v3/rooms/"+child+"/state/m.space.parent/"+space {
									t.Errorf("selected parent target changed: %s", r.URL.Path)
								}
							}
						}
						if r.Method == "POST" && !strings.HasSuffix(r.URL.Path, "/createRoom") || strings.Contains(r.URL.Path, "/directory/room/!") {
							unexpected++
						}
						return http.DefaultTransport.RoundTrip(r)
					})}
				}
				r := e.as("alice", productSelectorArgs(command, space)...).ok(t)
				if reads != 1 || children != 1 || parents != 1 || unexpected != 0 || !strings.Contains(r.stdout, "linked parent") {
					t.Fatalf("reads=%d child=%d parent=%d unexpected=%d result=%+v", reads, children, parents, unexpected, r)
				}
				wantCreated := 0
				if !existing {
					wantCreated = 1
				}
				if got := len(e.hs.createdRooms()); got != wantCreated {
					t.Fatalf("ID selection created extra rooms/aliases: got %d want %d", got, wantCreated)
				}
				assertProductSelectorLinks(t, e, space, child)
				// Existing idempotence semantics: no additional child/parent writes.
				e.as("alice", productSelectorArgs(command, space)...).ok(t)
				if children != 1 || parents != 1 {
					t.Fatal("repeat adoption rewrote links")
				}
				assertProductSelectorLinks(t, e, space, child)
			})
		}
	}
}

func TestProductSelectorVerificationRefusals(t *testing.T) {
	cases := []struct{ name, body string }{
		{"ordinary", `{"creator":"@alice:test.local"}`},
		{"non-space", `{"type":"m.room"}`},
		{"empty-type", `{"type":""}`},
		{"null-type", `{"type":null}`},
		{"number-type", `{"type":42}`},
		{"object-type", `{"type":{"type":"m.space"}}`},
		{"array-type", `{"type":["m.space"]}`},
		{"case-type", `{"Type":"m.space"}`},
		{"conflicting-case", `{"type":"m.space","TYPE":"m.room"}`},
		{"duplicate-type", `{"type":"m.room","type":"m.space"}`},
		{"escaped-duplicate", `{"type":"m.room","t\u0079pe":"m.space"}`},
		{"trailing", `{"type":"m.space"}{}`},
		{"array", `[{"type":"m.space"}]`},
		{"wrong-event", `{"type":"m.room.message","content":{"type":"m.space"}}`},
		{"event-envelope", `{"type":"m.room.create","state_key":"","content":{"type":"m.space"}}`},
		{"wrong-room", `{"type":"m.space","room_id":"!other:foreign.invalid"}`},
		{"null-room", `{"type":"m.space","room_id":null}`},
		{"room-case", `{"type":"m.space","ROOM_ID":"!other:foreign.invalid"}`},
		{"wrong-state-key", `{"type":"m.space","state_key":"wrong"}`},
		{"wrong-event-id", `{"type":"m.space","event_id":"$wrong"}`},
		{"refused", ""},
		{"redirect", ""},
		{"cancelled", ""},
		{"wrong-response-room", ""},
	}
	for _, command := range []string{"adopt", "new"} {
		for _, tc := range cases {
			t.Run(command+"/"+tc.name, func(t *testing.T) {
				e, space, child := productSelectorWorld(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reads, writes, creates, fallback := 0, 0, 0, 0
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
						if r.Method == "PUT" && strings.Contains(r.URL.Path, "/state/m.space.") {
							writes++
						}
						if r.Method == "POST" {
							creates++
						}
						if strings.HasSuffix(r.URL.Path, "/decoy") {
							fallback++
						}
						if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/rooms/"+space+"/state/m.room.create/" {
							return http.DefaultTransport.RoundTrip(r)
						}
						reads++
						status, body := 200, tc.body
						header := make(http.Header)
						if tc.name == "refused" {
							status, body = 403, `{"errcode":"M_FORBIDDEN"}`
						}
						if tc.name == "redirect" {
							status, body = 302, `{}`
							header.Set("Location", e.hs.srv.URL+"/decoy")
						}
						if tc.name == "cancelled" {
							cancel()
							body = `{"type":"m.space"}`
						}
						if tc.name == "wrong-response-room" {
							r = r.Clone(r.Context())
							r.URL.Path = strings.Replace(r.URL.Path, space, "!wrong:fixture", 1)
							body = `{"type":"m.space"}`
						}
						return &http.Response{StatusCode: status, Header: header, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
					})}
				}
				r := e.runCtx(ctx, append([]string{"--as", "alice"}, productSelectorArgs(command, space)...)...)
				if r.code != ExitError || reads != 1 || writes != 0 || creates != 0 || fallback != 0 || strings.Contains(r.stdout, "linked") {
					t.Fatalf("unverified target authorized work: reads=%d writes=%d creates=%d fallback=%d result=%+v", reads, writes, creates, fallback, r)
				}
				e.hs.mu.Lock()
				_, down := e.hs.rooms[space].state["m.space.child|"+child]
				_, up := e.hs.rooms[child].state["m.space.parent|"+space]
				e.hs.mu.Unlock()
				if down || up {
					t.Fatal("refusal stored a counterpart link")
				}
				// Neighboring valid selection on the same fixture exercises the
				// full command, independent of the refusal transport.
				e.configure = nil
				e.as("alice", productSelectorArgs(command, space)...).ok(t)
				assertProductSelectorLinks(t, e, space, child)
			})
		}
	}
}

// A name is resolved once; a directory change during room creation must not
// retarget the links to a second Space (or even an ordinary room).
func TestProductSelectorNamePinned(t *testing.T) {
	for _, command := range []string{"adopt", "new"} {
		t.Run(command, func(t *testing.T) {
			e, space, child := productSelectorWorld(t)
			e.hs.mu.Lock()
			e.hs.aliases["#named:"+testServer] = space
			e.hs.mu.Unlock()
			decoy := e.hs.addRoom("decoy", "@alice:"+testServer)
			resolves, reads := 0, 0
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/directory/room/#named:"+testServer) {
						resolves++
						e.hs.mu.Lock()
						e.hs.aliases["#named:"+testServer] = decoy
						e.hs.mu.Unlock()
					}
					if r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/rooms/"+space+"/state/m.room.create/" {
						reads++
					}
					return resp, err
				})}
			}
			e.as("alice", productSelectorArgs(command, "named")...).ok(t)
			if resolves != 1 || reads != 1 {
				t.Fatalf("selection repeated: resolves=%d reads=%d", resolves, reads)
			}
			assertProductSelectorLinks(t, e, space, child)
			e.hs.mu.Lock()
			_, linked := e.hs.rooms[decoy].state["m.space.child|"+child]
			e.hs.mu.Unlock()
			if linked {
				t.Fatal("alias rotation retargeted a link")
			}
		})
	}
}

func TestProductSelectorUsage(t *testing.T) {
	for _, command := range []string{"adopt", "new"} {
		for _, selector := range []string{"!", "!bad room:fixture", "!bad\nroom", "!" + strings.Repeat("x", 255), "Bad_Name", "#alias:fixture"} {
			t.Run(command+"/"+selector, func(t *testing.T) {
				e := newEnv(t)
				e.agent("alice")
				before := len(e.hs.Calls())
				e.as("alice", productSelectorArgs(command, selector)...).failsWith(t, ExitUsage, "invalid product selector")
				if len(e.hs.Calls()) != before {
					t.Fatal("invalid selector contacted the server")
				}
			})
		}
	}
}

func TestProductSelectorPartialLinkFailure(t *testing.T) {
	for _, command := range []string{"adopt", "new"} {
		for _, status := range []int{403, 500} {
			t.Run(command+"/"+http.StatusText(status), func(t *testing.T) {
				e, space, child := productSelectorWorld(t)
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
						if r.Method == "PUT" && strings.Contains(r.URL.Path, "/state/m.space.parent/") {
							return &http.Response{StatusCode: status, Request: r, Body: io.NopCloser(strings.NewReader(`{"errcode":"M_FORBIDDEN"}`))}, nil
						}
						return http.DefaultTransport.RoundTrip(r)
					})}
				}
				r := e.as("alice", productSelectorArgs(command, space)...)
				if status == 403 {
					r.ok(t)
					if !strings.Contains(r.stdout, "parent link skipped") {
						t.Fatal("partial link was not reported")
					}
				} else if r.code != ExitError {
					t.Fatalf("parent failure hidden: %+v", r)
				}
				if !strings.Contains(r.stdout, "linked child") || strings.Contains(r.stdout, "linked parent") {
					t.Fatalf("fabricated atomic success: %+v", r)
				}
				e.hs.mu.Lock()
				_, down := e.hs.rooms[space].state["m.space.child|"+child]
				_, up := e.hs.rooms[child].state["m.space.parent|"+space]
				e.hs.mu.Unlock()
				if !down || up {
					t.Fatal("partial link state was misrepresented")
				}
			})
		}
	}
}
