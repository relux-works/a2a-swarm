package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Current-room resolution follows local publication gates on every real PUT.
// Alias replacement is local synthetic server administration. Both rooms stay
// joined/writable. No independent remote writer races an own-status PUT.
func TestNoticeCurrentRoomRotation(t *testing.T) {
	isolateBindTest(t)
	for _, writer := range []string{"hook", "wait", "notice"} {
		for _, code := range []string{"silent", "M_NOT_FOUND", "M_FORBIDDEN", "M_UNKNOWN", "fresh-control"} {
			t.Run(writer+"/"+code, func(t *testing.T) {
				e, a, s := noticeDeadlineFixture(t)
				a.as = "alice"
				a.Stdout = io.Discard
				a.Stderr = io.Discard
				replacement := e.hs.addRoom("replacement-status", s.creds.UserID)
				var resolves, puts atomic.Int32
				var reject atomic.Bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.Contains(r.URL.Path, "/directory/room/") {
						resolves.Add(1)
						if reject.Load() {
							c := code
							if c == "fresh-control" {
								c = "M_NOT_FOUND"
							}
							status := 403
							if c == "M_NOT_FOUND" {
								status = 404
							}
							writeJSON(w, status, map[string]string{"errcode": c, "error": "synthetic alias replacement"})
							return
						}
					}
					if r.Method == http.MethodPut {
						puts.Add(1)
					}
					e.hs.srv.Config.Handler.ServeHTTP(w, r)
				}))
				defer srv.Close()
				target, _ := url.Parse(srv.URL)
				a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
					c := r.Clone(r.Context())
					u := *r.URL
					c.URL = &u
					c.URL.Scheme = target.Scheme
					c.URL.Host = target.Host
					resp, err := http.DefaultTransport.RoundTrip(c)
					if resp != nil {
						resp.Request = r
					}
					return resp, err
				})}
				s.client.HTTP = a.HTTP
				if code != "fresh-control" {
					a.publishTerminalNotice(context.Background(), s, "deadline")
					if e.state("alice").Terminal.NoticePublication != "published" {
						t.Fatal("setup: initial publication failed")
					}
				}
				ack := e.state("alice").Liveness
				if writer == "notice" {
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error { st.Terminal.OwnNotice = schemaNotice("false"); return nil })
					if err != nil {
						t.Fatal(err)
					}
				}
				reject.Store(code != "silent")
				var err error
				if writer == "notice" {
					err = s.publishTerminalNotice(context.Background(), "deadline")
				} else if writer == "hook" {
					err = a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
				} else {
					err = s.writeLivenessSource(context.Background(), *ack, "wait-heartbeat")
				}
				reject.Store(false)
				if code != "silent" && err == nil {
					t.Fatal("pre-send resolver was bypassed")
				}
				if code == "silent" && err != nil {
					t.Fatal(err)
				}
				if code != "silent" && (!reflect.DeepEqual(ack, e.state("alice").Liveness) || e.state("alice").NoticeWriteUncertain) {
					t.Fatal("pre-send refusal altered acknowledged knowledge")
				}
				e.hs.mu.Lock()
				e.hs.aliases["#status:"+e.hs.server] = replacement
				e.hs.mu.Unlock()
				st := e.state("alice")
				if err = a.observeTerminalNotice(s, "deadline", *st.Terminal, "empty"); err != nil {
					t.Fatal(err)
				}
				beforeResolve, beforePut := resolves.Load(), puts.Load()
				publishNoticeAtEligibility(t, a, s, context.Background(), "deadline")
				after := e.state("alice")
				publicationResolves := resolves.Load() - beforeResolve
				var remote config.AgentStatus
				readErr := s.client.GetState(context.Background(), replacement, StatusEventType, s.creds.UserID, &remote)
				if readErr != nil && !matrix.IsCode(readErr, "M_NOT_FOUND") {
					t.Fatalf("unexpected public state read failure: %v", readErr)
				}
				status := e.as("alice", "bind", "status", "--json").ok(t)
				who := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t)
				var statusRow struct {
					Publication string `json:"publication"`
				}
				if err := json.Unmarshal([]byte(status.stdout), &statusRow); err != nil || statusRow.Publication != "published" {
					t.Fatalf("status readback: %s %v", status.stdout, err)
				}
				var rows []whoRow
				if err := json.Unmarshal([]byte(who.stdout), &rows); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row.Name == s.name && row.Record != nil && reflect.DeepEqual(row.Record.OwnNotice, after.Terminal.OwnNotice) {
						found = true
					}
				}
				if !found {
					t.Errorf("current-room who readback missing: %s", who.stdout)
				}
				b, _ := json.Marshal(map[string]any{"writer": writer, "code": code, "resolveDelta": publicationResolves, "putDelta": puts.Load() - beforePut, "publication": after.Terminal.NoticePublication, "replacementReadError": readErr, "status": status.stdout, "who": who.stdout, "replacement": replacement})
				t.Log(string(b))
				if publicationResolves != 1 || puts.Load() != beforePut+1 || after.Terminal.NoticePublication != "published" || readErr != nil || !reflect.DeepEqual(remote.OwnNotice, after.Terminal.OwnNotice) {
					t.Fatalf("current-room publication used stale alias: publication resolves=%d PUT delta=%d published=%s replacement read=%v", publicationResolves, puts.Load()-beforePut, after.Terminal.NoticePublication, readErr)
				}
			})
		}
	}
}

