package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Proves whether this exact CLI accepts valid full MXIDs and rejects malformed
// server suffixes without partial stdout or local writes. Synthetic authority
// only; no live roster or remote user is read or changed.
func TestMigrationReviewMemberIDGrammar(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		valid    bool
	}{
		{"dns-port", "@guest:other.invalid:8448", true},
		{"ipv6-port", "@guest:[2001:db8::1]:8448", true},
		{"historical-localpart", "@Guest/Old:other.invalid", true},
		{"server-path", "@guest:other.invalid/path", false},
		{"server-query", "@guest:other.invalid?x=y", false},
		{"missing-server-host", "@guest::8448", false},
		{"nonnumeric-server-port", "@guest:other.invalid:wrong", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			seedMemberState(t, f)
			a := f.app()
			before := memberFiles(t, f.root)
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/members") {
					return nil, fmt.Errorf("forbidden request")
				}
				body, _ := json.Marshal(map[string]any{"chunk": []any{
					map[string]any{"type": "m.room.member", "room_id": "!room:fixture", "state_key": "@alice:alpha.invalid", "content": map[string]any{"membership": "join"}},
					map[string]any{"type": "m.room.member", "room_id": "!room:fixture", "state_key": tc.id, "content": map[string]any{"membership": "invite"}},
				}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), ContentLength: int64(len(body)), Request: r}, nil
			})}
			code := a.Run(context.Background(), []string{"room", "members", "!room:fixture", "--json"})
			if calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
				t.Fatal("wrong request count or local writes")
			}
			if tc.valid {
				if code != 0 || !strings.Contains(f.out.String(), tc.id) {
					t.Fatalf("valid principal refused: exit%d stdout%s stderr%s", code, f.out.String(), f.err.String())
				}
			} else if code == 0 || f.out.Len() != 0 || !strings.Contains(f.err.String(), "malformed membership") {
				t.Errorf("malformed principal must fail with no roster: exit%d stdout%s stderr%s", code, f.out.String(), f.err.String())
			}
		})
	}
}
