package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type stub struct {
	d      Decision
	err    error
	calls  int
	mutate bool
}

func (s *stub) Inbound(_ context.Context, in Inbound) (Decision, error) {
	s.calls++
	if s.mutate && len(in.Body) > 0 {
		in.Body[0] = 'X'
	}
	return s.d, s.err
}
func (s *stub) Outbound(context.Context, Outbound) (Decision, error) { return s.d, s.err }
func (*stub) Close() error                                           { return nil }
func testStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: filepath.Join(t.TempDir(), "admission", "alice")}
}
func testItem(id string) Item {
	return Item{Inbound: Inbound{RoomID: "!room:test", EventID: id, Sender: "@private:test", Kind: "m.room.message", Timestamp: 42, Body: []byte("PRIVATE_BODY")}, Payload: json.RawMessage(`{"private":"metadata"}`)}
}
func process(t *testing.T, s Store, c Checker, now time.Time, items []Item) []Item {
	t.Helper()
	var live []Item
	if err := s.Process(context.Background(), c, now, items, func(v []Item) error { live = v; return nil }); err != nil {
		t.Fatal(err)
	}
	return live
}
func counts(t *testing.T, s Store) map[string]uint64 {
	t.Helper()
	m, err := s.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Proves retryable items stay private, expire from first hold time, and definite/policy errors count closed codes.
func TestHeldTransitions(t *testing.T) {
	for _, mode := range []string{"live", "age", "definite", "error", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			c := &stub{d: Decision{Verdict: Refused, Retryable: true, Reason: "dependency_unavailable"}}
			item := testItem("$a")
			if len(process(t, s, c, now, []Item{item})) != 0 {
				t.Fatal("held became live")
			}
			later := now.Add(time.Hour)
			reason := ""
			switch mode {
			case "live":
				c.d = Decision{Verdict: Live, Reason: "allowed"}
			case "age":
				later = now.Add(HeldAgeLimit)
				reason = "held_age"
			case "definite":
				c.d = Decision{Verdict: Refused, Reason: "denied"}
				reason = "denied"
			case "error":
				c.err = errors.New("PRIVATE_BODY @private:test")
				reason = "policy_error"
			case "invalid":
				c.d.Reason = "PRIVATE_BODY"
				reason = "policy_invalid"
			}
			got := process(t, s, c, later, []Item{item}) // same event in the next batch must be checked only once
			if mode == "age" && c.calls != 1 {
				t.Fatal("expired item reached checker again")
			}
			if mode == "live" {
				if len(got) != 1 || got[0].Inbound.EventID != "$a" {
					t.Fatal("release mismatch")
				}
			} else if len(got) != 0 || counts(t, s)[reason] != 1 {
				t.Fatal("refusal/age count mismatch")
			}
			r, err := s.root()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			st, err := load(r)
			if err != nil || len(st.Held) != 0 {
				t.Fatal("resolved item retained")
			}
		})
	}
}

// Proves the incoming 257th item is dropped while the existing 256 remain; age drops only the expired item.
func TestHeldBounds(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	c := &stub{d: Decision{Verdict: Refused, Retryable: true, Reason: "dependency_unavailable"}}
	var items []Item
	for i := 0; i < HeldCountLimit+1; i++ {
		items = append(items, testItem(fmt.Sprintf("$%d", i)))
	}
	process(t, s, c, now, items)
	r, _ := s.root()
	st, err := load(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Held) != HeldCountLimit || st.Held[len(st.Held)-1].Item.Inbound.EventID != "$255" || counts(t, s)["held_count"] != 1 {
		t.Fatal("wrong item evicted")
	}
	s = testStore(t)
	process(t, s, c, now, []Item{testItem("$old")})
	process(t, s, c, now.Add(time.Hour), []Item{testItem("$young")})
	process(t, s, c, now.Add(HeldAgeLimit), nil)
	r, _ = s.root()
	st, err = load(r)
	r.Close()
	if err != nil || len(st.Held) != 1 || st.Held[0].Item.Inbound.EventID != "$young" || counts(t, s)["held_age"] != 1 {
		t.Fatal("wrong age eviction")
	}
}

// Proves v1 JSON/base64 format, private modes and a full spool refusing without truncation.
func TestExchangeFormatAndCapacity(t *testing.T) {
	s := testStore(t)
	c := &stub{d: Decision{Verdict: Exchange, Reason: "exchange"}, mutate: true}
	now := time.Now()
	item := testItem("$one")
	if len(process(t, s, c, now, []Item{item})) != 0 {
		t.Fatal("exchange delivered")
	}
	path := filepath.Join(s.Dir, "exchange.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"version":1,"event_id":"$one","room":"!room:test","sender":"@private:test","timestamp":42,"kind":"m.room.message","body":"UFJJVkFURV9CT0RZ","reason":"exchange"}` + "\n"
	if string(raw) != expected {
		t.Fatalf("format mismatch: %s", raw)
	}
	for _, p := range []string{s.Dir, path, filepath.Join(s.Dir, "held.json")} {
		info, err := os.Stat(p)
		want := os.FileMode(0600)
		if p == s.Dir {
			want = 0700
		}
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("mode mismatch: %v", err)
		}
	}
	// Fill one valid v1 line right up to the cap; the next line cannot fit.
	padding := strings.Repeat(" ", int(ExchangeSizeLimit)-len(raw))
	full := append(bytes.TrimSuffix(raw, []byte("\n")), []byte(padding)...)
	full = append(full, '\n')
	if int64(len(full)) != ExchangeSizeLimit {
		t.Fatal("bad cap fixture")
	}
	if err := os.WriteFile(path, full, 0600); err != nil {
		t.Fatal(err)
	}
	process(t, s, c, now, []Item{testItem("$two")})
	after, _ := os.ReadFile(path)
	if !bytes.Equal(full, after) || counts(t, s)["exchange_capacity"] != 1 {
		t.Fatal("cap changed spool or failed to count")
	}
}

// Proves failed mailbox persistence retains held data and retry emits it again for mailbox dedup.
func TestHeldDeliveryFailure(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	c := &stub{d: Decision{Verdict: Refused, Retryable: true, Reason: "dependency_unavailable"}}
	process(t, s, c, now, []Item{testItem("$a")})
	c.d = Decision{Verdict: Live, Reason: "allowed"}
	err := s.Process(context.Background(), c, now, nil, func([]Item) error { return errors.New("mailbox failed") })
	if err == nil {
		t.Fatal("lost mailbox failure")
	}
	if got := process(t, s, c, now, nil); len(got) != 1 {
		t.Fatal("failure lost held data")
	}
}

// Proves permissive files, links, corrupt state and a cancelled cycle fail closed without delivering.
func TestStorageRefusals(t *testing.T) {
	for _, mode := range []string{"mode", "link", "corrupt", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			if err := PrivateDir(s.Dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Dir, "held.json")
			switch mode {
			case "mode":
				if err := os.WriteFile(path, []byte(`{}`), 0644); err != nil {
					t.Fatal(err)
				}
			case "link":
				if err := os.Symlink("outside", path); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(path, []byte("PRIVATE_BODY"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			called := false
			err := s.Process(ctx, &stub{d: Decision{Verdict: Live, Reason: "allowed"}}, time.Now(), []Item{testItem("$a")}, func([]Item) error { called = true; return nil })
			if err == nil || called || strings.Contains(err.Error(), "PRIVATE_BODY") {
				t.Fatal("storage refusal violated")
			}
		})
	}
}
