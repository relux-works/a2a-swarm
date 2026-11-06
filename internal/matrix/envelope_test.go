package matrix

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// envelopeContent builds event content JSON for the projection fixtures. The
// body is quoted by encoding/json; use envelopeContentRaw for exact spellings.
func envelopeContent(envelope []byte, body string) string {
	quoted, _ := json.Marshal(body)
	return `{"msgtype":"m.text","body":` + string(quoted) + `,"` + EnvelopeContentKey + `":"` + base64.RawURLEncoding.EncodeToString(envelope) + `"}`
}

// envelopeSpellings keeps escapes (c, \r\n) exactly as a homeserver may
// return them, so decoded text can be compared with the intended bytes.
const (
	envelopeValid = `{"version":"swarm-envelope/2","body":{"type":"message/1"},"id":"sha256:00","signatures":[]}`
	envelopeBase  = `{"msgtype":"m.text","body":"hello","` + EnvelopeContentKey + `":"e30"}`
)

func allBytes() []byte {
	b := make([]byte, 0, 512)
	for i := 0; i < 256; i++ {
		b = append(b, byte(i))
	}
	return append(b, 0, 0xff, 0xfe, '\r', '\n', 0)
}

// refusalCase mutates the valid response and names the single intended code.
type refusalCase struct {
	name string
	body string
	code string
}

// Proves the projection returns the exact envelope bytes, the exact decoded
// event text, the full sender spelling and the requested identifiers, from one
// GET, for ordinary and binary-safe envelopes. It does not verify authority.
func TestEnvelopeEventExactObservation(t *testing.T) {
	crlf := []byte("\xef\xbb\xbf{\"a\":\"\u0436\r\n\t \"}\r\n \n")
	cases := []struct {
		name     string
		envelope []byte
		content  string // exact spelling; empty means build it from body
		body     string
	}{
		// Escapes stay as a homeserver may spell them; decoding must equal body.
		{"json-with-crlf-and-unicode", crlf, `{"msgtype":"m.text","body":"a\r\nb\n\u0063 \u00e9\ud83d\ude00","` + EnvelopeContentKey + `":"` + base64.RawURLEncoding.EncodeToString(crlf) + `"}`, "a\r\nb\nc é😀"},
		{"binary-all-byte-values", allBytes(), "", "x\x00y"},
		{"single-nul-byte", []byte{0}, "", "z"},
		{"empty-text-is-allowed", []byte("{}"), "", ""},
		{"escaped-nul-in-text", []byte("{}"), "", "a\u0000b"},
	}
	for _, tc := range cases {
		for _, extra := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "", true: "/with-unknown-fields"}[extra], func(t *testing.T) {
				content := tc.content
				if content == "" {
					content = envelopeContent(tc.envelope, tc.body)
				}
				response := verificationResponse(content)
				if extra { // unsigned data and other fields are never exported
					response = strings.Replace(response, `"content":`, `"unsigned":{"private":"not-exported"},"content":`, 1)
				}
				c, calls := verificationClient(response)
				guards := 0
				c.BeforeRequest = func() error { guards++; return nil }
				v, err := c.GetEnvelopeEvent(context.Background(), "!selected:fixture", "$selected")
				if err != nil {
					t.Fatal(err)
				}
				if *calls != 1 || guards != 1 {
					t.Fatalf("not exactly one guarded GET: calls=%d guards=%d", *calls, guards)
				}
				if !bytes.Equal(v.Envelope, tc.envelope) {
					t.Fatalf("envelope bytes changed: % x != % x", v.Envelope, tc.envelope)
				}
				if string(v.BodyText) != tc.body {
					t.Fatalf("body text changed: %q != %q", v.BodyText, tc.body)
				}
				if v.Sender != "@Historical:foreign.invalid" || v.EventID != "$selected" || v.RoomID != "!selected:fixture" {
					t.Fatalf("identifiers changed: %+v", v)
				}
			})
		}
	}
}

