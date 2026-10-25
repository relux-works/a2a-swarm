package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const membersJoined = `{"type":"m.room.member","room_id":"!r:s","state_key":"@me:s","content":{"membership":"join"}}`
const membersInvited = `{"type":"m.room.member","room_id":"!r:s","state_key":"@guest:other","content":{"membership":"invite","reason":"private-reason","displayname":"private-name"},"unsigned":{"private":"private-history"}}`

// Proves the canonical read projects sorted active principals only and rejects
// malformed/partial envelopes or archived caller state without partial results.
// It attests the returned response, not independent server honesty.
func TestCurrentRoomMembersContracts(t *testing.T) {
	valid := `{"chunk":[` + membersJoined + `,` + membersInvited + `]}`
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"valid", valid, nil},
		{"empty-filterable", `{"chunk":[` + membersJoined + `]}`, nil},
		{"missing", `{}`, ErrMembersMalformed},
		{"null", `{"chunk":null}`, ErrMembersMalformed},
		{"object", `{"chunk":{}}`, ErrMembersMalformed},
		{"null-row", `{"chunk":[null]}`, ErrMembersMalformed},
		{"bad-late-row", `{"chunk":[` + membersJoined + `,{}]}`, ErrMembersMalformed},
		{"wrong-room", strings.Replace(valid, `!r:s`, `!else:s`, 1), ErrMembersMalformed},
		{"missing-type", strings.Replace(valid, `"m.room.member"`, `null`, 1), ErrMembersMalformed},
		{"wrong-type", strings.Replace(valid, `m.room.member`, `m.room.message`, 1), ErrMembersMalformed},
		{"null-content", `{"chunk":[{"type":"m.room.member","room_id":"!r:s","state_key":"@me:s","content":null}]}`, ErrMembersMalformed},
		{"unknown-membership", strings.Replace(valid, `"join"`, `"future"`, 1), ErrMembersMalformed},
		{"membership-number", strings.Replace(valid, `"join"`, `42`, 1), ErrMembersMalformed},
		{"missing-key", strings.Replace(valid, `"@me:s"`, `null`, 1), ErrMembersMalformed},
		{"bad-id", strings.Replace(valid, `@guest:other`, `@guest`, 1), ErrMembersMalformed},
		{"nul-id", strings.Replace(valid, `@guest:other`, `@guest\u0000:other`, 1), ErrMembersMalformed},
		{"duplicate-user", `{"chunk":[` + membersJoined + `,` + membersJoined + `]}`, ErrMembersMalformed},
		{"duplicate-json", `{"chunk":[],"chunk":[` + membersJoined + `]}`, ErrMembersMalformed},
		{"duplicate-content", strings.Replace(valid, `"membership":"invite"`, `"membership":"leave","membership":"invite"`, 1), ErrMembersMalformed},
		{"trailing-value", valid + ` {}`, ErrMembersMalformed},
		{"syntax", valid[:len(valid)-1], ErrMembersMalformed},
		{"page", `{"next_batch":"x","chunk":[` + membersJoined + `]}`, ErrMembersIncomplete},
		{"limited", `{"limited":true,"chunk":[` + membersJoined + `]}`, ErrMembersIncomplete},
		{"partial", `{"partial_state":true,"chunk":[` + membersJoined + `]}`, ErrMembersIncomplete},
		{"empty-unverified", `{"chunk":[]}`, ErrMembersNotCurrent},
		{"missing-caller", `{"chunk":[` + membersInvited + `]}`, ErrMembersNotCurrent},
		{"left-caller", strings.Replace(valid, `"join"`, `"leave"`, 1), ErrMembersNotCurrent},
		{"invited-caller", strings.Replace(valid, `"join"`, `"invite"`, 1), ErrMembersNotCurrent},
		{"excluded-memberships", `{"chunk":[` + membersJoined + `,` + strings.Replace(membersInvited, `"invite"`, `"knock"`, 1) + `]}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := New("http://matrix.invalid", "synthetic-token")
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != v3+"/rooms/!r:s/members" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: int64(len(tc.body)), Request: r}, nil
			})}
			got, err := c.CurrentRoomMembers(context.Background(), "!r:s", "@me:s")
			if !errors.Is(err, tc.want) || calls != 1 {
				t.Fatalf("got %v; want %v, calls %d", err, tc.want, calls)
			}
			if err != nil && got != nil {
				t.Fatal("partial roster returned")
			}
			if tc.name == "valid" && (len(got) != 2 || got[0].UserID != "@guest:other" || got[1].UserID != "@me:s") {
				t.Fatalf("projection: %+v", got)
			}
			if tc.name == "excluded-memberships" && len(got) != 1 {
				t.Fatalf("nonactive member leaked: %+v", got)
			}
		})
	}
}

// Proves clean EOF/close, entity length, status, cap and context are required;
// denial/auth/network remain errors rather than being turned into empty success.
func TestCurrentRoomMembersTransport(t *testing.T) {
	for _, kind := range []string{"valid", "denied", "auth", "network", "partial-status", "read-error", "close-error", "short-length", "over-limit", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := New("http://matrix.invalid", "synthetic-token")
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				if kind == "network" {
					return nil, errors.New("synthetic transport unavailable")
				}
				body := `{"chunk":[` + membersJoined + `]}`
				status := 200
				if kind == "denied" {
					status = 403
					body = `{"errcode":"M_FORBIDDEN","error":"private-server-error"}`
				}
				if kind == "auth" {
					status = 401
					body = `{"errcode":"M_UNKNOWN_TOKEN","error":"private-server-error"}`
				}
				if kind == "partial-status" {
					status = 206
				}
				if kind == "over-limit" {
					body = strings.Repeat(" ", membersResponseLimit+1)
				}
				b := &membershipBody{Reader: strings.NewReader(body)}
				if kind == "read-error" {
					b.readErr = io.ErrUnexpectedEOF
				}
				if kind == "close-error" {
					b.closeErr = errors.New("synthetic close failure")
				}
				if kind == "cancel" {
					b.cancel = cancel
				}
				length := int64(len(body))
				if kind == "short-length" {
					length++
				}
				return &http.Response{StatusCode: status, Body: b, ContentLength: length, Request: r}, nil
			})}
			got, err := c.CurrentRoomMembers(ctx, "!r:s", "@me:s")
			if kind == "valid" {
				if err != nil || len(got) != 1 {
					t.Fatalf("valid: %v %+v", err, got)
				}
				return
			}
			if err == nil || got != nil {
				t.Fatalf("failure became success: %v %+v", err, got)
			}
			if strings.Contains(err.Error(), "private-server-error") || strings.Contains(err.Error(), "synthetic-token") {
				t.Fatal("secret/raw server diagnostic escaped")
			}
			switch kind {
			case "denied":
				if !IsStatus(err, 403) {
					t.Fatal(err)
				}
			case "auth":
				if !errors.Is(err, ErrAuth) {
					t.Fatal(err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "network":
			default:
				if !errors.Is(err, ErrMembersIncomplete) {
					t.Fatal(err)
				}
			}
		})
	}
}

type membershipBody struct {
	io.Reader
	readErr, closeErr error
	cancel            context.CancelFunc
}

func (b *membershipBody) Read(p []byte) (int, error) {
	n, e := b.Reader.Read(p)
	if e == io.EOF {
		if b.cancel != nil {
			b.cancel()
		}
		if b.readErr != nil {
			return n, b.readErr
		}
	}
	return n, e
}
func (b *membershipBody) Close() error { return b.closeErr }
