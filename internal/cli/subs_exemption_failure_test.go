package cli

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type exemptionReadTransport func(*http.Request) (*http.Response, error)

func (f exemptionReadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The shared classifier must not add baseline to an actual exempt global room
// when its alias lookup fails; an ordinary-room control remains repairable.
func TestReviewR7bExemptionAliasReadFailure(t *testing.T) {
	for _, cmd := range []string{"join", "sync", "plain", "preset"} {
		for _, class := range []string{"ordinary", "system", "broadcast"} {
			t.Run(cmd+"/"+class, func(t *testing.T) {
				e := newEnv(t)
				e.swarm()
				name := "mini_" + class + "_orch"
				uid := e.agent(name)
				id := e.hs.addRoom(class, uid)
				e.hs.makePublic(id)
				// Plain onboarding first resolves the explicit project alias; only its
				// subsequent classification read fails. Other commands receive an ID.
				var targetReads atomic.Int32
				var faultReads atomic.Int32
				e.configure = func(a *App) {
					base := a.HTTP.Transport
					a.HTTP = &http.Client{Transport: exemptionReadTransport(func(r *http.Request) (*http.Response, error) {
						target := strings.Contains(r.URL.Path, "/directory/room/#"+class+":"+testServer)
						if target && class != "ordinary" {
							n := targetReads.Add(1)
							allowedReads := int32(0)
							if cmd == "plain" {
								allowedReads = 1
								if class == "broadcast" {
									allowedReads = 2
								}
							}
							if n > allowedReads {
								faultReads.Add(1)
								body := `{"errcode":"M_UNAVAILABLE","error":"review alias read failure"}`
								return &http.Response{StatusCode: 503, Header: make(http.Header), ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
							}
						}
						return base.RoundTrip(r)
					})}
				}
				args := []string{"join", id}
				switch cmd {
				case "sync":
					args = []string{"subs", "sync"}
				case "plain":
					args = []string{"identity", "ensure", "--role", "orch", "--project", class, "--runtime", "none"}
				case "preset":
					args = []string{"identity", "ensure", "--role", "orch", "--project", class, "--runtime", "none", "--preset", "arbiter", "--resource", id}
				}
				puts := e.hs.CallCount(accountDataPut)
				r := e.as(name, args...)
				if class == "ordinary" {
					r.ok(t)
				}
				baseline := false
				for _, s := range e.serverSubs(name).Subs {
					if s.Room == id && s.Preset == "" {
						baseline = true
					}
				}
				if class != "ordinary" && baseline {
					t.Fatalf("classification lookup failed but %s received forbidden baseline (exit %d)", class, r.code)
				}
				if class == "ordinary" && !baseline {
					t.Fatal("ordinary positive baseline missing")
				}
				if class != "ordinary" && faultReads.Load() == 0 {
					t.Fatal("alias-read fault did not execute")
				}
				if class != "ordinary" {
					if r.code != ExitError || !strings.Contains(r.stderr, "resolve subscription exemption") || !strings.Contains(r.stderr, "HTTP 503") {
						t.Fatalf("failed alias classification did not return its specific refusal: %+v", r)
					}
					if e.hs.CallCount(accountDataPut) != puts {
						t.Fatal("failed alias classification wrote subscriptions")
					}
					// Recovery must work without changing membership or erasing intent.
					e.configure = nil
					e.as(name, args...).ok(t)
					for _, sub := range e.serverSubs(name).Subs {
						if sub.Room == id && sub.Preset == "" {
							t.Fatal("retry added a baseline in an exempt room")
						}
					}
				}
			})
		}
	}
}