// Proves a foreign-server, mixed-case sender keeps its exact spelling and is
// not collapsed to a local name or compared with the receiver: the projection
// carries the authenticated sender and leaves the authority decision to the
// consumer.
func TestEnvelopeEventSenderSpellingPreserved(t *testing.T) {
	for _, sender := range []string{"@Same:alpha.invalid", "@Same:beta.invalid", "@same:alpha.invalid", "@ж:ж.invalid"} {
		response := strings.Replace(verificationResponse(envelopeBase), "@Historical:foreign.invalid", sender, 1)
		c, _ := verificationClient(response)
		v, err := c.GetEnvelopeEvent(context.Background(), "!selected:fixture", "$selected")
		if err != nil || v.Sender != sender {
			t.Fatalf("sender changed: got %q want %q err=%v", v.Sender, sender, err)
		}
	}
}

// Proves each malformed, ambiguous or mismatching response is refused with its
// own stable code, returns no projection, makes exactly one request and echoes
// no response data. The "valid-neighbour" and "trailing-newline" rows are the
// positive controls: a projection that refuses everything cannot pass them.
func TestEnvelopeEventRefusals(t *testing.T) {
	base := verificationResponse(envelopeBase)
	swap := func(old, next string) string {
		if strings.Count(base, old) != 1 {
			t.Fatalf("fixture anchor %q", old)
		}
		return strings.Replace(base, old, next, 1)
	}
	content := func(c string) string { return verificationResponse(c) }
	enc := func(v string) string {
		return `{"msgtype":"m.text","body":"hello","` + EnvelopeContentKey + `":` + v + `}`
	}
	cases := []refusalCase{
		{"valid-neighbour", base, ""},
		{"trailing-newline", base + "\n", ""},
		{"foreign-sender-valid", swap("@Historical:foreign.invalid", "@Other:elsewhere.invalid"), ""},

		// Server redaction metadata refuses even with content still present, as
		// GetVerificationEvent does; an unrelated unsigned field stays valid.
		{"unsigned-age-valid", strings.TrimSuffix(base, "}") + `,"unsigned":{"age":12}}`, ""},
		{"redacted-with-content", strings.TrimSuffix(base, "}") + `,"unsigned":{"redacted_because":{"type":"m.room.redaction"}}}`, RefuseEventRedacted},
		{"redacted-null-marker", strings.TrimSuffix(base, "}") + `,"unsigned":{"redacted_because":null}}`, RefuseEventRedacted},
		{"redacted-case-variant", strings.TrimSuffix(base, "}") + `,"unsigned":{"Redacted_Because":{}}}`, RefuseEventRedacted},
		{"unsigned-not-object", strings.TrimSuffix(base, "}") + `,"unsigned":"redacted"}`, RefuseResponseInvalid},
		{"unsigned-case-variant", strings.TrimSuffix(base, "}") + `,"Unsigned":{"redacted_because":{}}}`, RefuseKeyCaseAlias},

		// Trailing data after the one JSON document.
		{"trailing-document", base + "{}", RefuseTrailingData},
		{"trailing-junk", base + "x", RefuseTrailingData},
		{"trailing-array", base + "[]", RefuseTrailingData},

		// Duplicate member names at every depth, including escaped spellings.
		{"duplicate-event-id", swap(`"event_id":`, `"event_id":"$decoy","event_id":`), RefuseDuplicateKey},
		{"duplicate-event-id-escaped", swap(`"event_id":`, `"event_id":"$decoy","event_id":`), RefuseDuplicateKey},
		{"duplicate-envelope-key", content(`{"msgtype":"m.text","body":"hello","` + EnvelopeContentKey + `":"e30","` + EnvelopeContentKey + `":"e31"}`), RefuseDuplicateKey},
		{"duplicate-envelope-key-escaped", content(`{"msgtype":"m.text","body":"hello","` + EnvelopeContentKey + `":"e30","works.relux.swarm.envelope":"e31"}`), RefuseDuplicateKey},
		{"duplicate-body", content(`{"msgtype":"m.text","body":"a","body":"b","` + EnvelopeContentKey + `":"e30"}`), RefuseDuplicateKey},
		{"duplicate-nested", content(`{"msgtype":"m.text","body":"a","m.mentions":{"user_ids":[],"user_ids":[]},"` + EnvelopeContentKey + `":"e30"}`), RefuseDuplicateKey},

		// Case aliases of known names, and unrelated fold collisions.
		{"alias-with-real-envelope-key", content(`{"msgtype":"m.text","body":"hello","` + EnvelopeContentKey + `":"e30","Works.Relux.Swarm.Envelope":"e31"}`), RefuseKeyCaseAlias},
		{"alias-only-envelope-key", content(`{"msgtype":"m.text","body":"hello","Works.Relux.Swarm.Envelope":"e30"}`), RefuseKeyCaseAlias},
		{"alias-kelvin-sign", content(`{"msgtype":"m.text","body":"hello","wor` + "K" + `s.relux.swarm.envelope":"e30"}`), RefuseKeyCaseAlias},
		{"alias-long-s", content(`{"msgtype":"m.text","body":"hello","work` + "ſ" + `.relux.swarm.envelope":"e30"}`), RefuseKeyCaseAlias},
		{"alias-body", content(`{"msgtype":"m.text","body":"a","BODY":"b","` + EnvelopeContentKey + `":"e30"}`), RefuseKeyCaseAlias},
		{"alias-msgtype", content(`{"msgtype":"m.text","MsgType":"m.notice","body":"a","` + EnvelopeContentKey + `":"e30"}`), RefuseKeyCaseAlias},
		{"unrelated-collision", content(`{"msgtype":"m.text","body":"a","extra":1,"EXTRA":2,"` + EnvelopeContentKey + `":"e30"}`), RefuseKeyCaseAlias},
		{"alias-event-id", swap(`"event_id":`, `"EVENT_ID":"$decoy","event_id":`), RefuseKeyCaseAlias},
		{"alias-event-id-only", swap(`"event_id":`, `"Event_ID":`), RefuseKeyCaseAlias},

		// Wrong event, room, type and sender.
		{"wrong-event", swap("$selected", "$other"), RefuseEventIDMismatch},
		{"event-absent", swap(`"event_id":"$selected",`, ""), RefuseEventIDMismatch},
		{"event-number", swap(`"event_id":"$selected"`, `"event_id":42`), RefuseEventIDMismatch},
		{"wrong-room", swap("!selected:fixture", "!other:fixture"), RefuseRoomIDMismatch},
		{"room-empty", swap(`"room_id":"!selected:fixture"`, `"room_id":""`), RefuseRoomIDMismatch},
		{"room-null", swap(`"room_id":"!selected:fixture"`, `"room_id":null`), RefuseRoomIDMismatch},
		{"room-number", swap(`"room_id":"!selected:fixture"`, `"room_id":42`), RefuseRoomIDMismatch},
		{"room-absent", swap(`"room_id":"!selected:fixture",`, ""), RefuseRoomIDMissing},
		{"wrong-event-type", swap("m.room.message", "m.room.encrypted"), RefuseEventTypeInvalid},
		{"event-type-absent", swap(`"type":"m.room.message",`, ""), RefuseEventTypeInvalid},
		{"sender-short", swap("@Historical:foreign.invalid", "Historical"), RefuseSenderInvalid},
		{"sender-no-server", swap("@Historical:foreign.invalid", "@Historical"), RefuseSenderInvalid},
		{"sender-control", swap("@Historical:foreign.invalid", `@His\ntorical:foreign.invalid`), RefuseSenderInvalid},
		{"sender-space", swap("@Historical:foreign.invalid", `@His torical:foreign.invalid`), RefuseSenderInvalid},
		{"sender-null", swap(`"@Historical:foreign.invalid"`, `null`), RefuseSenderInvalid},
		{"sender-number", swap(`"@Historical:foreign.invalid"`, `7`), RefuseSenderInvalid},
		{"sender-absent", swap(`"sender":"@Historical:foreign.invalid",`, ""), RefuseSenderInvalid},

		// Content shape.
		{"empty-redaction", content(`{}`), RefuseContentInvalid},
		{"null-content", content(`null`), RefuseContentInvalid},
		{"array-content", content(`[]`), RefuseContentInvalid},
		{"string-content", content(`"x"`), RefuseContentInvalid},
		{"missing-content", swap(`,"content":`+envelopeBase, ""), RefuseContentInvalid},
		{"msgtype-notice", content(strings.Replace(envelopeBase, "m.text", "m.notice", 1)), RefuseMsgtypeInvalid},
		{"msgtype-absent", content(strings.Replace(envelopeBase, `"msgtype":"m.text",`, "", 1)), RefuseMsgtypeInvalid},
		{"msgtype-number", content(strings.Replace(envelopeBase, `"m.text"`, "1", 1)), RefuseMsgtypeInvalid},
		{"body-absent", content(strings.Replace(envelopeBase, `"body":"hello",`, "", 1)), RefuseBodyMissing},
		{"body-null", content(strings.Replace(envelopeBase, `"hello"`, "null", 1)), RefuseBodyMissing},
		{"body-number", content(strings.Replace(envelopeBase, `"hello"`, "5", 1)), RefuseBodyMissing},
		{"body-array", content(strings.Replace(envelopeBase, `"hello"`, `["hello"]`, 1)), RefuseBodyMissing},

		// Envelope key: missing and malformed are different, specific refusals.
		{"envelope-missing", content(`{"msgtype":"m.text","body":"hello"}`), RefuseEnvelopeMissing},
		{"envelope-null", content(enc(`null`)), RefuseEnvelopeBad},
		{"envelope-number", content(enc(`123`)), RefuseEnvelopeBad},
		{"envelope-array", content(enc(`["e30"]`)), RefuseEnvelopeBad},
		{"envelope-object", content(enc(`{"e30":1}`)), RefuseEnvelopeBad},
		{"envelope-empty-string", content(enc(`""`)), RefuseEnvelopeBad},
		{"envelope-padded", content(enc(`"e30="`)), RefuseEnvelopeBad},
		{"envelope-standard-alphabet", content(enc(`"+/+/"`)), RefuseEnvelopeBad},
		{"envelope-newline-inside", content(enc(`"e3\n0"`)), RefuseEnvelopeBad},
		{"envelope-space-inside", content(enc(`"e3 0"`)), RefuseEnvelopeBad},
		{"envelope-trailing-space", content(enc(`"e30 "`)), RefuseEnvelopeBad},
		{"envelope-nonzero-trailing-bits", content(enc(`"e31"`)), RefuseEnvelopeBad},
		{"envelope-impossible-length", content(enc(`"e"`)), RefuseEnvelopeBad},
		{"envelope-bad-character", content(enc(`"e30!"`)), RefuseEnvelopeBad},

		// Strict UTF-8 text.
		{"invalid-utf8-text", content("{\"msgtype\":\"m.text\",\"body\":\"" + string([]byte{'a', 0xff, 'b'}) + "\",\"" + EnvelopeContentKey + "\":\"e30\"}"), RefuseInvalidUTF8},
		{"invalid-utf8-overlong", content("{\"msgtype\":\"m.text\",\"body\":\"" + string([]byte{0xc0, 0xaf}) + "\",\"" + EnvelopeContentKey + "\":\"e30\"}"), RefuseInvalidUTF8},
		{"lone-surrogate-escape", content(strings.Replace(envelopeBase, `"hello"`, `"\ud800"`, 1)), RefuseInvalidUTF8},
		{"lone-low-surrogate-escape", content(strings.Replace(envelopeBase, `"hello"`, `"\udc00x"`, 1)), RefuseInvalidUTF8},
		{"invalid-utf8-envelope-value", content("{\"msgtype\":\"m.text\",\"body\":\"a\",\"" + EnvelopeContentKey + "\":\"" + string([]byte{0xff}) + "\"}"), RefuseInvalidUTF8},

		// Not an object / not JSON.
		{"top-array", "[" + base + "]", RefuseResponseInvalid},
		{"truncated", base[:len(base)-1], RefuseResponseInvalid},
		{"not-json", "nope", RefuseResponseInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := verificationClient(tc.body)
			v, err := c.GetEnvelopeEvent(context.Background(), "!selected:fixture", "$selected")
			if *calls != 1 {
				t.Fatalf("%s: %d requests, want exactly 1", tc.name, *calls)
			}
			if tc.code == "" {
				if err != nil || len(v.Envelope) == 0 {
					t.Fatalf("%s: valid response refused: %v", tc.name, err)
				}
				return
			}
			if got := RefusalCode(err); got != tc.code {
				t.Fatalf("%s: got code %q want %q (err=%v)", tc.name, got, tc.code, err)
			}
			if v.EventID != "" || v.Sender != "" || v.RoomID != "" || len(v.Envelope) != 0 || len(v.BodyText) != 0 {
				t.Fatalf("%s: projection returned with a refusal: %+v", tc.name, v)
			}
			for _, leaked := range []string{"Historical", "decoy", "hello", "e30", "foreign"} {
				if strings.Contains(err.Error(), leaked) {
					t.Fatalf("%s: response data %q escaped in the diagnostic: %v", tc.name, leaked, err)
				}
			}
		})
	}
}

