package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Qualify the mutating startup operation separately from sync: retrying an
// ambiguous PUT is safe here only if rereading full-state account data preserves
// unrelated rows, deduplicates legacy migration and leaves delivery untouched.
func TestWaitRetryStartDelivery(t *testing.T) {
	for _, migration := range []bool{false, true} {
		name := "prune-only"
		if migration {
			name = "migration-and-prune"
		}
		for _, fault := range []string{"none", "get-refused", "put-rejected", "put-response-lost"} {
			t.Run(name+"/"+fault, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					qualifyStartDeliveryRetry(t, migration, fault)
				})
			})
		}
	}
}

func qualifyStartDeliveryRetry(t *testing.T, migration bool, fault string) {
	t.Helper()
	ctx := context.Background()
	a, p, _, _ := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
		t.Fatal("startup must not sync")
		return nil, nil
	})
	a.as = "alice"
	st, err := config.LoadState(p, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if migration {
		st.LegacySubscriptions = map[string][]string{
			"!existing:test.local": {"decision"},
			"!new:test.local":      {"request"},
		}
	}
	wantLegacy := st.LegacySubscriptions
	if err := config.SaveState(p, "alice", st); err != nil {
		t.Fatal(err)
	}
	mb, err := config.OpenMailbox(p, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mb.Append([]config.MailEntry{
		{ID: "$received", Kind: config.MailEvent, Room: "!existing:test.local"},
		{ID: "$pending", Kind: config.MailEvent, Room: "!existing:test.local"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mb.Receive([]string{"$received"}); err != nil {
		t.Fatal(err)
	}
	if err := mb.MarkShown([]string{"$pending"}); err != nil {
		t.Fatal(err)
	}
	mailBefore, err := os.ReadFile(p.MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	checkDelivery := func() config.State {
		t.Helper()
		cur, err := config.LoadState(p, "alice")
		if err != nil || cur.NextBatch != "seed" {
			t.Fatalf("startup cursor=%q, error=%v; want seed", cur.NextBatch, err)
		}
		mail, err := os.ReadFile(p.MailboxFile("alice"))
		if err != nil || !bytes.Equal(mail, mailBefore) {
			t.Fatalf("startup changed mailbox history/receipts: %v", err)
		}
		return cur
	}
	encode := func(v SubList) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	now := a.Now()
	kept := Sub{ID: "kept", SubSelector: SubSelector{Room: "!manual:test.local", AllEvents: true}, CreatedAt: legacyCreatedAt}
	existing := Sub{ID: "existing", SubSelector: SubSelector{Room: "!existing:test.local", Kinds: []string{"decision"}}, CreatedAt: legacyCreatedAt, Source: "legacy"}
	server := encode(SubList{V: 1, Subs: []Sub{kept, existing, {
		ID: "expired", SubSelector: SubSelector{Room: "!old:test.local", Until: now.Add(-time.Second).Format(time.RFC3339)}, CreatedAt: legacyCreatedAt,
	}}})
	var history, accepted []string
	gets, puts, effects := 0, 0, 0
	a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
		cur := checkDelivery()
		if !reflect.DeepEqual(cur.LegacySubscriptions, wantLegacy) {
			t.Fatalf("local migration committed before acknowledged PUT: %#v", cur.LegacySubscriptions)
		}
		if !strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
			t.Fatalf("unexpected startup request: %s %s", r.Method, r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			gets++
			if fault == "get-refused" && gets == 1 {
				history = append(history, "GET refused")
				return nil, waitRefused()
			}
			history = append(history, "GET state")
			return waitResponse(r, 200, server)
		case http.MethodPut:
			puts++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			var list SubList
			if err := json.Unmarshal(body, &list); err != nil {
				t.Fatal(err)
			}
			want := []Sub{kept, existing}
			if migration {
				if len(list.Subs) != 3 || !strings.HasPrefix(list.Subs[2].ID, "sub-") || len(list.Subs[2].ID) != 12 {
					t.Fatalf("expected one fresh migration row: %#v", list)
				}
				want = append(want, Sub{ID: list.Subs[2].ID, SubSelector: SubSelector{Room: "!new:test.local", Kinds: []string{"request"}}, CreatedAt: legacyCreatedAt, Source: "legacy"})
			}
			if list.V != 1 || !reflect.DeepEqual(list.Subs, want) {
				t.Fatalf("PUT full list=%#v, want %#v", list, want)
			}
			if fault == "put-rejected" && puts == 1 {
				history = append(history, "PUT rejected")
				return waitResponse(r, 503, `{"errcode":"M_UNAVAILABLE"}`)
			}
			if server != string(body) {
				effects++
			}
			server = string(body)
			accepted = append(accepted, server)
			if fault == "put-response-lost" && puts == 1 {
				history = append(history, "PUT accepted response-lost")
				return nil, io.ErrUnexpectedEOF
			}
			history = append(history, "PUT acknowledged")
			return waitResponse(r, 200, `{}`)
		default:
			t.Fatalf("unexpected method %s", r.Method)
			return nil, nil
		}
	})
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	var attempts []time.Time
	got, err := waitRetry(ctx, s, now.Add(time.Minute), func() (config.State, error) {
		attempts = append(attempts, a.Now())
		cur, _, err := s.startDelivery(ctx, a.Now())
		return cur, err
	})
	if err != nil || got.NextBatch != "seed" || len(got.LegacySubscriptions) != 0 {
		t.Fatalf("startup result cursor=%q legacy=%v error=%v", got.NextBatch, got.LegacySubscriptions, err)
	}
	wantHistory := []string{"GET state", "PUT acknowledged"}
	switch fault {
	case "get-refused":
		wantHistory = []string{"GET refused", "GET state", "PUT acknowledged"}
	case "put-rejected":
		wantHistory = []string{"GET state", "PUT rejected", "GET state", "PUT acknowledged"}
	case "put-response-lost":
		wantHistory = []string{"GET state", "PUT accepted response-lost", "GET state"}
		if migration {
			wantHistory = append(wantHistory, "PUT acknowledged")
		}
	}
	if !reflect.DeepEqual(history, wantHistory) || effects != 1 {
		t.Fatalf("history=%v, effects=%d; want %v, 1", history, effects, wantHistory)
	}
	wantAttempts := 2
	if fault == "none" {
		wantAttempts = 1
	}
	if len(attempts) != wantAttempts || wantAttempts == 2 && attempts[1].Sub(attempts[0]) != 5*time.Second {
		t.Fatalf("startup attempt times=%v", attempts)
	}
	if len(accepted) == 2 && accepted[0] != accepted[1] {
		t.Fatal("ambiguous migration retry changed accepted replacement bytes")
	}
	if cur := checkDelivery(); len(cur.LegacySubscriptions) != 0 {
		t.Fatal("acknowledged startup did not persist migration")
	}
	// A successful restart is a read-only positive control: no further effect,
	// replacement, cursor advance or mailbox receipt is necessary.
	wantLegacy = nil
	before := len(history)
	if _, _, err := s.startDelivery(ctx, a.Now()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(history[before:], []string{"GET state"}) || effects != 1 {
		t.Fatalf("stable restart history=%v effects=%d", history[before:], effects)
	}
	checkDelivery()
	t.Logf("history=%v; attempts=%d; accepted PUTs=%d; effects=%d; cursor=seed; mailbox bytes unchanged", history, len(attempts), len(accepted), effects)
}
