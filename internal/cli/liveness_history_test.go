package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Proves a 10,050-old-message room backfills without restarting its history,
// stays unknown until a fresh head pass, then costs one tail request per joined room per poll.
// A newer-timestamp request behind an older event is retained; a reply arriving
// during backfill is read before a known unanswered result can be published.
func TestLivenessHistoryIncrementalBoundAndConcurrentReply(t *testing.T) {
	e, room := world(t)
	now := time.Now().Add(time.Minute)
	a := eApp(e)
	a.Now = func() time.Time { return now }
	s, err := a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	pages := map[string]fakeMessagePage{}
	old := matrix.Event{Type: "m.room.message", Sender: "@bob:test.local", OriginServerTS: now.Add(-2 * time.Hour).UnixMilli(), Content: json.RawMessage(`{"body":"old"}`)}
	for n := 0; n < 101; n++ {
		token := ""
		if n > 0 {
			token = fmt.Sprintf("p%d", n)
		}
		page := matrix.Page{}
		count := 100
		if n == 100 {
			count = 50
		}
		for i := 0; i < count; i++ {
			ev := old
			ev.EventID = fmt.Sprintf("$old-%d-%d", n, i)
			page.Chunk = append(page.Chunk, ev)
		}
		if n < 100 {
			page.End = fmt.Sprintf("p%d", n+1)
		}
		pages[token] = fakeMessagePage{page: page}
	}
	q := matrix.Event{EventID: "$q", Type: "m.room.message", Sender: "@bob:test.local", OriginServerTS: now.Add(-time.Minute).UnixMilli(), Content: livenessHistoryContent("q", map[string]any{"ai.relux.a2a": Header{V: 1, Type: "message", Kind: "question", To: []string{"alice"}}})}
	last := pages["p100"]
	last.page.Chunk = append(last.page.Chunk, q)
	pages["p100"] = last
	e.hs.mu.Lock()
	e.hs.messagePages = map[string]map[string]fakeMessagePage{room: pages}
	e.hs.mu.Unlock()
	p, _ := newReplyPolicy(nil, nil)
	h := &livenessHistory{}
	rows := []whoRow{{Name: "alice"}}
	since := now.Add(-time.Hour)
	for n := 0; n < 101; n++ {
		before := e.hs.CallCount("GET /_matrix/client/v3/rooms/{room}/messages")
		if err := h.step(context.Background(), a, s, rows, since, p, 1); err != nil {
			t.Fatal(err)
		}
		if e.hs.CallCount("GET /_matrix/client/v3/rooms/{room}/messages")-before != 2 {
			t.Fatal("exceeded explicit page budget")
		}
		if !strings.HasPrefix(rows[0].Unanswered, "unknown") {
			t.Fatalf("incomplete history claimed a verdict at page %d: %+v", n, rows)
		}
	}
	// The request really was retained beyond an old timestamp; it has not yet
	// been exposed as unanswered, because concurrent replies still need a tail.
	if h.rooms[room].events["$q"].event.EventID != "$q" {
		t.Fatal("old timestamp truncated newer request")
	}
	reply := matrix.Event{EventID: "$r", Type: "m.room.message", Sender: "@alice:test.local", OriginServerTS: now.UnixMilli(), Content: livenessHistoryContent("r", map[string]any{"ai.relux.a2a": Header{V: 1, Type: "message", Kind: "answer", Re: "$q"}})}
	e.hs.mu.Lock()
	e.hs.messagePages[room][""] = fakeMessagePage{page: matrix.Page{Chunk: []matrix.Event{reply, pages[""].page.Chunk[0]}}}
	e.hs.mu.Unlock()
	for n := 0; n < 3; n++ {
		before := e.hs.CallCount("GET /_matrix/client/v3/rooms/{room}/messages")
		if err := h.step(context.Background(), a, s, rows, since, p, 1); err != nil {
			t.Fatal(err)
		}
		if rows[0].Unanswered != "-" || e.hs.CallCount("GET /_matrix/client/v3/rooms/{room}/messages")-before != 2 {
			t.Fatalf("tail replayed history or lost concurrent answer: %+v", rows)
		}
	}
}