// Proves the inclusive entity (1 MiB) and content (64 KiB) caps with their
// neighbouring refusals, each with its own code. Unknown extensions consume
// entity budget though they are never projected.
func TestEnvelopeEventBounds(t *testing.T) {
	const prefix = `{"msgtype":"m.text","body":"`
	suffix := `","` + EnvelopeContentKey + `":"e30"}`
	mk := func(n int) string { return prefix + strings.Repeat("x", n-len(prefix)-len(suffix)) + suffix }
	content := mk(VerificationContentLimit)
	if len(content) != VerificationContentLimit {
		t.Fatalf("content fixture size %d", len(content))
	}
	base := verificationResponse(content)
	whole := strings.TrimSuffix(base, "}") + `,"extension":"` + strings.Repeat("x", VerificationEntityLimit-len(base)-len(`,"extension":""`)) + `"}`
	if len(whole) != VerificationEntityLimit {
		t.Fatalf("entity fixture size %d", len(whole))
	}
	for _, tc := range []struct{ name, body, code string }{
		{"content-limit", base, ""},
		{"content-plus-one", verificationResponse(mk(VerificationContentLimit + 1)), RefuseContentTooLarge},
		{"entity-limit", whole, ""},
		{"entity-plus-one", whole + " ", RefuseEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, calls := verificationClient(tc.body)
			v, err := c.GetEnvelopeEvent(context.Background(), "!selected:fixture", "$selected")
			if got := RefusalCode(err); got != tc.code || (tc.code == "" && err != nil) || *calls != 1 {
				t.Fatalf("got code %q err=%v calls=%d, want code %q", got, err, *calls, tc.code)
			}
			if tc.code != "" && len(v.Envelope) != 0 {
				t.Fatal("projection returned with a refusal")
			}
		})
	}
}