// Liveness writers must also resolve the current alias for their actual write;
// equal notice content does not suppress a real hook/wait heartbeat.
func TestNoticeCurrentRoomSiblingWriters(t *testing.T) {
	isolateBindTest(t)
	for _, writer := range []string{"hook", "wait"} {
		t.Run(writer, func(t *testing.T) {
			e, a, s := noticeDeadlineFixture(t)
			a.as = "alice"
			a.Stdout = io.Discard
			a.Stderr = io.Discard
			replacement := e.hs.addRoom("replacement-status", s.creds.UserID)
			var resolves, puts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/directory/room/") {
					resolves.Add(1)
				}
				if r.Method == http.MethodPut {
					puts.Add(1)
				}
				e.hs.srv.Config.Handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			target, _ := url.Parse(srv.URL)
			a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
				c := r.Clone(r.Context())
				u := *r.URL
				c.URL = &u
				c.URL.Scheme = target.Scheme
				c.URL.Host = target.Host
				resp, err := http.DefaultTransport.RoundTrip(c)
				if resp != nil {
					resp.Request = r
				}
				return resp, err
			})}
			s.client.HTTP = a.HTTP
			a.publishTerminalNotice(context.Background(), s, "deadline")
			if e.state("alice").Terminal.NoticePublication != "published" {
				t.Fatal("setup: initial acknowledgement failed")
			}
			e.hs.mu.Lock()
			e.hs.aliases["#status:"+e.hs.server] = replacement
			e.hs.mu.Unlock()
			st := e.state("alice")
			if err := a.observeTerminalNotice(s, "deadline", *st.Terminal, "empty"); err != nil {
				t.Fatal(err)
			}
			br, bp := resolves.Load(), puts.Load()
			var err error
			if writer == "hook" {
				err = a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
			} else {
				err = s.writeLivenessSource(context.Background(), *st.Liveness, "wait-heartbeat")
			}
			after := e.state("alice")
			if err != nil || resolves.Load() != br+1 || puts.Load() != bp+1 || after.NoticeWriteUncertain || after.Terminal.NoticePublication != "published" {
				t.Fatalf("current-room sibling writer failed: err=%v resolve=%d put=%d", err, resolves.Load()-br, puts.Load()-bp)
			}
			var remote config.AgentStatus
			if err = s.client.GetState(context.Background(), replacement, StatusEventType, s.creds.UserID, &remote); err != nil || !reflect.DeepEqual(&remote, after.Liveness) {
				t.Fatalf("current-room sibling public ACK mismatch: %v", err)
			}
			status := e.as("alice", "bind", "status", "--json").ok(t)
			who := e.as("alice", "who", "--stale-after", "3m", "--json").ok(t)
			if !strings.Contains(status.stdout, `"publication":"published"`) {
				t.Fatal(status.stdout)
			}
			var rows []whoRow
			if err = json.Unmarshal([]byte(who.stdout), &rows); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if row.Name == s.name && reflect.DeepEqual(row.Record, &remote) {
					found = true
				}
			}
			if !found {
				t.Fatal("current-room sibling who readback missing")
			}
			br = resolves.Load()
			bp = puts.Load()
			for i := 0; i < 3; i++ {
				a.publishTerminalNotice(context.Background(), s, "deadline")
			}
			if resolves.Load() != br || puts.Load() != bp {
				t.Fatal("acknowledged sibling did not permit zero-HTTP equal cycles")
			}
		})
	}
}