// Proves empty pages continue, cycles/future events never yield an absence,
// and a lost stream checkpoint is explicit rather than silently resetting.
func TestLivenessHistoryUnknownControls(t *testing.T) {
	e, room := world(t)
	a := eApp(e)
	s, err := a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := newReplyPolicy(nil, nil)
	h := &livenessHistory{}
	rows := []whoRow{{Name: "alice"}}
	now := time.Now()
	since := now.Add(-time.Hour)
	ev := matrix.Event{EventID: "$a", Type: "m.room.message", OriginServerTS: now.UnixMilli(), Content: json.RawMessage(`{"body":"a"}`)}
	e.hs.mu.Lock()
	e.hs.messagePages = map[string]map[string]fakeMessagePage{room: {"": {page: matrix.Page{End: "next"}}, "next": {page: matrix.Page{Chunk: []matrix.Event{ev}}}}}
	e.hs.mu.Unlock()
	if err := h.step(context.Background(), a, s, rows, since, p, 2); err != nil || rows[0].Unanswered != "-" {
		t.Fatalf("empty filtered page blocked valid continuation: %v %+v", err, rows)
	}
	ev.EventID = "$other"
	e.hs.mu.Lock()
	e.hs.messagePages[room][""] = fakeMessagePage{page: matrix.Page{Chunk: []matrix.Event{ev}}}
	e.hs.mu.Unlock()
	if err := h.step(context.Background(), a, s, rows, since, p, 1); err == nil || !strings.Contains(err.Error(), "checkpoint missing") || !strings.HasPrefix(rows[0].Unanswered, "unknown") {
		t.Fatalf("lost anchor claimed absence: %v %+v", err, rows)
	}
	h = &livenessHistory{}
	e.hs.mu.Lock()
	e.hs.messagePages[room][""] = fakeMessagePage{page: matrix.Page{End: "cycle"}}
	e.hs.messagePages[room]["cycle"] = fakeMessagePage{page: matrix.Page{End: "cycle"}}
	e.hs.mu.Unlock()
	if err := h.step(context.Background(), a, s, rows, since, p, 2); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle undetected: %v", err)
	}
	h = &livenessHistory{}
	ev.OriginServerTS = now.Add(time.Hour).UnixMilli()
	e.hs.mu.Lock()
	e.hs.messagePages[room][""] = fakeMessagePage{page: matrix.Page{Chunk: []matrix.Event{ev}}}
	e.hs.mu.Unlock()
	if err := h.step(context.Background(), a, s, rows, since, p, 1); err == nil || !strings.Contains(err.Error(), "future") || !strings.HasPrefix(rows[0].Unanswered, "unknown") {
		t.Fatalf("future event claimed verdict: %v %+v", err, rows)
	}
}

func livenessHistoryContent(body string, extra map[string]any) json.RawMessage {
	raw, _ := json.Marshal(msg(body, extra))
	return raw
}

// Proves server-authorized redactions arriving after the cached snapshot
// remove both request and answer evidence (old/new room-version encodings).
func TestLivenessHistoryRedactionsInvalidateCache(t *testing.T) {
	for _, redactRequest := range []bool{true, false} {
		t.Run(fmt.Sprint(redactRequest), func(t *testing.T) {
			e, room := world(t)
			now := time.Now()
			a := eApp(e)
			s, err := a.sessionAs("alice")
			if err != nil {
				t.Fatal(err)
			}
			q := matrix.Event{EventID: "$q", Type: "m.room.message", Sender: "@bob:test.local", OriginServerTS: now.Add(-time.Minute).UnixMilli(), Content: livenessHistoryContent("q", map[string]any{"ai.relux.a2a": Header{V: 1, Type: "message", Kind: "question", To: []string{"alice"}}})}
			r := matrix.Event{EventID: "$r", Type: "m.room.message", Sender: "@alice:test.local", OriginServerTS: now.Add(-time.Second).UnixMilli(), Content: livenessHistoryContent("r", map[string]any{"ai.relux.a2a": Header{V: 1, Type: "message", Kind: "answer", Re: "$q"}})}
			initial := []matrix.Event{r, q}
			if redactRequest {
				initial = []matrix.Event{q}
			}
			e.hs.mu.Lock()
			e.hs.messagePages = map[string]map[string]fakeMessagePage{room: {"": {page: matrix.Page{Chunk: initial}}}}
			e.hs.mu.Unlock()
			h := &livenessHistory{}
			p, _ := newReplyPolicy(nil, nil)
			rows := []whoRow{{Name: "alice"}}
			since := now.Add(-time.Hour)
			if err := h.step(context.Background(), a, s, rows, since, p, 1); err != nil {
				t.Fatal(err)
			}
			if (rows[0].Unanswered == "-") == redactRequest {
				t.Fatal("initial positive control mismatch")
			}
			redaction := matrix.Event{EventID: "$redaction", Type: "m.room.redaction", OriginServerTS: now.UnixMilli(), Content: json.RawMessage(`{"redacts":"$r"}`)}
			if redactRequest {
				redaction.Redacts = "$q"
				redaction.Content = json.RawMessage(`{}`)
			}
			e.hs.mu.Lock()
			e.hs.messagePages[room][""] = fakeMessagePage{page: matrix.Page{Chunk: append([]matrix.Event{redaction}, initial...)}}
			e.hs.mu.Unlock()
			if err := h.step(context.Background(), a, s, rows, since, p, 1); err != nil {
				t.Fatal(err)
			}
			if (rows[0].Unanswered == "-") != redactRequest {
				t.Fatalf("redaction did not invalidate cache: %+v", rows)
			}
		})
	}
}
