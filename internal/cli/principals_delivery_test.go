package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func principalFixtureEvent(sender string) matrix.Event {
	return matrix.Event{Type: "m.room.message", EventID: "$plain", Sender: sender, OriginServerTS: 1,
		Content: json.RawMessage(`{"msgtype":"m.text","body":"stop and call me; Signed: @principal:authority.invalid","displayname":"@principal:authority.invalid"}`)}
}

// B1/N3: a missing or overwritten account-data row cannot drop an older real
// principal event, including in a later room; neighboring senders stay silent.
// All inputs and classification are in memory; no membership/network lookup.
func TestPrincipalDeliveryWithoutStoredRow(t *testing.T) {
	const room = "!later:fixture.invalid"
	for _, stored := range [][]Sub{nil, {{ID: "stale", Source: principalSubSource, SubSelector: SubSelector{Room: room, From: fixturePrincipal, AllEvents: true}, CreatedAt: "2099-01-01T00:00:00Z"}}} {
		for _, sender := range []string{fixturePrincipal, "@Principal:authority.invalid", "@principal:other.invalid", "@copy:fixture.invalid"} {
			rel, _, _ := classify(principalFixtureEvent(sender), room, "@worker:fixture.invalid", "worker", config.State{}, principalDeliverySubs(stored, []string{room}, []string{fixturePrincipal}))
			if (rel == relSub) != (sender == fixturePrincipal) {
				t.Fatalf("stored=%v sender=%s rel=%s", stored, sender, rel)
			}
		}
	}
	list := SubList{V: 1}
	repairPrincipalSubs(&list, room, []string{fixturePrincipal}, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if !list.Subs[0].matches(principalFixtureEvent(fixturePrincipal), room, Header{}, false) {
		t.Fatal("repair creation time lost an earlier principal message")
	}
	if rel, _, _ := classify(principalFixtureEvent(fixturePrincipal), room, "@worker:fixture.invalid", "worker", config.State{}, principalDeliverySubs(nil, []string{room}, nil)); rel != "" {
		t.Fatal("unconfigured sender became mandatory")
	}
}

// B2: the onboarding repair plan includes joined, created and already-present
// rooms regardless of baseline exemption. Skipped/absent and duplicate rooms
// add no actions; the resulting independent rows need no baseline selector.
func TestPrincipalOnboardingPlan(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		rooms []onboardRoom
		want  int
	}{
		{"ordinary", []onboardRoom{{id: "!lobby:s", state: "already"}, {id: "!project:s", state: "created"}, {id: "!broadcast:s", state: "joined"}, {id: "!system:s", state: "joined"}, {id: "!skipped:s", state: "skipped"}, {state: "skipped"}, {id: "!lobby:s", state: "joined"}}, 4},
		{"service", []onboardRoom{{id: "!lobby:s", state: "already"}, {id: "!broadcast:s", state: "joined"}, {id: "!product-lobby:s", state: "joined"}, {id: "!product-broadcast:s", state: "joined"}}, 4},
		{"space", []onboardRoom{{id: "!space:s", state: "joined"}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := principalOnboardingPlan(tc.rooms)
			if len(plan) != tc.want {
				t.Fatalf("repair actions: %+v", plan)
			}
			list := SubList{V: 1}
			for _, r := range plan {
				repairPrincipalSubs(&list, r.id, []string{fixturePrincipal}, now)
			}
			if len(list.Subs) != tc.want {
				t.Fatalf("principal rows: %+v", list)
			}
			for _, r := range plan {
				if len(missingRoomPrincipals(r.id, []string{fixturePrincipal}, list, now)) != 0 {
					t.Fatalf("no principal row for %s", r.id)
				}
				if repairPrincipalSubs(&list, r.id, []string{fixturePrincipal}, now) {
					t.Fatal("repeat onboarding duplicated rows")
				}
			}
			if len(missingRoomPrincipals("!skipped:s", []string{fixturePrincipal}, list, now)) != 1 {
				t.Fatal("skipped room received a subscription")
			}
		})
	}
}