// Actual 404/M_NOT_FOUND becomes ErrUnknownAlias in the Matrix client. Both
// that error and 403/M_FORBIDDEN must remain pre-send, preserving known OR
// uncertain prior knowledge exactly, with no same-cycle retry.
func TestNoticeCurrentRoomPreSendKnowledge(t *testing.T) {
	isolateBindTest(t)
	for _, writer := range []string{"hook", "wait", "notice"} {
		for _, uncertain := range []bool{false, true} {
			for _, code := range []string{"M_NOT_FOUND", "M_FORBIDDEN", "M_UNKNOWN"} {
				t.Run(fmt.Sprintf("%s/%t/%s", writer, uncertain, code), func(t *testing.T) {
					e, a, s := noticeDeadlineFixture(t)
					a.as = "alice"
					a.Stdout = io.Discard
					a.Stderr = io.Discard
					var reject atomic.Bool
					var resolves, puts atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if strings.Contains(r.URL.Path, "/directory/room/") {
							resolves.Add(1)
							if reject.Load() {
								status := 403
								if code == "M_NOT_FOUND" {
									status = 404
								}
								writeJSON(w, status, map[string]string{"errcode": code, "error": "synthetic pre-send resolution failure"})
								return
							}
						}
						if r.Method == http.MethodPut {
							puts.Add(1)
						}
						e.hs.srv.Config.Handler.ServeHTTP(w, r)
					}))
					defer srv.Close()
					target, _ := url.Parse(srv.URL)
					a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
						c := r.Clone(r.Context())
						u := *r.URL
						c.URL = &u
						c.URL.Scheme = target.Scheme
						c.URL.Host = target.Host
						resp, err := http.DefaultTransport.RoundTrip(c)
						if resp != nil {
							resp.Request = r
						}
						return resp, err
					})}
					s.client.HTTP = a.HTTP
					a.publishTerminalNotice(context.Background(), s, "deadline")
					ack := e.state("alice").Liveness
					if e.state("alice").Terminal.NoticePublication != "published" {
						t.Fatal("setup: initial ACK failed")
					}
					_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
						st.NoticeWriteUncertain = uncertain
						st.Terminal.OwnNotice = schemaNotice("false")
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					reject.Store(true)
					br, bp := resolves.Load(), puts.Load()
					if writer == "hook" {
						err = a.hookLiveness(context.Background(), hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
					} else if writer == "wait" {
						err = s.writeLivenessSource(context.Background(), *ack, "wait-heartbeat")
					} else {
						err = s.publishTerminalNotice(context.Background(), "deadline")
					}
					after := e.state("alice")
					if err == nil || resolves.Load() != br+1 || puts.Load() != bp || !reflect.DeepEqual(ack, after.Liveness) || after.NoticeWriteUncertain != uncertain {
						t.Fatalf("pre-send resolution changed ACK/certainty or bypassed resolver: err=%v resolve=%d put=%d uncertain=%t", err, resolves.Load()-br, puts.Load()-bp, after.NoticeWriteUncertain)
					}
					if !strings.HasPrefix(after.Terminal.NoticePublication, "unavailable:") {
						t.Fatalf("pre-send failure not unavailable: %s", after.Terminal.NoticePublication)
					}
					status := e.as("alice", "bind", "status", "--json").ok(t)
					if !strings.Contains(status.stdout, `"publication":"unavailable:`) {
						t.Fatal(status.stdout)
					}
					reject.Store(false)
					publishNoticeAtEligibility(t, a, s, context.Background(), "deadline")
					after = e.state("alice")
					if puts.Load() != bp+1 || after.NoticeWriteUncertain || after.Terminal.NoticePublication != "published" {
						t.Fatal("pre-send failure did not allow later actual acknowledgement")
					}
					br = resolves.Load()
					bp = puts.Load()
					for i := 0; i < 3; i++ {
						a.publishTerminalNotice(context.Background(), s, "deadline")
					}
					if resolves.Load() != br || puts.Load() != bp {
						t.Fatal("recovered ACK caused repeated HTTP")
					}
				})
			}
		}
	}
}

