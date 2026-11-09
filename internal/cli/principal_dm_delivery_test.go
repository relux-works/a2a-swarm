package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// The room-wide DM row and mandatory exact-sender coverage have independent
// lifecycles. Unset must preserve principal delivery, including a stale config
// that names a dedicated principal row instead of its own subscription.
func TestPrincipalDMSubscriptionIndependence(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "stale-sub-id"}[stale], func(t *testing.T) {
			e, f, who := principalFixture(t)
			e.as("bob", "host", "principals", "set", who).ok(t)
			e.as("bob", "principal-dm", "ensure", who).ok(t)
			d := principalEntry(t, f, who)
			var list SubList
			if err := json.Unmarshal(e.hs.accountDataOf("@bob:"+testServer, SubsType), &list); err != nil {
				t.Fatal(err)
			}
			if len(missingRoomPrincipals(d.Room, []string{who}, list, time.Now())) != 0 {
				t.Fatal("DM room lacks independent principal coverage")
			}
			if stale {
				if !list.remove(d.SubID) {
					t.Fatal("missing DM subscription")
				}
				for _, sub := range list.Subs {
					if protectedPrincipalSub(sub, []string{who}) {
						d.SubID = sub.ID
					}
				}
				state, err := principalLoad(f)
				if err != nil {
					t.Fatal(err)
				}
				state.DMs[who] = d
				ownerPut(t, f.path("json"), state)
				e.hs.setAccountData("@bob:"+testServer, SubsType, list)
			}
			e.as("bob", "principal-dm", "unset", who).ok(t)
			if err := json.Unmarshal(e.hs.accountDataOf("@bob:"+testServer, SubsType), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Subs) != 1 || !protectedPrincipalSub(list.Subs[0], []string{who}) {
				t.Fatalf("unset lost principal coverage or retained DM row: %+v", list)
			}
			ev := principalFixtureEvent(who)
			if !list.Subs[0].matches(ev, d.Room, Header{}, false) {
				t.Fatal("plain principal message no longer matches")
			}
			e.as("bob", "principal-dm", "ensure", who).ok(t)
			next := principalEntry(t, f, who)
			if next.Room == d.Room {
				t.Fatal("re-ensure reused retired room")
			}
			if err := json.Unmarshal(e.hs.accountDataOf("@bob:"+testServer, SubsType), &list); err != nil {
				t.Fatal(err)
			}
			for _, room := range []string{d.Room, next.Room} {
				if len(missingRoomPrincipals(room, []string{who}, list, time.Now())) != 0 {
					t.Fatalf("lost principal coverage in %s", room)
				}
			}
		})
	}
}

// H3: a DM pin for a non-host-principal never adds authority. All pre-existing
// credentials, host pins/principals, and state (including scope/custody fields)
// remain byte-identical across ensure, activation, say and unset. The ordinary
// subscription classification is identical in a DM and an unrelated room.
func TestPrincipalDMNoTrustChange(t *testing.T) {
	for _, activateBy := range []string{"ensure", "say"} {
		t.Run(activateBy, func(t *testing.T) {
			e, f, who := principalFixture(t)
			e.as("bob", "host", "principals", "set", "@alice:"+testServer).ok(t)
			if _, err := config.UpdateState(e.paths(), "bob", func(st *config.State) error {
				now := time.Now()
				scope := config.HostScope{Agent: "bob", Member: "@bob:" + testServer, Connection: "test", Configuration: "own", Session: "host-session", NativeThread: "thread", Generation: "generation", Consumer: "consumer"}
				lease := config.Lease{Holder: scope.Session, Host: "fixture", PID: 123, Expires: now.Add(time.Hour)}
				st.Lease = &lease
				return st.EnrollHost(config.HostAttachment{Reference: "existing-trust", Scope: scope, Lease: lease}, scope, now)
			}); err != nil {
				t.Fatal(err)
			}
			snapshot := func() map[string]string {
				out := map[string]string{}
				err := filepath.WalkDir(e.dir, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.IsDir() {
						return nil
					}
					// These are the only intended lifecycle writes. Scope/credentials/state
					// have no exemption; absent new trust files are checked by inventory too.
					if strings.HasSuffix(path, ".lock") || path == f.path("json") || path == ownerFiles(e.paths(), "bob").path("archive-exclude") {
						return nil
					}
					b, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					out[path] = string(b)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			before := snapshot()
			check := func() {
				t.Helper()
				if !reflect.DeepEqual(before, snapshot()) {
					t.Fatal("DM lifecycle changed authority/state bytes or file inventory")
				}
			}
			e.as("bob", "principal-dm", "ensure", who).ok(t)
			check()
			d := principalEntry(t, f, who)
			principalJoin(e, d.Room, who)
			if activateBy == "ensure" {
				e.as("bob", "principal-dm", "ensure", who).ok(t)
				check()
			}
			e.as("bob", "principal-dm", "say", who, "ordinary text").ok(t)
			if !principalEntry(t, f, who).Active {
				t.Fatal("activation failed")
			}
			check()
			creds, err := config.LoadCredentials(e.paths(), "bob")
			if err != nil {
				t.Fatal(err)
			}
			a := &App{Now: time.Now}
			s := &session{app: a, paths: e.paths(), name: "bob", creds: creds, server: testServer, client: &matrix.Client{}}
			configured, err := ownerFiles(e.paths(), "bob").principals()
			if err != nil {
				t.Fatal(err)
			}
			if contains(configured, who) {
				t.Fatal("principal unexpectedly trusted")
			}
			ev := principalFixtureEvent(who)
			ev.EventID = "$same-principal"
			ev.OriginServerTS = time.Now().UnixMilli()
			var entries []config.MailEntry
			for _, room := range []string{d.Room, "!elsewhere:test.local"} {
				var resp matrix.SyncResponse
				raw, _ := json.Marshal(map[string]any{"rooms": map[string]any{"join": map[string]any{room: map[string]any{"timeline": map[string]any{"events": []matrix.Event{ev}}}}}})
				if err := json.Unmarshal(raw, &resp); err != nil {
					t.Fatal(err)
				}
				b, err := s.collectBatchWithPrincipals(context.Background(), resp, "previous", config.State{}, []Sub{{ID: d.SubID, SubSelector: SubSelector{Room: room, AllEvents: true}}}, nil, map[string]string{room: "room"}, configured)
				if err != nil || len(b.entries) != 1 {
					t.Fatal("classification", b, err)
				}
				entry := b.entries[0]
				if entry.Sender != who || entry.Rel != string(relSub) || entry.Kind != config.MailEvent || entry.Sub != d.SubID || !bytes.Equal(entry.Event, mustPrincipalEvent(t, ev)) {
					t.Fatalf("elevated/reclassified message %+v", entry)
				}
				entry.Room = ""
				entries = append(entries, entry)
			}
			if !reflect.DeepEqual(entries[0], entries[1]) {
				t.Fatal("DM changes classification", entries)
			}
			e.as("bob", "principal-dm", "unset", who).ok(t)
			check()
		})
	}

}

func mustPrincipalEvent(t *testing.T, ev matrix.Event) []byte {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