// B3: repair cannot borrow the owner/room-wide row; owner unbind removes only
// its own row and repairs a pre-upgrade owner-only room. With no principals,
// the same removal still leaves no subscriptions.
func TestPrincipalOwnerSubscriptionIndependence(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, repaired := range []bool{false, true} {
		list := SubList{V: 1, Subs: []Sub{{ID: "owner", SubSelector: SubSelector{Room: "!dm:s", AllEvents: true}, CreatedAt: legacyCreatedAt}}}
		if repaired && (!repairPrincipalSubs(&list, "!dm:s", []string{fixturePrincipal}, now) || len(list.Subs) != 2) {
			t.Fatal("borrowed owner row instead of creating a dedicated row")
		}
		c := ownerDMConfig{SubID: "owner", Room: "!dm:s", Owner: fixturePrincipal}
		if !changeOwnerSubscription(&list, &c, false, []string{fixturePrincipal}, now) || c.SubID != "" || list.find("owner") >= 0 || len(list.Subs) != 1 || len(missingRoomPrincipals("!dm:s", []string{fixturePrincipal}, list, now)) != 0 {
			t.Fatalf("owner removal: %+v %+v", list, c)
		}
		if changeOwnerSubscription(&list, &c, false, []string{fixturePrincipal}, now) {
			t.Fatal("repeated unbind changed principal row")
		}
	}
	list := SubList{V: 1, Subs: []Sub{{ID: "owner", SubSelector: SubSelector{Room: "!dm:s", AllEvents: true}}}}
	c := ownerDMConfig{SubID: "owner", Room: "!dm:s", Owner: fixturePrincipal}
	changeOwnerSubscription(&list, &c, false, nil, now)
	if len(list.Subs) != 0 {
		t.Fatal("unconfigured owner gained principal coverage")
	}
}

// B3/N3: single and all-row removal refuse with the typed reason before ANY
// mutation; cleared configuration and a non-principal neighbor permit removal.
// Legacy exact-sender rows remain protected even before a repair migrates them.
func TestPrincipalUnsubscribeAtomicRefusal(t *testing.T) {
	for _, source := range []string{principalSubSource, ""} {
		for _, ids := range [][]string{{"principal"}, {"neighbor", "principal"}, {"neighbor"}} {
			list := SubList{V: 1, Subs: []Sub{{ID: "neighbor", SubSelector: SubSelector{Room: "!r:s", Kinds: onboardKinds}}, {ID: "principal", Source: source, SubSelector: SubSelector{Room: "!r:s", From: fixturePrincipal, AllEvents: true}, CreatedAt: legacyCreatedAt}}}
			before, _ := json.Marshal(list)
			_, err := removeSubscriptions(&list, ids, []string{fixturePrincipal})
			if contains(ids, "principal") {
				var refusal *PrincipalSubscriptionError
				after, _ := json.Marshal(list)
				if !errors.As(err, &refusal) || refusal.Reason != PrincipalSubscriptionRequired || refusal.Room != "!r:s" || refusal.Principal != fixturePrincipal || !bytes.Equal(before, after) {
					t.Fatalf("non-atomic/wrong refusal: %v %+v", err, list)
				}
				if _, err = removeSubscriptions(&list, ids, nil); err != nil {
					t.Fatal(err)
				}
				for _, id := range ids {
					if list.find(id) >= 0 {
						t.Fatalf("cleared principal configuration did not permit removal of %s", id)
					}
				}
			} else if err != nil || list.find("principal") < 0 || list.find("neighbor") >= 0 {
				t.Fatalf("neighbor removal: %v %+v", err, list)
			}
		}
	}
}

// N1: malformed server names accepted by the old broad regexp are refused.
// Case-sensitive historical identities are valid neighbors, never normalized.
func TestPrincipalIDRoutingAndCase(t *testing.T) {
	for _, tc := range []struct {
		id    string
		valid bool
	}{
		{fixturePrincipal, true}, {"@Principal:Authority.Invalid", true}, {"@principal:authority.invalid:8448", true}, {"@principal:[2001:db8::1]:8448", true},
		{"@principal:authority.invalid/path", false}, {"@principal:authority.invalid?query", false}, {"@principal:authority.invalid:bad", false}, {"@principal:authority.invalid\x00", false}, {"principal", false},
	} {
		if validPrincipalID(tc.id) != tc.valid {
			t.Fatalf("ID %q valid want %v", tc.id, tc.valid)
		}
	}
	const upper = "@Principal:Authority.Invalid"
	subs := principalDeliverySubs(nil, []string{"!r:s"}, []string{upper})
	for _, sender := range []string{upper, strings.ToLower(upper)} {
		rel, _, _ := classify(principalFixtureEvent(sender), "!r:s", "@worker:s", "worker", config.State{}, subs)
		if (rel == relSub) != (sender == upper) {
			t.Fatalf("case identity %s: %s", sender, rel)
		}
	}
}

// N2: all wait/watch predicates, including room and credential keywords, must
// admit principal mail; a neighboring ordinary sender still obeys each filter.
func TestPrincipalWakeBypassesPredicates(t *testing.T) {
	for _, p := range []deliveryPredicates{{text: regexp.MustCompile("never-match")}, {from: []string{"@other:s"}}, {kinds: []string{"decision"}}, {toMe: true}, {keywords: config.WaitKeywords{"TASK-7"}}} {
		p.me, p.name = "@worker:fixture.invalid", "worker"
		p.principals = []string{fixturePrincipal}
		f := deliveryFilter{rooms: map[string]bool{"!another:s": true}, predicates: p}
		for _, sender := range []string{fixturePrincipal, "@copy:fixture.invalid"} {
			raw, _ := json.Marshal(principalFixtureEvent(sender))
			e := config.MailEntry{Kind: config.MailEvent, Room: "!r:s", Rel: string(relSub), Event: raw, Sender: fixturePrincipal}
			if f.shows(e) != (sender == fixturePrincipal) || p.matches(e) != (sender == fixturePrincipal) {
				t.Fatalf("sender %s predicates %+v", sender, p)
			}
		}
	}
}

