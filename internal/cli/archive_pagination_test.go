package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Proves both Matrix continuation-token formats preserve later events across
// empty pages, reject cycles/read failures without returning a partial result,
// and distinguish terminal history at the page bound from unresolved history.
func TestArchivePagination(t *testing.T) {
	for _, tokenField := range []string{"end", "next_batch"} {
		t.Run(tokenField, func(t *testing.T) {
			page := func(next string, ids ...string) matrix.Page {
				p := matrix.Page{}
				if tokenField == "end" {
					p.End = next
				} else {
					p.NextBatch = next
				}
				for _, id := range ids {
					p.Chunk = append(p.Chunk, matrix.Event{EventID: id})
				}
				return p
			}
			for _, tc := range []struct {
				name      string
				pages     map[string]matrix.Page
				failAt    string
				bounded   bool
				terminal  bool
				wantIDs   []string
				wantError string
				wantReads int
			}{
				{name: "empty then older history", pages: map[string]matrix.Page{"": page("a"), "a": page("b", "$new"), "b": page("", "$old")}, wantIDs: []string{"$old", "$new"}, wantReads: 3},
				{name: "terminal empty", pages: map[string]matrix.Page{"": page("a"), "a": page("")}, wantReads: 2},
				{name: "stalled token", pages: map[string]matrix.Page{"": page("a", "$new"), "a": page("a")}, wantError: "repeated pagination token", wantReads: 2},
				{name: "cyclic token", pages: map[string]matrix.Page{"": page("a", "$new"), "a": page("b"), "b": page("a")}, wantError: "repeated pagination token", wantReads: 3},
				{name: "later read failure", pages: map[string]matrix.Page{"": page("a", "$new")}, failAt: "a", wantError: "fixture read failure", wantReads: 2},
				{name: "unresolved bound", bounded: true, wantError: "10000 pages", wantReads: 10000},
				{name: "terminal at bound", bounded: true, terminal: true, wantIDs: []string{"$old"}, wantReads: 10000},
			} {
				t.Run(tc.name, func(t *testing.T) {
					reads := 0
					got, err := pageAllEvents("fixture", func(from string) (matrix.Page, error) {
						reads++
						if reads > 10000 {
							return matrix.Page{}, errors.New("test guard: excessive pagination")
						}
						if tc.bounded {
							if tc.terminal && reads == 10000 {
								return page("", "$old"), nil
							}
							return page(fmt.Sprint(reads)), nil
						}
						if tc.failAt != "" && from == tc.failAt {
							return matrix.Page{}, errors.New("fixture read failure")
						}
						p, ok := tc.pages[from]
						if !ok {
							return matrix.Page{}, fmt.Errorf("unexpected token %q", from)
						}
						return p, nil
					})
					if tc.wantError != "" {
						if err == nil || !strings.Contains(err.Error(), tc.wantError) {
							t.Fatalf("error = %v, want %q", err, tc.wantError)
						}
						if got != nil {
							t.Fatalf("returned partial events on failure: %v", got)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						var ids []string
						for _, ev := range got {
							ids = append(ids, ev.EventID)
						}
						if !reflect.DeepEqual(ids, tc.wantIDs) {
							t.Fatalf("events = %v, want %v", ids, tc.wantIDs)
						}
					}
					if reads != tc.wantReads {
						t.Fatalf("reads = %d, want %d", reads, tc.wantReads)
					}
				})
			}
		})
	}
}

type archiveRoundTripFunc func(*http.Request) (*http.Response, error)

func (f archiveRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Proves a failed paginated room scan neither creates an archive nor rewrites
// an existing one; the same fixture with terminal pagination retains the older
// room message. These are isolated HTTP/file fixtures, not a live export.
func TestArchivePaginationPreservesFiles(t *testing.T) {
	for _, failure := range []string{"cycle", "read error"} {
		t.Run(failure, func(t *testing.T) {
			e, dir, _ := archiveWorld(t)
			id := e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "older room message"})
			e.hs.mu.Lock()
			var history []matrix.Event
			found := false
			for i := len(e.hs.events) - 1; i >= 0; i-- {
				ev := e.hs.events[i]
				if ev.RoomID == "!projRoomID" {
					history = append(history, ev)
					found = found || ev.EventID == id
				}
			}
			e.hs.mu.Unlock()
			if !found {
				t.Fatal("fixture event missing")
			}
			refuse := false
			e.configure = func(a *App) {
				base := a.HTTP.Transport
				if base == nil {
					base = http.DefaultTransport
				}
				a.HTTP = &http.Client{Transport: archiveRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(r.URL.Path, "/messages") {
						return base.RoundTrip(r)
					}
					w := httptest.NewRecorder()
					p := matrix.Page{Chunk: []matrix.Event{}}
					if r.URL.Query().Get("from") == "" {
						p.End = "older"
					} else if refuse {
						if failure == "read error" {
							writeErr(w, http.StatusServiceUnavailable, "M_UNKNOWN", "fixture read failure")
							return w.Result(), nil
						}
						p.End = "older"
					} else {
						p.Chunk = history
					}
					writeJSON(w, http.StatusOK, p)
					return w.Result(), nil
				})}
			}
			e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
			roomPath := filepath.Join(dir, "proj", RoomFileName)
			if !strings.Contains(readFileStr(t, roomPath), "older room message") {
				t.Fatal("older message omitted after an empty continuation page")
			}
			names := []string{"alpha.md", "beta.md", RoomFileName, IndexFileName}
			before := map[string]string{}
			for _, name := range names {
				before[name] = readFileStr(t, filepath.Join(dir, "proj", name))
			}
			refuse = true
			wantError := "repeated pagination token"
			if failure == "read error" {
				wantError = "HTTP 503"
			}
			e.as("alice", "archive", "proj", "--dir", dir, "--json-index").failsWith(t, ExitError, wantError)
			for _, name := range names {
				if got := readFileStr(t, filepath.Join(dir, "proj", name)); got != before[name] {
					t.Fatalf("%s rewritten after failed collection", name)
				}
			}
			if got := dirEntries(t, filepath.Join(dir, "proj")); !reflect.DeepEqual(got, []string{RoomFileName, "alpha.md", "beta.md", IndexFileName}) {
				t.Fatalf("archive files changed after refusal: %v", got)
			}
			fresh := filepath.Join(t.TempDir(), "fresh")
			e.as("alice", "archive", "proj", "--dir", fresh).failsWith(t, ExitError, wantError)
			mustNotExist(t, fresh)
		})
	}
}