// Proves the byte codec: exact round trip for every byte value and for the
// specification's size limit, an independent envelope limit, and strict decoding
// (including the over-limit boundary and its valid neighbour).
func TestEnvelopeCodec(t *testing.T) {
	t.Run("round-trip", func(t *testing.T) {
		limit := bytes.Repeat([]byte{0}, EnvelopeMaxBytes)
		for _, in := range [][]byte{{0}, {0xff}, []byte("{}"), allBytes(), limit} {
			s, err := EncodeEnvelope(in)
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsAny(s, "=+/\r\n ") {
				t.Fatalf("encoding is not unpadded base64url (%d chars)", len(s))
			}
			out, err := DecodeEnvelope(s)
			if err != nil || !bytes.Equal(in, out) {
				t.Fatalf("round trip changed %d bytes: %v", len(in), err)
			}
		}
	})
	t.Run("encode-refusals", func(t *testing.T) {
		if _, err := EncodeEnvelope(make([]byte, EnvelopeMaxBytes+1)); RefusalCode(err) != RefuseEnvelopeTooLarge {
			t.Fatalf("encode over limit: %v", err)
		}
		if _, err := EncodeEnvelope(nil); RefusalCode(err) != RefuseEnvelopeEmpty {
			t.Fatalf("encode empty: %v", err)
		}
		if _, err := EncodeEnvelope(make([]byte, EnvelopeMaxBytes)); err != nil {
			t.Fatalf("encode at limit refused: %v", err)
		}
	})
	t.Run("decode-over-limit", func(t *testing.T) {
		// 87,382 characters decode to exactly 65,536 bytes; one more decodes to
		// 65,537 and must be refused before any decoding work.
		if out, err := DecodeEnvelope(strings.Repeat("A", 87382)); err != nil || len(out) != EnvelopeMaxBytes {
			t.Fatalf("at limit: %d bytes, %v", len(out), err)
		}
		if _, err := DecodeEnvelope(strings.Repeat("A", 87383)); RefusalCode(err) != RefuseEnvelopeTooLarge {
			t.Fatalf("over limit: %v", err)
		}
	})
	t.Run("decode-malformed", func(t *testing.T) {
		for _, bad := range []string{"", "e", "e30=", "e3\n0", "e3\r\n0", "e3 0", " e30", "+/+/", "e31", "e30!", "é"} {
			if _, err := DecodeEnvelope(bad); RefusalCode(err) != RefuseEnvelopeBad {
				t.Fatalf("%q: got %v", bad, err)
			}
		}
		if out, err := DecodeEnvelope("e30"); err != nil || string(out) != "{}" {
			t.Fatalf("valid neighbour: %q %v", out, err)
		}
	})
	t.Run("content-budget", func(t *testing.T) {
		at := map[string]string{"k": strings.Repeat("x", EnvelopeEventContentBudget-len(`{"k":""}`))}
		if err := CheckEventContentBudget(at); err != nil {
			t.Fatalf("content exactly at the budget refused: %v", err)
		}
		over := map[string]string{"k": strings.Repeat("x", EnvelopeEventContentBudget-len(`{"k":""}`)+1)}
		if err := CheckEventContentBudget(over); RefusalCode(err) != RefuseEventTooLarge {
			t.Fatalf("content one byte over the budget: %v", err)
		}
	})
}

