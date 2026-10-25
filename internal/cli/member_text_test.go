package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Proves historical event principals survive JSON/text round trips byte-for-byte,
// while text cannot inject extra rows, columns or terminal control sequences.
// Synthetic dispatch retains the existing no-write and exact-request contract.
func TestRoomMembersHistoricalOutput(t *testing.T) {
	id := "@Guest space\t\n\r\x1b[31m\x7f界:Other.Invalid"
	for _, asJSON := range []bool{false, true} {
		t.Run(strconv.FormatBool(asJSON), func(t *testing.T) {
			f := newNamedFixture(t, true)
			seedMemberState(t, f)
			a := f.app()
			before := memberFiles(t, f.root)
			calls := 0
			var body map[string]any
			if err := json.Unmarshal([]byte(membershipReply("alpha", true)), &body); err != nil {
				t.Fatal(err)
			}
			body["chunk"].([]any)[1].(map[string]any)["state_key"] = id
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/rooms/!room:fixture/members" || r.URL.RawQuery != "" {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded))), ContentLength: int64(len(encoded)), Request: r}, nil
			})}
			args := []string{"room", "members", "!room:fixture", "--membership", "invite"}
			if asJSON {
				args = append(args, "--json")
			}
			if code := a.Run(context.Background(), args); code != 0 {
				t.Fatalf("exit%d: %s", code, f.err.String())
			}
			if calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
				t.Fatal("unexpected request count or local writes")
			}
			if asJSON {
				var got struct {
					Members []matrix.RoomMember `json:"members"`
				}
				if err := json.Unmarshal(f.out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Members) != 1 || got.Members[0].UserID != id || got.Members[0].Membership != "invite" {
					t.Fatalf("principal changed: %+v", got)
				}
			} else {
				lines := strings.Split(strings.TrimSuffix(f.out.String(), "\n"), "\n")
				if len(lines) != 2 {
					t.Fatalf("row injection: %q", f.out.String())
				}
				columns := strings.Split(lines[1], "\t")
				if len(columns) != 2 || columns[0] != "invite" {
					t.Fatalf("column injection: %q", lines[1])
				}
				got, err := strconv.Unquote(columns[1])
				if err != nil || got != id {
					t.Fatalf("lost principal: %q %v", got, err)
				}
				for _, b := range []byte(columns[1]) {
					if b < 32 || b > 126 {
						t.Fatalf("unsafe text byte %x", b)
					}
				}
			}
		})
	}
}
