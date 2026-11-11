package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func privateProductState(caller string) string {
	return `[{"type":"m.room.create","state_key":"","content":{"type":"m.space"}},` +
		`{"type":"m.room.join_rules","state_key":"","content":{"join_rule":"invite"}},` +
		`{"type":"m.room.guest_access","state_key":"","content":{"guest_access":"forbidden"}},` +
		`{"type":"m.room.history_visibility","state_key":"","content":{"history_visibility":"joined"}},` +
		`{"type":"m.room.member","state_key":"` + caller + `","content":{"membership":"join"}}]`
}

func privateProductResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}
}

// Proves private creation has one explicit policy request and no default rooms;
// existing/raced reuse separately observes policy and the selected directory.
func TestPrivateProductSelectedCreateAndReuse(t *testing.T) {
	for _, profile := range []string{"alpha", "beta"} {
		for _, mode := range []string{"fresh", "reuse", "race"} {
			t.Run(profile+"/"+mode, func(t *testing.T) {
				f := newNamedFixture(t, true)
				seedMemberState(t, f)
				before := memberFiles(t, f.root)
				a := f.app()
				aliases, creates, states, directories := 0, 0, 0, 0
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles[profile].URL || r.Header.Get("Authorization") != "Bearer isolated-"+profile {
						t.Fatalf("wrong selected server or caller: %s", r.URL)
					}
					switch {
					case r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/directory/room/#quiet:"+profile+".invalid":
						aliases++
						if mode != "reuse" && aliases == 1 {
							return privateProductResponse(r, 404, `{"errcode":"M_NOT_FOUND","error":"absent"}`), nil
						}
						return privateProductResponse(r, 200, `{"room_id":"!quiet","servers":[]}`), nil
					case r.Method == "POST" && r.URL.Path == "/_matrix/client/v3/createRoom":
						creates++
						var request matrix.CreateRoomRequest
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Fatal(err)
						}
						if request.Preset != "private_chat" || request.Visibility != "private" || request.RoomAliasName != "quiet" || request.CreationContent["type"] != "m.space" || len(request.Invite) != 0 || len(request.PowerLevelContentOverride) != 0 || len(request.InitialState) != 3 {
							t.Fatalf("wrong private creation policy: %+v", request)
						}
						want := map[string]string{"m.room.join_rules": `{"join_rule":"invite"}`, "m.room.guest_access": `{"guest_access":"forbidden"}`, "m.room.history_visibility": `{"history_visibility":"joined"}`}
						for _, event := range request.InitialState {
							if event.StateKey != "" || want[event.Type] != string(event.Content) {
								t.Fatalf("wrong initial event: %+v", event)
							}
							delete(want, event.Type)
						}
						if len(want) != 0 {
							t.Fatal("missing private policy event")
						}
						if mode == "race" {
							return privateProductResponse(r, 400, `{"errcode":"M_ROOM_IN_USE","error":"taken"}`), nil
						}
						return privateProductResponse(r, 200, `{"room_id":"!quiet"}`), nil
					case r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/rooms/!quiet/state":
						states++
						return privateProductResponse(r, 200, privateProductState("@alice:"+profile+".invalid")), nil
					case r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/directory/list/room/!quiet":
						directories++
						return privateProductResponse(r, 200, `{"visibility":"private"}`), nil
					default:
						t.Fatalf("forbidden child/invite/power/subscription or extra request: %s %s", r.Method, r.URL)
						return nil, errors.New("unexpected request")
					}
				})}
				if code := a.Run(context.Background(), []string{"--profile", profile, "product", "new", "quiet", "--private"}); code != 0 {
					t.Fatalf("private positive refused: exit%d %s", code, f.err.String())
				}
				wantCreates, wantReads, wantAliases := 0, 1, 1
				if mode == "fresh" {
					wantCreates, wantReads = 1, 0
				}
				if mode == "race" {
					wantCreates, wantAliases = 1, 2
				}
				if creates != wantCreates || states != wantReads || directories != wantReads || aliases != wantAliases || !strings.Contains(f.out.String(), "private space #quiet !quiet") || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
					t.Fatalf("wrong effects: create%d state%d directory%d alias%d output%s", creates, states, directories, aliases, f.out.String())
				}
			})
		}
	}
}