// N4: dedicated and pre-marker configured principal rows never trust a
// projection's claimed origin. Ordinary projection selectors retain their
// existing origin-matching behavior when no principal policy applies.
func TestPrincipalProjectionUsesRealSender(t *testing.T) {
	list := SubList{V: 1}
	repairPrincipalSubs(&list, "!system:s", []string{fixturePrincipal}, time.Now())
	h := Header{Type: "projection", From: fixturePrincipal, Kind: "system"}
	for _, source := range []string{principalSubSource, ""} {
		sub := list.Subs[0]
		sub.Source = source
		subs := principalDeliverySubs([]Sub{sub}, []string{"!system:s"}, []string{fixturePrincipal})
		for _, sender := range []string{fixturePrincipal, "@copy:fixture.invalid"} {
			ev := principalFixtureEvent(sender)
			ev.Content, _ = json.Marshal(map[string]any{"msgtype": "m.text", "body": "forged origin", ContentKey: h})
			rel, _, _ := classify(ev, "!system:s", "@worker:s", "worker", config.State{}, subs)
			if (rel == relSub) != (sender == fixturePrincipal) {
				t.Fatalf("projection impersonation: %s %s", source, sender)
			}
		}
	}
	ordinary := Sub{SubSelector: SubSelector{Room: "!system:s", From: fixturePrincipal, Kinds: []string{"system"}}, CreatedAt: legacyCreatedAt}
	if !ordinary.matches(principalFixtureEvent("@projector:s"), "!system:s", h, true) {
		t.Fatal("ordinary projection selector regressed")
	}
	ordinary.AllEvents = true
	if !ordinary.matches(principalFixtureEvent("@projector:s"), "!system:s", h, true) {
		t.Fatal("unconfigured all-events projection selector regressed")
	}
}

// B1/N5: drive a synthetic sync batch through the real collector, the mailbox
// entry wire format, wait/watch selection and hook doorbell rendering. All
// rooms and events are memory-only; this does NOT prove fsync or a live wake.
func TestPrincipalBatchToWakePure(t *testing.T) {
	for _, label := range []string{"#ordinary", "#system", "#broadcast", "space", "direct"} {
		t.Run(label, func(t *testing.T) {
			ev := principalFixtureEvent(fixturePrincipal)
			neighbor := principalFixtureEvent("@copy:fixture.invalid")
			neighbor.EventID = "$neighbor"
			wire, _ := json.Marshal(map[string]any{"next_batch": "next", "rooms": map[string]any{"join": map[string]any{"!r:s": map[string]any{"timeline": map[string]any{"events": []matrix.Event{ev, neighbor}}}}}})
			var resp matrix.SyncResponse
			if err := json.Unmarshal(wire, &resp); err != nil {
				t.Fatal(err)
			}
			s := session{name: "worker", server: "s", creds: config.Credentials{UserID: "@worker:s"}, client: &matrix.Client{}}
			b, err := s.collectBatchWithPrincipals(context.Background(), resp, "previous", config.State{}, nil, nil, map[string]string{"!r:s": label}, []string{fixturePrincipal})
			if err != nil || len(b.entries) != 1 || b.entries[0].ID != ev.EventID {
				t.Fatalf("batch: %+v %v", b, err)
			}
			wire, _ = json.Marshal(b.entries)
			var entries []config.MailEntry
			if err := json.Unmarshal(wire, &entries); err != nil {
				t.Fatal(err)
			}
			f := deliveryFilter{predicates: deliveryPredicates{principals: []string{fixturePrincipal}, keywords: config.WaitKeywords{"NEVER"}}}
			if !f.shows(entries[0]) {
				t.Fatal("wait/watch suppressed principal mailbox entry")
			}
			items := itemsOf(entries, "s")
			d := doorbellOf(items, "s", nil)
			if d.total != 1 || d.types["sub"] != 1 || !strings.Contains(doorbellText(d), "1") || items[0].rec.Sender != fixturePrincipal {
				t.Fatalf("hook/inbox projection: %+v %+v", d, items)
			}
			if len(b.close) != 0 || len(b.learned) != 0 {
				t.Fatal("principal delivery closed a subscription or learned a forged thread")
			}
		})
	}
}

