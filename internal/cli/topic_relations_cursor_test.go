package cli

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// Use the real read collector in both output modes and both root-age cases.
// Even after a valid prefix, malformed cursors must fail atomically and never
// query the repaired token, where a server could return an exhausted listing.
func TestTopicRelationsCursorTransport(t *testing.T) {
	for _, old := range []bool{false, true} {
		for _, asJSON := range []bool{false, true} {
			for _, tc := range []struct {
				name, raw, want string
				valid           bool
			}{
				{"high-surrogate", `"page-\ud800"`, "page-�", false},
				{"low-surrogate", `"page-\udc00"`, "page-�", false},
				{"invalid-utf8", "\"page-\xff\"", "page-�", false},
				{"opaque", `"opaque%2F/\u0061:+?&="`, "opaque%2F/a:+?&=", true},
				{"literal-replacement", `"page-�"`, "page-�", true},
				{"surrogate-pair", `"page-\ud83d\ude00"`, "page-😀", true},
			} {
				t.Run(fmt.Sprintf("old=%v/json=%v/%s", old, asJSON, tc.name), func(t *testing.T) {
					f := newTopicRelationsFixture(t, old)
					f.pages[""] = relationsResponse{relationsJSON(nil, "prefix"), 200}
					// A valid reply already collected must not leak on later failure.
					prefix := relationsJSON(nil, "")
					prefix = strings.Replace(prefix, `"chunk":[]`, `"chunk":[{"event_id":"$reply","type":"m.room.message","content":{"body":"legacy\ud800","body":"last","m.relates_to":{"rel_type":"m.thread","event_id":"$archive-root"}}}]`, 1)
					f.pages["prefix"] = relationsResponse{strings.TrimSuffix(prefix, "}") + `,"next_batch":` + tc.raw + `}`, 200}
					f.pages[tc.want] = relationsResponse{`{"chunk":[]}`, 200}
					r := f.read(relationsRoot, 10, asJSON)
					f.mu.Lock()
					defer f.mu.Unlock()
					var cursors []string
					for _, request := range f.requests {
						u, err := url.Parse(strings.TrimPrefix(request, "GET "))
						if err != nil {
							t.Fatal(err)
						}
						if strings.Contains(u.Path, "/relations/") {
							cursors = append(cursors, u.Query().Get("from"))
						}
					}
					t.Logf("exit=%d relation-cursors=%q stdout-bytes=%d", r.code, cursors, len(r.stdout))
					if tc.valid {
						if r.code != ExitOK || !strings.Contains(r.stdout, "$reply") || len(cursors) != 3 || cursors[2] != tc.want {
							t.Fatalf("valid cursor or peer body refused: %+v", r)
						}
					} else if r.code == ExitOK || r.stdout != "" || !strings.Contains(r.stderr, "incomplete topic replies") || len(cursors) != 2 {
						t.Fatalf("malformed cursor followed or partial output accepted: %+v", r)
					}
				})
			}
		}
	}
}
