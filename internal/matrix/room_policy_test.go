package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const policyStateFixture = `[{"type":"m.room.join_rules","state_key":"","room_id":"!selected:fixture","content":{"join_rule":"invite"}}]`

func TestRoomPolicyStateHTTP(t *testing.T) {
	for name, body := range map[string]string{
		"valid":             policyStateFixture,
		"room-omitted":      strings.Replace(policyStateFixture, `"room_id":"!selected:fixture",`, "", 1),
		"trailing":          policyStateFixture + `{}`,
		"duplicate":         strings.Replace(policyStateFixture, `"join_rule":"invite"`, `"join_rule":"public","join_rule":"invite"`, 1),
		"escaped-duplicate": strings.Replace(policyStateFixture, `"join_rule":"invite"`, `"join_rule":"public","join_\u0072ule":"invite"`, 1),
		"null":              `null`,
		"object":            `{}`,
		"null-item":         `[null]`,
		"null-content":      strings.Replace(policyStateFixture, `{"join_rule":"invite"}`, `null`, 1),
		"missing-key":       strings.Replace(policyStateFixture, `"state_key":"",`, "", 1),
		"null-key":          strings.Replace(policyStateFixture, `"state_key":""`, `"state_key":null`, 1),
		"case-key":          strings.Replace(policyStateFixture, `"state_key"`, `"State_Key"`, 1),
		"case-type":         strings.Replace(policyStateFixture, `"type"`, `"Type"`, 1),
		"case-shadow":       strings.Replace(policyStateFixture, `"type":`, `"TYPE":"m.room.encryption","type":`, 1),
		"wrong-room":        strings.Replace(policyStateFixture, "!selected:fixture", "!other:fixture", 1),
		"null-room":         strings.Replace(policyStateFixture, `"!selected:fixture"`, `null`, 1),
		"invalid-unicode":   strings.Replace(policyStateFixture, "invite", `\ud800`, 1),
		"redacted":          strings.Replace(policyStateFixture, `"content":`, `"unsigned":{"redacted_because":{}},"content":`, 1),
		"redacted-case":     strings.Replace(policyStateFixture, `"content":`, `"unsigned":{"Redacted_Because":null},"content":`, 1),
		"unsigned-case":     strings.Replace(policyStateFixture, `"content":`, `"Unsigned":{},"content":`, 1),
		"unsigned-null":     strings.Replace(policyStateFixture, `"content":`, `"unsigned":null,"content":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			calls, guards := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || r.URL.Path != "/_matrix/client/v3/rooms/!selected:fixture/state" {
					t.Error("wrong selected endpoint")
				}
				w.Write([]byte(body))
			}))
			defer server.Close()
			c := New(server.URL, "synthetic-token")
			c.BeforeRequest = func() error { guards++; return nil }
			state, err := c.GetRoomPolicyState(context.Background(), "!selected:fixture")
			valid := name == "valid" || name == "room-omitted"
			if (err == nil) != valid || calls != 1 || guards != 1 {
				t.Fatalf("readback valid=%t err=%v calls=%d guards=%d", valid, err, calls, guards)
			}
			if valid && (len(state) != 1 || state[0].Type != "m.room.join_rules" || state[0].StateKey != "" || state[0].Content["join_rule"] != "invite") {
				t.Fatalf("incorrect state: %+v", state)
			}
			if !valid && state != nil {
				t.Fatal("usable partial state escaped refusal")
			}
		})
	}
}

type policyReadCloser struct {
	io.Reader
	readErr, closeErr error
	closed            bool
}

func (b *policyReadCloser) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.Reader.Read(p)
}
func (b *policyReadCloser) Close() error { b.closed = true; return b.closeErr }

func TestRoomPolicyStateTransport(t *testing.T) {
	for _, kind := range []string{"valid", "status-201", "status-206", "status-403", "read-error", "close-error", "length-short", "length-long", "canceled", "at-bound", "over-bound"} {
		t.Run(kind, func(t *testing.T) {
			raw := policyStateFixture
			if kind == "at-bound" || kind == "over-bound" {
				raw = strings.Replace(raw, `"content":`, `"extension":"`+strings.Repeat("x", VerificationEntityLimit-len(raw)-len(`"extension":"",`))+`","content":`, 1)
				if len(raw) != VerificationEntityLimit {
					t.Fatal("wrong bound fixture")
				}
				if kind == "over-bound" {
					raw += " "
				}
			}
			body := &policyReadCloser{Reader: strings.NewReader(raw)}
			status, length := 200, int64(len(raw))
			switch kind {
			case "status-201":
				status = 201
			case "status-206":
				status = 206
			case "status-403":
				status = 403
			case "read-error":
				body.readErr = errors.New("synthetic read failure")
			case "close-error":
				body.closeErr = errors.New("synthetic close failure")
			case "length-short":
				length--
			case "length-long":
				length++
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := New("http://fixture.invalid", "synthetic")
			c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
				if kind == "canceled" {
					cancel()
				}
				return &http.Response{StatusCode: status, ContentLength: length, Body: body, Request: r}, nil
			})}
			state, err := c.GetRoomPolicyState(ctx, "!selected:fixture")
			valid := kind == "valid" || kind == "at-bound"
			if (err == nil) != valid || !body.closed || (!valid && state != nil) {
				t.Fatalf("transport valid=%t err=%v closed=%t", valid, err, body.closed)
			}
		})
	}
	t.Run("selection and connection guard", func(t *testing.T) {
		c, calls := verificationClient(policyStateFixture)
		if _, err := c.GetRoomPolicyState(context.Background(), "not-room"); err == nil || *calls != 0 {
			t.Fatal("invalid selection reached transport")
		}
		c.BeforeRequest = func() error { return errors.New("connection changed") }
		if _, err := c.GetRoomPolicyState(context.Background(), "!selected:fixture"); err == nil || *calls != 0 {
			t.Fatal("connection guard bypassed")
		}
	})
}