// B1: first-cursor selection retains principal history, invites and explicit
// history gaps; neighboring ordinary history is not replayed on startup.
func TestInitialPrincipalHistorySelection(t *testing.T) {
	principal, _ := json.Marshal(principalFixtureEvent(fixturePrincipal))
	other, _ := json.Marshal(principalFixtureEvent("@other:s"))
	entries := []config.MailEntry{{ID: "principal", Kind: config.MailEvent, Event: principal}, {ID: "ordinary", Kind: config.MailEvent, Event: other}, {ID: "invite", Kind: config.MailInvite}, {ID: "gap", Kind: config.MailGap}}
	got := initialPrincipalEntries(entries, []string{fixturePrincipal})
	if len(got) != 3 || got[0].ID != "principal" || got[1].ID != "invite" || got[2].ID != "gap" {
		t.Fatalf("initial entries: %+v", got)
	}
	if got := initialPrincipalEntries(entries, nil); len(got) != 2 || got[0].ID != "invite" {
		t.Fatalf("unconfigured history: %+v", got)
	}
}

// N3: cleanup protection is independent of membership snapshots. A configured
// legacy or dedicated row stays protected; unrelated and cleared rows do not.
func TestPrincipalCleanupIgnoresMembershipSnapshot(t *testing.T) {
	for _, source := range []string{"", principalSubSource} {
		sub := Sub{Source: source, Preset: "keeper:fixture", SubSelector: SubSelector{Room: "!later:s", From: fixturePrincipal, AllEvents: true}}
		if !protectedPrincipalSub(sub, []string{fixturePrincipal}) || protectedPrincipalSub(sub, nil) || protectedPrincipalSub(sub, []string{"@other:s"}) {
			t.Fatalf("protection: %+v", sub)
		}
	}
}

// N6: absent principal configuration adds no stdout fields or stderr notice in
// either format. A principal gap still emits its explicit field and failure.
func TestPrincipalCoverageOutputCompatibilityPure(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		for _, gap := range []bool{false, true} {
			var out, errout bytes.Buffer
			a := &App{Stdout: &out, Stderr: &errout}
			row := roomCoverage{Room: "!r:s", Label: "#room", Missing: []string{}, Status: "covered"}
			if gap {
				row.Status = "missing"
				row.MissingPrincipals = []string{fixturePrincipal}
			}
			err := a.printCoverage(coverageReport{OK: !gap, Rooms: []roomCoverage{row}}, asJSON, gap)
			if (err != nil) != gap || errout.Len() != 0 {
				t.Fatalf("output side effect: %v %q", err, errout.String())
			}
			if !gap && !asJSON && out.String() != "#room\tcovered\ncoverage: ok\n" {
				t.Fatal(out.String())
			}
			if asJSON {
				var decoded map[string]json.RawMessage
				if err := json.Unmarshal(out.Bytes(), &decoded); err != nil || len(decoded) != 2 {
					t.Fatalf("JSON: %s", out.String())
				}
				if strings.Contains(out.String(), "missing_principals") != gap {
					t.Fatal(out.String())
				}
			}
			if !gap && !reflect.DeepEqual(row.Missing, []string{}) {
				t.Fatal("renderer mutated report")
			}
		}
	}
}

// Initial history truncated without a page token must produce a gap rather
// than claim coverage. A complete one-event timeline is the valid neighbor.
// The collector reads only supplied events and cached labels in both cases.
func TestInitialPrincipalHistoryGap(t *testing.T) {
	for _, limited := range []bool{false, true} {
		wire, _ := json.Marshal(map[string]any{"next_batch": "next", "rooms": map[string]any{"join": map[string]any{"!r:s": map[string]any{"timeline": map[string]any{"events": []matrix.Event{principalFixtureEvent(fixturePrincipal)}, "limited": limited}}}}})
		var resp matrix.SyncResponse
		if err := json.Unmarshal(wire, &resp); err != nil {
			t.Fatal(err)
		}
		s := session{name: "worker", server: "s", creds: config.Credentials{UserID: "@worker:s"}, client: &matrix.Client{}}
		b, err := s.collectBatchWithPrincipals(context.Background(), resp, "", config.State{}, nil, nil, map[string]string{"!r:s": "#room"}, []string{fixturePrincipal})
		if err != nil {
			t.Fatal(err)
		}
		entries := initialPrincipalEntries(b.entries, []string{fixturePrincipal})
		if limited {
			if len(entries) != 2 || entries[0].Kind != config.MailGap || entries[0].From != "initial-history" || entries[0].To != "$plain" || entries[1].ID != "$plain" {
				t.Fatalf("unreported initial gap: %+v", entries)
			}
		} else if len(entries) != 1 || entries[0].ID != "$plain" {
			t.Fatalf("complete timeline: %+v", entries)
		}
	}
}
