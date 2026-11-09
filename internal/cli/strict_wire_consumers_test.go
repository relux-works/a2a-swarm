package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestStrictWireSendConsumers(t *testing.T) {
	for _, caller := range []string{"topic", "post", "attach", "projector", "monitor", "card", "pin"} {
		for _, kind := range []string{"valid", "missing", "trailing"} {
			t.Run(caller+"/"+kind, func(t *testing.T) {
				var e *env
				var room string
				if caller == "projector" {
					e, room, _ = projWorld(t)
					e.project().ok(t)
					e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "event"))
				} else {
					e, room = world(t)
				}
				args := []string{}
				as := "alice"
				switch caller {
				case "topic":
					args = []string{"topic", "new", "proj", "wire", "Wire"}
				case "post", "attach", "pin":
					e.as("alice", "topic", "new", "proj", "wire", "Wire").ok(t)
					if caller == "post" {
						args = []string{"post", "proj", "--topic", "wire", "hello"}
					}
					if caller == "attach" {
						p := writeFile(t, t.TempDir(), "wire.txt", []byte("wire"))
						args = []string{"attach", "proj", "--topic", "wire", p, "file"}
					}
					if caller == "pin" {
						id := e.hs.inject(room, "@bob:test.local", msg("pin", nil))
						args = []string{"pin", "proj", id}
					}
				case "card":
					p := writeFile(t, t.TempDir(), "card.md", []byte("# Wire"))
					args = []string{"card", "set", "--file", p}
				case "projector":
					as = "proj"
					args = []string{"project", "--system", "system", "--source", "proj", "--once", "--poll", "50ms"}
				}
				hits := 0
				intercept := func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					match := r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/send/")
					if caller == "card" || caller == "pin" {
						match = r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/state/")
					}
					if !match || resp.StatusCode != 200 {
						return resp, nil
					}
					hits++
					if kind == "valid" {
						return resp, nil
					}
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					if kind == "missing" {
						raw = []byte(`{}`)
					} else {
						raw = append(raw, []byte(" {}")...)
					}
					resp.Body = io.NopCloser(strings.NewReader(string(raw)))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				}
				e.configure = func(a *App) { a.HTTP = &http.Client{Transport: predicateTransport(intercept)} }
				if caller == "monitor" {
					a := eApp(e)
					a.HTTP = &http.Client{Transport: predicateTransport(intercept)}
					s, err := a.sessionAs("alice")
					if err != nil {
						t.Fatal(err)
					}
					err = a.publishLivenessTransition(context.Background(), s, "wire", livenessIncident{key: "wire", value: "stale", names: []string{"bob"}}, room, []string{"bob"})
					if (err == nil) != (kind == "valid") || hits != 1 {
						t.Fatalf("monitor strict receipt: hits=%d err=%v", hits, err)
					}
					rec := e.state("alice").LivenessAlerts["wire"]
					if (rec.Pending == nil) != (kind == "valid") {
						t.Fatal("monitor cleared pending without ACK")
					}
					return
				}
				var result result
				if caller == "projector" {
					before := e.state("proj").NextBatch
					result = e.as(as, args...)
					after := e.state("proj").NextBatch
					if kind != "valid" && before != after {
						t.Fatal("projector advanced cursor without ACK")
					}
				} else {
					result = e.as(as, args...)
				}
				if (result.code == 0) != (kind == "valid") || hits != 1 {
					t.Fatalf("consumer=%s kind=%s hits=%d result=%+v", caller, kind, hits, result)
				}
			})
		}
	}
}

func TestStrictAliasFailureDoesNotCreate(t *testing.T) {
	for _, caller := range []string{"room", "product", "broadcast", "status"} {
		for _, kind := range []string{"valid-absent", "malformed-404", "case-404"} {
			t.Run(caller+"/"+kind, func(t *testing.T) {
				e, _ := world(t)
				creates := 0
				hits := 0
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
						if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/createRoom") {
							creates++
						}
						if r.Method == "GET" && strings.Contains(r.URL.Path, "/directory/room/") {
							hits++
							body := `{"errcode":"M_NOT_FOUND","error":"absent"}`
							if kind == "malformed-404" {
								body = `{"errcode":"M_NOT_FOUND",`
							}
							if kind == "case-404" {
								body = `{"ERRCODE":"M_NOT_FOUND"}`
							}
							return &http.Response{StatusCode: 404, Header: make(http.Header), ContentLength: int64(len(body)), Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
						}
						return http.DefaultTransport.RoundTrip(r)
					})}
				}
				var args []string
				switch caller {
				case "room":
					args = []string{"room", "new", "wire"}
				case "product":
					args = []string{"product", "new", "wire"}
				case "broadcast":
					args = []string{"broadcast-room", "init"}
				case "status":
					args = []string{"status", "init"}
				}
				r := e.as("alice", args...)
				if hits == 0 {
					t.Fatal("alias route not reached")
				}
				if kind != "valid-absent" && (creates != 0 || r.code == 0) {
					t.Fatalf("failed read treated as absence: creates=%d result=%+v", creates, r)
				}
				if kind == "valid-absent" && creates == 0 {
					t.Fatalf("valid absence control not driven: %+v", r)
				}
			})
		}
	}
}