// Proves one request without redirects or fallback, the original-request
// provenance gate, complete I/O and the connection guard: none can be replaced
// by a successful-looking body. The "valid" row is the positive control.
func TestEnvelopeEventTransport(t *testing.T) {
	for _, kind := range []string{"valid", "missing-request", "wrong-url", "wrong-method", "redirect", "cross-origin", "bad-length", "close-error", "read-error", "cancel", "binding"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := verificationResponse(envelopeBase)
			calls := 0
			c := New("https://fixture.invalid", "fixture-token")
			c.BeforeRequest = func() error {
				if kind == "binding" {
					return errors.New("binding refused")
				}
				return nil
			}
			c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				res := &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
				switch kind {
				case "missing-request":
					res.Request = nil
				case "wrong-url":
					q := r.Clone(r.Context())
					u := *r.URL
					u.Path += "/other"
					q.URL = &u
					res.Request = q
				case "wrong-method":
					q := r.Clone(r.Context())
					q.Method = "POST"
					res.Request = q
				case "redirect", "cross-origin":
					res.StatusCode = 302
					target := "https://fixture.invalid/redirect"
					if kind == "cross-origin" {
						target = "https://elsewhere.invalid/redirect"
					}
					res.Header.Set("Location", target)
				case "bad-length":
					res.ContentLength++
				case "close-error":
					res.Body = verificationBadBody{Reader: strings.NewReader(body)}
				case "read-error":
					res.Body = verificationReadError{}
				case "cancel":
					cancel()
				}
				return res, nil
			})}
			v, err := c.GetEnvelopeEvent(ctx, "!selected:fixture", "$selected")
			if kind == "valid" {
				if err != nil || v.EventID != "$selected" || calls != 1 {
					t.Fatalf("positive: %v calls=%d", err, calls)
				}
				return
			}
			if err == nil || v.EventID != "" || len(v.Envelope) != 0 || calls > 1 {
				t.Fatalf("invalid transport accepted: %+v %v calls=%d", v, err, calls)
			}
			switch kind {
			case "bad-length", "close-error", "read-error":
				if RefusalCode(err) != RefuseResponseInvalid {
					t.Fatalf("got %v, want %s", err, RefuseResponseInvalid)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation reported as %v", err)
				}
			case "binding":
				if calls != 0 {
					t.Fatal("request sent after the guard refused")
				}
			}
		})
	}
}