// Proves failed qualification never converts an existing room or creates default
// children, and an uncertain create is not automatically retried.
func TestPrivateProductRefusals(t *testing.T) {
	for _, name := range []string{"public", "race-public", "missing-visibility", "duplicate-visibility", "visibility403", "visibility404", "visibility206", "visibility-network", "non-space", "public-join", "guest", "history", "caller-invite", "missing-create", "duplicate-state", "case-shadow", "state403", "alias403", "race-alias403", "create-unknown", "invalid-created-id"} {
		t.Run(name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			seedMemberState(t, f)
			before := memberFiles(t, f.root)
			a := f.app()
			aliases, creates := 0, 0
			state := privateProductState("@alice:alpha.invalid")
			switch name {
			case "non-space":
				state = strings.Replace(state, "m.space", "m.other", 1)
			case "public-join":
				state = strings.Replace(state, `"join_rule":"invite"`, `"join_rule":"public"`, 1)
			case "guest":
				state = strings.Replace(state, `"forbidden"`, `"can_join"`, 1)
			case "history":
				state = strings.Replace(state, `"history_visibility":"joined"`, `"history_visibility":"world_readable"`, 1)
			case "caller-invite":
				state = strings.Replace(state, `"membership":"join"`, `"membership":"invite"`, 1)
			case "missing-create":
				state = strings.Replace(state, `"m.room.create"`, `"custom"`, 1)
			case "duplicate-state":
				state = strings.TrimSuffix(state, "]") + `,{"type":"m.room.create","state_key":"","content":{"type":"m.space"}}]`
			case "case-shadow":
				state = strings.Replace(state, `"join_rule":"invite"`, `"join_rule":"invite","Join_Rule":"public"`, 1)
			}
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/directory/room/"):
					aliases++
					if name == "alias403" || (name == "race-alias403" && aliases == 2) {
						return privateProductResponse(r, 403, `{"errcode":"M_FORBIDDEN","error":"denied"}`), nil
					}
					if aliases == 1 && (strings.HasPrefix(name, "race-") || name == "create-unknown" || name == "invalid-created-id") {
						return privateProductResponse(r, 404, `{"errcode":"M_NOT_FOUND","error":"absent"}`), nil
					}
					return privateProductResponse(r, 200, `{"room_id":"!quiet"}`), nil
				case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/createRoom"):
					creates++
					if name == "create-unknown" {
						return nil, errors.New("synthetic lost acknowledgement")
					}
					if name == "invalid-created-id" {
						return privateProductResponse(r, 200, `{"room_id":"invalid"}`), nil
					}
					return privateProductResponse(r, 400, `{"errcode":"M_ROOM_IN_USE","error":"taken"}`), nil
				case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/state"):
					if name == "state403" {
						return privateProductResponse(r, 403, `{}`), nil
					}
					return privateProductResponse(r, 200, state), nil
				case r.Method == "GET" && strings.Contains(r.URL.Path, "/directory/list/room/"):
					status, body := 200, `{"visibility":"private"}`
					switch name {
					case "public", "race-public":
						body = `{"visibility":"public"}`
					case "missing-visibility":
						body = `{}`
					case "duplicate-visibility":
						body = `{"visibility":"public","visibility":"private"}`
					case "visibility403":
						status = 403
					case "visibility404":
						status = 404
					case "visibility206":
						status = 206
					case "visibility-network":
						return nil, errors.New("synthetic network failure")
					}
					return privateProductResponse(r, status, body), nil
				default:
					t.Fatalf("forbidden effect after refusal: %s %s", r.Method, r.URL)
					return nil, errors.New("unexpected effect")
				}
			})}
			if code := a.Run(context.Background(), []string{"product", "new", "quiet", "--private"}); code == 0 || (!strings.Contains(f.err.String(), "private Space") && !strings.Contains(f.err.String(), "private Space creation")) {
				t.Fatalf("private refusal not enforced: exit%d stderr%s", code, f.err.String())
			}
			wantCreates := 0
			if strings.HasPrefix(name, "race-") || name == "create-unknown" || name == "invalid-created-id" {
				wantCreates = 1
			}
			if creates != wantCreates || f.out.Len() != 0 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
				t.Fatalf("refusal produced effects or partial output: create%d output%s", creates, f.out.String())
			}
		})
	}
}

// Proves invalid private-mode arguments fail before session/network effects.
func TestPrivateProductArgumentRefusals(t *testing.T) {
	for _, args := range [][]string{{"quiet", "--private", "--keeper", "owner"}, {"quiet", "--private", "--projector", "relay"}, {"bad/name", "--private"}, {"quiet", "--private", "--unknown"}, {"--private"}} {
		f := newNamedFixture(t, true)
		a := f.app()
		a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
			t.Fatal("invalid arguments reached network")
			return nil, errors.New("unexpected request")
		})}
		if code := a.Run(context.Background(), append([]string{"product", "new"}, args...)); code != ExitUsage || f.out.Len() != 0 {
			t.Fatalf("wrong usage refusal exit%d stdout%s stderr%s", code, f.out.String(), f.err.String())
		}
	}
}