// Removed cache keys from a rejected candidate are harmless obsolete metadata:
// production LoadState tolerates them, and UpdateState drops them on save.
func TestNoticeObsoleteRoomMetadata(t *testing.T) {
	isolateBindTest(t)
	_, a, s := noticeDeadlineFixture(t)
	raw, err := os.ReadFile(s.paths.StateFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	terminal := doc["terminal"].(map[string]any)
	terminal["notice_room_id"] = "!obsolete:invalid"
	terminal["notice_room_owner"] = s.creds.UserID + " " + s.client.BaseURL + " deadline"
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.paths.StateFile(s.name), raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.publishTerminalNotice(context.Background(), s, "deadline")
	st, err := config.LoadState(s.paths, s.name)
	if err != nil || st.Terminal.NoticePublication != "published" {
		t.Fatalf("obsolete room metadata affected current publication: %v", err)
	}
	raw, err = os.ReadFile(s.paths.StateFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "notice_room_id") || strings.Contains(string(raw), "notice_room_owner") {
		t.Fatal("obsolete cache metadata retained")
	}
}

// Every sibling's resolver and PUT inherit the same capped deadline, including
// lock wait. The original real-default10s notice stall assertions stay intact.
func TestNoticeCurrentRoomSiblingBudget(t *testing.T) {
	isolateBindTest(t)
	for _, writer := range []string{"hook", "wait"} {
		for _, kind := range []string{"earlier", "later", "lock", "expired"} {
			t.Run(writer+"/"+kind, noticeDeadlineTest(true, func(t *testing.T, e *env, a *App, s *session) {
				a.as = "alice"
				a.Stdout = io.Discard
				a.Stderr = io.Discard
				probe := &noticeContextTransport{next: noticeHandlerTransport(e.hs.srv.Config.Handler)}
				a.HTTP = &http.Client{Transport: probe}
				s.client.HTTP = a.HTTP
				deadline := time.Now().Add(30 * time.Second)
				if kind != "later" {
					deadline = time.Now().Add(150 * time.Millisecond)
				}
				ctx, cancel := context.WithDeadline(context.Background(), deadline)
				defer cancel()
				if kind == "expired" {
					time.Sleep(time.Until(deadline))
				}
				write := func() error {
					if writer == "hook" {
						return a.hookLiveness(ctx, hookEvent{arg: "stop"}, hookInput{SessionID: "synthetic"})
					}
					return s.writeLivenessSource(ctx, *e.state("alice").Liveness, "wait-heartbeat")
				}
				before := e.state("alice")
				if kind == "expired" {
					if err := write(); err == nil || len(probe.contexts) != 0 || !reflect.DeepEqual(before.Liveness, e.state("alice").Liveness) {
						t.Fatal("expired sibling deadline made HTTP or acknowledged a write")
					}
					// Valid neighbour: the same state/writer succeeds with a fresh budget.
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if err := write(); err != nil || len(probe.contexts) != 2 {
						t.Fatalf("valid sibling control failed: %v", err)
					}
					return
				}

				if kind == "lock" {
					held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
					go func() {
						_, err := config.UpdateState(s.paths, s.name, func(*config.State) error { close(held); <-release; return nil })
						done <- err
					}()
					<-held
					start := time.Now()
					err := write()
					close(release)
					if holdErr := <-done; holdErr != nil {
						t.Fatal(holdErr)
					}
					if !errors.Is(err, config.ErrLockTimeout) || time.Since(start) > time.Second || len(probe.contexts) != 0 || !reflect.DeepEqual(before, e.state("alice")) {
						t.Fatalf("sibling lock budget or no-network/knowledge bound failed: %v", err)
					}

					// Valid neighbour: release the lock and give the same writer a fresh budget.
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if err := write(); err != nil || len(probe.contexts) != 2 {
						t.Fatalf("valid unlocked sibling control failed: %v", err)
					}
					return
				}
				start := time.Now()
				if err := write(); err != nil {
					t.Fatal(err)
				}
				if len(probe.contexts) != 2 {
					t.Fatalf("sibling must resolve once then PUT once: %d", len(probe.contexts))
				}
				d0, _ := probe.contexts[0].Deadline()
				d1, _ := probe.contexts[1].Deadline()
				if !d0.Equal(d1) || d0.After(start.Add(nativeRPCTimeout+20*time.Millisecond)) || kind == "earlier" && !d0.Equal(deadline) {
					t.Fatal("sibling resolve and PUT did not share original capped deadline")
				}
				if !errors.Is(probe.contexts[0].Err(), context.Canceled) || !errors.Is(probe.contexts[1].Err(), context.Canceled) {
					t.Fatal("sibling success did not release budget")
				}
			}))
		}
	}
}