// Proves invalid identifiers are refused with their own code before any
// request, rather than repaired or resolved into another event.
func TestEnvelopeEventInvalidSelection(t *testing.T) {
	for _, ids := range [][2]string{{"#alias:fixture", "$selected"}, {"!", "$selected"}, {"!selected:fixture", "$"}, {"!selected:fixture", "$" + strings.Repeat("x", 255)}, {"!selected:fixture", "selected"}} {
		c, calls := verificationClient(verificationResponse(envelopeBase))
		_, err := c.GetEnvelopeEvent(context.Background(), ids[0], ids[1])
		if RefusalCode(err) != RefuseSelectionInvalid || *calls != 0 {
			t.Fatalf("%v: got %v calls=%d", ids, err, *calls)
		}
	}
}

// Proves the projection's scoped no-redirect transport does not change another
// caller's configured same-origin redirect behaviour.
func TestEnvelopeEventRedirectPolicyUnchanged(t *testing.T) {
	c := New("https://fixture.invalid", "fixture-token")
	calls := 0
	c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/target" {
			return &http.Response{StatusCode: 302, Request: r, Header: http.Header{"Location": []string{"/target"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Request: r, Body: io.NopCloser(strings.NewReader(verificationResponse(envelopeBase)))}, nil
	})}
	if _, err := c.GetEnvelopeEvent(context.Background(), "!selected:fixture", "$selected"); err == nil || calls != 1 {
		t.Fatal("envelope projection followed a redirect")
	}
	if _, err := c.GetEvent(context.Background(), "!selected:fixture", "$selected"); err != nil || calls != 3 {
		t.Fatalf("ordinary redirect behaviour changed: %v calls%d", err, calls)
	}
}