// Existing optional decisions keep their conservative behavior on invalid
// alias evidence. They do not receive a case-folded room from the client.
func TestStrictAliasHelpers(t *testing.T) {
	for _, kind := range []string{"valid", "case", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			e, room := world(t)
			a := eApp(e)
			s, err := a.sessionAs("alice")
			if err != nil {
				t.Fatal(err)
			}
			s.client.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" && strings.Contains(r.URL.Path, "/directory/room/") {
					body := fmt.Sprintf(`{"room_id":%q}`, room)
					if kind == "case" {
						body = fmt.Sprintf(`{"ROOM_ID":%q}`, room)
					}
					if kind == "trailing" {
						body += " {}"
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: int64(len(body)), Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			id, _, err := s.resolveRoom(context.Background(), "proj")
			if (err == nil) != (kind == "valid") || kind != "valid" && id != "" {
				t.Fatalf("resolveRoom used invalid value: %q %v", id, err)
			}
		})
	}
}

func TestStrictWireStateConsumers(t *testing.T) {
	for _, caller := range []string{"link-child", "link-parent", "adopt-child", "adopt-parent", "keeper", "retire"} {
		for _, kind := range []string{"valid", "missing", "trailing"} {
			t.Run(caller+"/"+kind, func(t *testing.T) {
				e, _ := world(t)
				e.as("alice", "product", "new", "wire").ok(t)
				e.as("alice", "room", "new", "extra").ok(t)
				a := eApp(e)
				a.as = "alice"
				var out strings.Builder
				a.Stdout = &out
				a.Stderr = io.Discard
				s, err := a.sessionAs("alice")
				if err != nil {
					t.Fatal(err)
				}
				space := "!wireRoomID"
				room := "!extraRoomID"
				if caller == "retire" {
					if err := s.linkSpace(context.Background(), space, room); err != nil {
						t.Fatal(err)
					}
					out.Reset()
				}
				hits := 0
				typ := "m.space.child"
				if strings.HasSuffix(caller, "parent") {
					typ = "m.space.parent"
				}
				if caller == "keeper" {
					typ = "m.room.power_levels"
				}
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					if r.Method != http.MethodPut || !strings.Contains(r.URL.Path, "/state/"+typ+"/") || resp.StatusCode != 200 {
						return resp, nil
					}
					hits++
					if kind == "valid" {
						return resp, nil
					}
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					if kind == "missing" {
						raw = []byte(`{}`)
					} else {
						raw = append(raw, []byte(" {}")...)
					}
					resp.Body = io.NopCloser(strings.NewReader(string(raw)))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				})}
				s.client.HTTP = a.HTTP
				switch {
				case strings.HasPrefix(caller, "link"):
					err = s.linkSpace(context.Background(), space, room)
				case strings.HasPrefix(caller, "adopt"):
					err = s.adoptProductRoom(context.Background(), "wire", "extra", false)
				case caller == "keeper":
					s.ensureKeeperPower(context.Background(), space, "wire", "@bob:test.local")
				case caller == "retire":
					err = cmdRoomRetire(context.Background(), a, "extra")
				}
				if hits != 1 {
					t.Fatalf("state call not reached once: %d err=%v out=%s", hits, err, out.String())
				}
				if caller == "keeper" {
					if strings.Contains(out.String(), "not set") != (kind != "valid") {
						t.Fatalf("keeper output attested invalid ACK: %s", out.String())
					}
					return
				}
				if caller == "retire" {
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(out.String(), "not unlinked") != (kind != "valid") {
						t.Fatalf("unlink output attested invalid ACK: %s", out.String())
					}
					return
				}
				if (err == nil) != (kind == "valid") {
					t.Fatalf("state ACK caller=%s kind=%s err=%v", caller, kind, err)
				}
			})
		}
	}
}

