package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type metaMembershipBody struct {
	io.Reader
	n, closes int
	cancel    context.CancelFunc
}

func (b *metaMembershipBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	b.n += n
	return n, e
}
func (b *metaMembershipBody) Close() error {
	b.closes++
	if b.cancel != nil {
		b.cancel()
	}
	return nil
}

// Proves supplementary complete-entity, byte-cap and Unicode/unknown-field
// rejection controls on a synthetic transport. No live Matrix request occurs.
func TestMetaMembersTransportAndJSON(t *testing.T) {
	valid := `{"chunk":[` + membersJoined + `]}`
	for _, tc := range []struct {
		name, body  string
		want        error
		cancelClose bool
	}{
		{"valid", valid, nil, false},
		{"exact-cap", valid + strings.Repeat(" ", membersResponseLimit-len(valid)), nil, false},
		{"over-cap", valid + strings.Repeat(" ", membersResponseLimit+1-len(valid)), ErrMembersIncomplete, false},
		{"huge-tail", valid + strings.Repeat(" ", 2*membersResponseLimit), ErrMembersIncomplete, false},
		{"invalid-utf8-unknown", strings.TrimSuffix(valid, "}") + `,"ignored":"` + string([]byte{0xff}) + `"}`, ErrMembersMalformed, false},
		{"lone-surrogate-unknown", strings.TrimSuffix(valid, "}") + `,"ignored":"\ud800"}`, ErrMembersMalformed, false},
		{"decoded-duplicate-unknown", strings.TrimSuffix(valid, "}") + `,"ignored":{"a":1,"\u0061":2}}`, ErrMembersMalformed, false},
		{"unknown-harmless", strings.TrimSuffix(valid, "}") + `,"ignored":{"value":"private-discarded","n":1e+10}}`, nil, false},
		{"cancel-at-close", valid, context.Canceled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := &metaMembershipBody{Reader: strings.NewReader(tc.body)}
			if tc.cancelClose {
				b.cancel = cancel
			}
			c := New("https://matrix.invalid", "synthetic")
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: b, ContentLength: -1, Request: r}, nil
			})}
			got, err := c.CurrentRoomMembers(ctx, "!r:s", "@me:s")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if b.closes != 1 || b.n > membersResponseLimit+1 {
				t.Fatalf("read=%d closes=%d", b.n, b.closes)
			}
			if err != nil && got != nil {
				t.Fatal("partial roster escaped")
			}
			if err == nil && (len(got) != 1 || got[0].UserID != "@me:s") {
				t.Fatalf("projection: %+v", got)
			}
		})
	}
}

// Matrix v1.16 appendices Historical User IDs requires these localparts in
// events to be accepted. This witness proves valid neighboring current IDs
// pass and exposes whole-response rejection of valid historical members.
func TestMetaMembersHistoricalLocalparts(t *testing.T) {
	for _, id := range []string{"@modern:example.org", "@a/b+c:example.org", "@ALICE:example.org", "@alice smith:example.org", "@:example.org", "@alice\tlegacy:example.org"} {
		t.Run(id, func(t *testing.T) {
			row, _ := json.Marshal(map[string]any{"type": "m.room.member", "room_id": "!r:s", "state_key": id, "content": map[string]any{"membership": "invite"}})
			body := `{"chunk":[` + membersJoined + `,` + string(row) + `]}`
			c := New("https://matrix.invalid", "synthetic")
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
			})}
			got, err := c.CurrentRoomMembers(context.Background(), "!r:s", "@me:s")
			if err != nil || len(got) != 2 {
				t.Fatalf("valid historical/current member %q rejected: got=%+v err=%v", id, got, err)
			}
		})
	}
}
