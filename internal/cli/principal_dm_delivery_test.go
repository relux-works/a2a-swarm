package cli

import (
	"encoding/json"
	"testing"
	"time"
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