func TestStrictWireAliasCallSites(t *testing.T) {
	for _, caller := range []string{"room-existing", "room-race", "product-existing", "product-race", "product-lobby", "product-broadcast", "product-system", "product-lobby-race", "product-broadcast-race", "product-system-race", "broadcast-existing", "broadcast-race", "status-existing", "status-race", "retire"} {
		for _, kind := range []string{"valid", "case", "trailing"} {
			t.Run(caller+"/"+kind, func(t *testing.T) {
				e, _ := world(t)
				e.as("alice", "product", "new", "wire").ok(t)
				e.as("alice", "room", "new", "extra").ok(t)
				e.as("alice", "broadcast-room", "init").ok(t)
				e.as("alice", "status", "init").ok(t)
				alias := "#wire:test.local"
				args := []string{"product", "new", "wire"}
				if strings.HasPrefix(caller, "room") {
					alias = "#extra:test.local"
					args = []string{"room", "new", "extra"}
				}
				if strings.HasPrefix(caller, "broadcast") {
					alias = "#broadcast:test.local"
					args = []string{"broadcast-room", "init"}
				}
				if strings.HasPrefix(caller, "status") {
					alias = "#status:test.local"
					args = []string{"status", "init"}
				}
				if caller == "retire" {
					alias = "#extra:test.local"
					args = []string{"room", "retire", "extra"}
				}
				if strings.Contains(caller, "lobby") {
					alias = "#wire-lobby:test.local"
				}
				if strings.Contains(caller, "product-broadcast") {
					alias = "#wire-broadcast:test.local"
				}
				if strings.Contains(caller, "product-system") {
					alias = "#wire-system:test.local"
				}
				calls := 0
				corrupted := 0
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
						match := r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/directory/room/"+alias)
						if !match {
							return http.DefaultTransport.RoundTrip(r)
						}
						calls++
						before := 0
						if strings.HasSuffix(caller, "race") {
							before = 1
						}
						if caller == "product-lobby-race" {
							before = 2
						}
						if calls <= before {
							body := `{"errcode":"M_NOT_FOUND","error":"absent"}`
							return &http.Response{StatusCode: 404, Header: make(http.Header), ContentLength: int64(len(body)), Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
						}
						resp, err := http.DefaultTransport.RoundTrip(r)
						if err != nil || resp.StatusCode != 200 {
							return resp, err
						}
						if kind == "valid" {
							return resp, nil
						}
						raw, err := io.ReadAll(resp.Body)
						resp.Body.Close()
						if err != nil {
							return nil, err
						}
						if kind == "case" {
							raw = []byte(strings.Replace(string(raw), "room_id", "ROOM_ID", 1))
						} else {
							raw = append(raw, []byte(" {}")...)
						}
						corrupted++
						resp.Body = io.NopCloser(strings.NewReader(string(raw)))
						resp.ContentLength = int64(len(raw))
						return resp, nil
					})}
				}
				r := e.as("alice", args...)
				if calls == 0 || kind != "valid" && corrupted != 1 {
					t.Fatalf("alias caller not driven: calls=%d corrupted=%d result=%+v", calls, corrupted, r)
				}
				if (r.code == 0) != (kind == "valid") {
					t.Fatalf("alias caller=%s kind=%s result=%+v", caller, kind, r)
				}
			})
		}
	}
}

func TestStrictOptionalAliasConsumers(t *testing.T) {
	for _, caller := range []string{"product-discovery", "room-sub-skip", "leave-by-id"} {
		for _, kind := range []string{"valid", "case", "trailing"} {
			t.Run(caller+"/"+kind, func(t *testing.T) {
				e, room := world(t)
				e.as("alice", "product", "new", "wire").ok(t)
				e.as("alice", "broadcast-room", "init").ok(t)
				a := eApp(e)
				a.as = "alice"
				a.Stdout = io.Discard
				a.Stderr = io.Discard
				calls := 0
				transport := predicateTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil || r.Method != "GET" || !strings.Contains(r.URL.Path, "/directory/room/") || resp.StatusCode != 200 {
						return resp, err
					}
					calls++
					if kind == "valid" {
						return resp, nil
					}
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					if kind == "case" {
						raw = []byte(strings.Replace(string(raw), "room_id", "ROOM_ID", 1))
					} else {
						raw = append(raw, []byte(" {}")...)
					}
					if caller == "leave-by-id" {
						// A permissive decoder would falsely identify the explicitly
						// supplied nonlobby room as lobby and refuse the leave.
						if kind == "case" {
							raw = []byte(fmt.Sprintf(`{"ROOM_ID":%q}`, room))
						} else {
							raw = []byte(fmt.Sprintf(`{"room_id":%q} {}`, room))
						}
					}
					resp.Body = io.NopCloser(strings.NewReader(string(raw)))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				})
				a.HTTP = &http.Client{Transport: transport}
				s, err := a.sessionAs("alice")
				if err != nil {
					t.Fatal(err)
				}
				switch caller {
				case "product-discovery":
					_, _, ok := s.productSpaceForID(context.Background(), "!wireRoomID")
					if ok != (kind == "valid") {
						t.Fatal("product discovery trusted invalid alias")
					}
				case "room-sub-skip":
					why, err := s.roomSubSkip(context.Background(), "!broadcastRoomID")
					if (why != "") != (kind == "valid") {
						t.Fatal("room subscription skipped on invalid alias")
					}
					if (err == nil) != (kind == "valid") {
						t.Fatal("invalid alias classified as an ordinary room instead of refusing")
					}
				case "leave-by-id":
					// Card-room protection must be established before an unforced
					// leave, including an explicit ID. Malformed protection metadata
					// cannot identify a safe room; --force remains the explicit escape.
					e.configure = func(a *App) { a.HTTP = &http.Client{Transport: transport} }
					r := e.as("alice", "leave", room)
					if kind == "valid" {
						r.ok(t)
					} else {
						r.failsWith(t, ExitError, "card_room_unavailable")
						if n := len(e.hs.CallsFor("POST /_matrix/client/v3/rooms/{room}/leave")); n != 0 {
							t.Fatal("unproven protection reached the leave route")
						}
						e.as("alice", "leave", room, "--force").ok(t)
					}
				}
				if calls == 0 {
					t.Fatal("optional alias route not reached")
				}
			})
		}
	}
}
