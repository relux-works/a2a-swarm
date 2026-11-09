package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func TestCMOwnerSenderDebounce(t *testing.T) {
	const owner = "@Owner:example.invalid"
	for _, tc := range []struct {
		name, sender  string
		urgent, early bool
	}{
		{"owner", owner, false, true},
		{"urgent-non-owner", "@peer:example.invalid", true, false},
		{"urgent-owner", owner, true, true},
		{"owner-display-name", "@Owner:other.invalid", false, false},
		{"case-is-not-owner", "@owner:example.invalid", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var w noticeWindow
			if w.ready(base, true, false) {
				t.Fatal("ordinary neighbour did not open window")
			}
			content, _ := json.Marshal(map[string]any{"msgtype": "m.text", "body": "urgent " + owner, "urgent": tc.urgent, "displayname": owner, ContentKey: map[string]any{"from": owner, "kind": "owner-needed"}})
			items := []waitItem{{ev: &matrix.Event{Type: "m.room.message", Sender: tc.sender, Content: content}}}
			if got := w.ready(base.Add(time.Second), true, noticeOwnerSender(items, owner)); got != tc.early {
				t.Fatal("early close did not use exact Matrix sender")
			}
			if !w.ready(base.Add(30*time.Second), true, false) {
				t.Fatal("ordinary neighbour starved at fixed deadline")
			}
			if noticeOwnerSender(items, "") {
				t.Fatal("missing pin acquired priority")
			}
		})
	}
}

type cmOutput func([]byte) (int, error)

func (f cmOutput) Write(p []byte) (int, error) { return f(p) }

// Run the production loop with virtual time and the real mailbox/ingest path.
// No timer sleeps are used: a blocked sync ends on its request deadline.
func TestCMCoalescedBursts(t *testing.T) {
	for _, tc := range []struct {
		name, sender  string
		bursts        int
		urgent, early bool
	}{
		{"one-burst", "@peer:test.local", 1, false, false},
		{"two-bursts", "@peer:test.local", 2, false, false},
		{"owner", "@bob:test.local", 1, false, true},
		{"urgent-non-owner", "@peer:test.local", 1, true, false},
		{"urgent-owner", "@bob:test.local", 1, true, true},
		{"owner-display-name", "@bob:other.invalid", 1, false, false},
	} {
		bursts := tc.bursts
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				syncs := 0
				notices := 0
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				transport := waitRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/sync") {
						syncs++
						if syncs == 1 || bursts == 2 && syncs == 4 {
							id := "$first"
							if syncs == 4 {
								id = "$second"
							}
							content := map[string]any{"msgtype": "m.text", "body": "@alice:test.local recovered", "urgent": tc.urgent, "displayname": "@bob:test.local", ContentKey: map[string]any{"from": "@bob:test.local"}}
							events := []map[string]any{{"event_id": id, "type": "m.room.message", "sender": tc.sender, "content": content}}
							if !tc.early {
								events = append(events, map[string]any{"event_id": id + "-burst", "type": "m.room.message", "sender": tc.sender, "content": content})
							}
							data, _ := json.Marshal(map[string]any{"next_batch": id, "rooms": map[string]any{"join": map[string]any{"!proj:test.local": map[string]any{"timeline": map[string]any{"events": events}}}}})
							return waitResponse(r, 200, string(data))
						}
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					if strings.Contains(r.URL.Path, "/account_data/") {
						return waitResponse(r, 200, `{"v":1,"subs":[]}`)
					}
					if strings.Contains(r.URL.Path, "/state/m.room.member/") {
						return waitResponse(r, 200, `{"membership":"join"}`)
					}
					if strings.Contains(r.URL.Path, "/state/m.room.canonical_alias") {
						return waitResponse(r, 200, `{"alias":"#proj:test.local"}`)
					}
					t.Error("unexpected fake boundary")
					return nil, io.EOF
				})
				a, p, _, _ := waitRetryApp(t, "seed", transport)
				canonical, err := filepath.EvalSymlinks(p.Base)
				if err != nil {
					t.Fatal(err)
				}
				p.Base = canonical
				a.Getenv = func(k string) string {
					if k == config.EnvConfigDir {
						return p.Base
					}
					if k == EnvSessionID {
						return "monitor-session"
					}
					return ""
				}
				a.as = "alice"
				if err := ownerWrite(ctx, ownerFiles(p, "alice").path("pin"), ownerDMPin{Mode: "host", Owner: "@bob:test.local"}); err != nil {
					t.Fatal(err)
				}
				a.HTTP = &http.Client{Transport: transport}
				if _, err := config.UpdateState(p, "alice", func(st *config.State) error {
					st.Lease = &config.Lease{Holder: "monitor-session", Host: machineLabel(), PID: os.Getpid(), Expires: time.Now().Add(time.Hour)}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := config.RecordMonitorOwnerHook(p, "alice", time.Now().Add(time.Second), "muse", "monitor-session", "SessionStart", false, time.Now()); err != nil {
					t.Fatal(err)
				}
				s, err := a.session()
				if err != nil {
					t.Fatal(err)
				}
				a.Stdout = cmOutput(func(p []byte) (int, error) {
					notices++
					var n notice
					wantTotal := 2
					if tc.early {
						wantTotal = 1
					}
					if json.Unmarshal(p, &n) != nil || n.Total != wantTotal || strings.Contains(string(p), "recovered") {
						t.Fatal("not a closed one-event projection")
					}
					wantTime := time.Duration(notices) * 30 * time.Second
					if tc.early {
						wantTime = 0
					}
					if elapsed := time.Since(start); elapsed != wantTime {
						t.Fatalf("coalescing deadline %v", elapsed)
					}
					if notices == bursts {
						cancel()
					}
					return len(p), nil
				})
				record := cliMonitorRecord()
				if err := config.WithMonitorGeneration(s.paths, s.name, time.Now().Add(time.Second), config.MonitorArm, record, func(g *config.MonitorGeneration) error {
					return a.noticeLoop(ctx, s, g, record, deliveryFilter{}, true, false)
				}); err != nil {
					t.Fatal(err)
				}
				if notices != bursts {
					t.Fatal("burst count differs")
				}
				mb, err := config.OpenMailbox(s.paths, s.name)
				wantPending := 2 * bursts
				if tc.early {
					wantPending = bursts
				}
				if err != nil || len(mb.Pending()) != wantPending || mb.Received("$first") || mb.Received("$second") {
					t.Fatal("watch lost or receipted mail")
				}
				st, err := config.LoadState(s.paths, s.name)
				if err != nil || !st.Monitor.LastNoticeAt.IsZero() {
					t.Fatal("ordinary stdout became idle wake evidence")
				}
			})
		})
	}
}