// Proves the refinement of the strict reader's errors did not change what it
// refuses or the diagnostic text of the pre-existing G2 projection.
func TestStrictReaderErrorsStayErrWireJSON(t *testing.T) {
	for _, raw := range []string{`{"a":1,"a":2}`, `{"a":1}x`, `{"a":"\ud800"}`, `{"a":1`, `[1,]`} {
		_, err := strictJSON([]byte(raw))
		if !errors.Is(err, errWireJSON) || err.Error() != errWireJSON.Error() {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	if _, err := strictJSON([]byte(`{"a":1}`)); err != nil {
		t.Fatalf("valid neighbour: %v", err)
	}
	if _, err := strictJSON([]byte{'"', 0xff, '"'}); err == nil || err.Error() != "matrix: response is not UTF-8" {
		t.Fatalf("utf8 message changed: %v", err)
	}
}

// Proves the envelope fetch always carries a deadline: the default one when the
// caller set none, and the caller's own deadline unchanged when it did.
func TestEnvelopeEventDeadline(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		var sawDeadline bool
		var got time.Time
		body := verificationResponse(envelopeBase)
		c := New("https://fixture.invalid", "fixture-token")
		c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
			got, sawDeadline = r.Context().Deadline()
			return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		ctx := context.Background()
		want := time.Now().Add(time.Hour)
		if callerDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, want)
			defer cancel()
		}
		if _, err := c.GetEnvelopeEvent(ctx, "!selected:fixture", "$selected"); err != nil {
			t.Fatal(err)
		}
		if !sawDeadline {
			t.Fatalf("envelope fetch has no deadline (caller deadline %v)", callerDeadline)
		}
		if callerDeadline && !got.Equal(want) {
			t.Fatalf("caller deadline replaced: got %v want %v", got, want)
		}
		if !callerDeadline && time.Until(got) > DefaultRequestTimeout {
			t.Fatalf("default deadline too far: %v", time.Until(got))
		}
	}
}

// Proves only a 200 response is an observation: 201, 203 and 206 with the same
// strictly valid body refuse as response_invalid, while 200 projects.
func TestEnvelopeEventRequiresOK(t *testing.T) {
	for _, status := range []int{200, 201, 203, 206} {
		body := verificationResponse(envelopeBase)
		c := New("https://fixture.invalid", "fixture-token")
		c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		_, err := c.GetEnvelopeEvent(context.Background(), "!selected:fixture", "$selected")
		if status == 200 {
			if err != nil {
				t.Fatalf("status 200 refused: %v", err)
			}
			continue
		}
		var r *EnvelopeRefusal
		if !errors.As(err, &r) || r.Code != RefuseResponseInvalid {
			t.Fatalf("status %d: want %s, got %v", status, RefuseResponseInvalid, err)
		}
	}
}
