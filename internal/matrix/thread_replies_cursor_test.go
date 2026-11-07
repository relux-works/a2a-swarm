package matrix

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestThreadRepliesCursorScalars(t *testing.T) {
	for _, field := range []string{"next_batch", "end"} {
		for _, tc := range []struct {
			name, raw, want string
			valid           bool
		}{
			{"opaque", `"opaque%2F/\u0061:+?&="`, "opaque%2F/a:+?&=", true},
			{"literal-replacement", `"page-�"`, "page-�", true},
			{"escaped-replacement", `"page-\ufffd"`, "page-�", true},
			{"surrogate-pair", `"page-\ud83d\ude00"`, "page-😀", true},
			{"scalar-bounds", `"\ud800\udc00\udbff\udfff"`, "\U00010000\U0010ffff", true},
			{"escaped-backslash", `"page-\\ud800"`, `page-\ud800`, true},
			{"whitespace", `" "`, " ", true},
			{"high-surrogate", `"page-\ud800"`, "", false},
			{"low-surrogate", `"page-\udc00"`, "", false},
			{"reversed-pair", `"\udc00\ud800"`, "", false},
			{"high-before-scalar", `"\ud800\u0061"`, "", false},
			{"pair-then-lone", `"\ud83d\ude00\udfff"`, "", false},
			{"invalid-utf8", "\"page-\xff\"", "", false},
			{"encoded-surrogate", "\"\xed\xa0\x80\"", "", false},
			{"empty", `""`, "", false},
			{"null", `null`, "", false},
			{"number", `123`, "", false},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				p, err := decodeRelationsPage([]byte(fmt.Sprintf(`{"chunk":[],%q:%s}`, field, tc.raw)), "!room:fixture", "$root")
				got := p.NextBatch
				if field == "end" {
					got = p.End
				}
				if (err == nil) != tc.valid || got != tc.want || (!tc.valid && p.Chunk != nil) {
					t.Fatalf("valid=%v got=%q error=%v", tc.valid, got, err)
				}
			})
		}
	}
}

// Exercise the actual transport boundary. A malformed first page must never
// supply a repaired selector to the exhausted second page.
func TestThreadRepliesCursorTransport(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
		valid           bool
	}{
		{"high-surrogate", `"page-\ud800"`, "", false},
		{"low-surrogate", `"page-\udc00"`, "", false},
		{"invalid-utf8", "\"page-\xff\"", "", false},
		{"opaque", `"opaque%2F/\u0061:+?&="`, "opaque%2F/a:+?&=", true},
		{"literal-replacement", `"page-�"`, "page-�", true},
		{"surrogate-pair", `"page-\ud83d\ude00"`, "page-😀", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []string
			c := New("http://offline.invalid", "")
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r.URL.Query().Get("from"))
				body := `{"chunk":[]}`
				if len(requests) == 1 {
					body = fmt.Sprintf(`{"chunk":[],"next_batch":%s}`, tc.raw)
				}
				return &http.Response{StatusCode: 200, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			p, firstErr := c.ThreadReplies(context.Background(), "!room:fixture", "$root", "", "b", 100)
			var secondErr error
			if firstErr == nil {
				p, secondErr = c.ThreadReplies(context.Background(), "!room:fixture", "$root", p.NextBatch, "b", 100)
			}
			t.Logf("valid=%v requests=%q first-error=%v second-error=%v", tc.valid, requests, firstErr, secondErr)
			if tc.valid {
				if firstErr != nil || secondErr != nil || len(requests) != 2 || requests[0] != "" || requests[1] != tc.want || p.NextBatch != "" {
					t.Fatal("valid opaque cursor did not reach exact next page and exhaust")
				}
			} else if firstErr == nil || len(requests) != 1 || p.Chunk != nil || p.NextBatch != "" {
				t.Fatal("malformed cursor reached repaired second request or accepted exhaustion")
			}
		})
	}
}
