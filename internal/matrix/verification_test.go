package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type verificationTransport func(*http.Request) (*http.Response, error)

func (f verificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const verificationContent = `{ "msgtype":"m.text", "body":"a\r\nb\n\u0063" }`

func verificationResponse(content string) string {
	return `{"event_id":"$selected","sender":"@Historical:foreign.invalid","type":"m.room.message","room_id":"!selected:fixture","content":` + content + `}`
}

func verificationClient(body string) (*Client, *int) {
	calls := new(int)
	c := New("https://fixture.invalid", "fixture-token")
	c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
		*calls++
		return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return c, calls
}

// Proves exact returned content spelling and full sender survive this path,
// with absent room kept distinct; this does not verify application authority.
func TestVerificationEventExactObservation(t *testing.T) {
	for _, absent := range []bool{false, true} {
		body := verificationResponse(verificationContent)
		if absent {
			body = strings.ReplaceAll(body, `"room_id":"!selected:fixture",`, "")
		}
		c, calls := verificationClient(body)
		guards := 0
		c.BeforeRequest = func() error { guards++; return nil }
		v, err := c.GetVerificationEvent(context.Background(), "!selected:fixture", "$selected")
		if err != nil {
			t.Fatal(err)
		}
		if *calls != 1 || guards != 1 || v.EventID != "$selected" || v.Sender != "@Historical:foreign.invalid" || v.Type != "m.room.message" || string(v.Content) != verificationContent || v.RoomIDPresent == absent {
			t.Fatalf("incorrect observation: %+v calls=%d guards=%d", v, *calls, guards)
		}
		if absent && v.RoomID != "" {
			t.Fatal("synthesized returned room")
		}
	}
}

// Proves malformed, ambiguous and context-mismatched responses return no usable
// value, paired with the valid observations above. No real server is contacted.
func TestVerificationEventRefusals(t *testing.T) {
	base := verificationResponse(verificationContent)
	cases := map[string]string{
		"event-mismatch":         strings.Replace(base, "$selected", "$other", 1),
		"room-mismatch":          strings.Replace(base, "!selected:fixture", "!other:fixture", 1),
		"room-empty":             strings.Replace(base, `"room_id":"!selected:fixture"`, `"room_id":""`, 1),
		"room-null":              strings.Replace(base, `"room_id":"!selected:fixture"`, `"room_id":null`, 1),
		"room-number":            strings.Replace(base, `"room_id":"!selected:fixture"`, `"room_id":42`, 1),
		"event-absent":           strings.Replace(base, `"event_id":"$selected",`, "", 1),
		"sender-short":           strings.Replace(base, "@Historical:foreign.invalid", "Historical", 1),
		"sender-control":         strings.Replace(base, "@Historical:foreign.invalid", `@His\ntorical:foreign.invalid`, 1),
		"wrong-event-type":       strings.Replace(base, "m.room.message", "m.room.encrypted", 1),
		"top-array":              "[" + base + "]",
		"trailing-document":      base + "{}",
		"trailing-junk":          base + "x",
		"duplicate":              strings.Replace(base, `"event_id":`, `"event_id":"$decoy","event_id":`, 1),
		"escaped-duplicate":      strings.Replace(base, `"event_id":`, `"event_\u0069d":"$decoy","event_id":`, 1),
		"case-alias":             strings.Replace(base, `"event_id":`, `"EVENT_ID":"$decoy","event_id":`, 1),
		"case-only":              strings.Replace(base, `"event_id":`, `"Event_ID":`, 1),
		"unknown-case-collision": strings.Replace(base, `{`, `{"extension":1,"EXTENSION":2,`, 1),
		"content-duplicate":      verificationResponse(`{"body":"a","\u0062ody":"b"}`),
		"empty-redaction":        verificationResponse(`{}`),
		"null-content":           verificationResponse(`null`),
		"array-content":          verificationResponse(`[]`),
		"missing-content":        strings.Replace(base, `,"content":`+verificationContent, "", 1),
		"invalid-utf8":           verificationResponse("{\"body\":\"" + string([]byte{0xff}) + "\"}"),
		"lone-surrogate":         verificationResponse(`{"body":"\ud800"}`),
		"redacted-with-content":  strings.TrimSuffix(base, "}") + `,"unsigned":{"redacted_because":{"type":"m.room.redaction"}}}`,
		"redacted-null-marker":   strings.TrimSuffix(base, "}") + `,"unsigned":{"redacted_because":null}}`,
		"redacted-case-variant":  strings.TrimSuffix(base, "}") + `,"unsigned":{"Redacted_Because":{}}}`,
		"unsigned-not-object":    strings.TrimSuffix(base, "}") + `,"unsigned":"redacted"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, calls := verificationClient(body)
			v, err := c.GetVerificationEvent(context.Background(), "!selected:fixture", "$selected")
			if err == nil || v.EventID != "" || len(v.Content) != 0 || *calls != 1 {
				t.Fatalf("accepted invalid response (%s), value=%+v calls=%d error=%v", name, v, *calls, err)
			}
			if strings.Contains(err.Error(), "Historical") || strings.Contains(err.Error(), "decoy") {
				t.Fatal("response data escaped in diagnostic")
			}
		})
	}
}

// Proves inclusive content/entity caps and their neighboring refusals; unknown
// extensions consume entity budget even though they are never projected.
func TestVerificationEventBounds(t *testing.T) {
	content := `{"body":"` + strings.Repeat("x", (64<<10)-len(`{"body":""}`)) + `"}`
	if len(content) != 64<<10 {
		t.Fatal("fixture size")
	}
	base := verificationResponse(content)
	whole := strings.TrimSuffix(base, "}") + `,"extension":"` + strings.Repeat("x", (1<<20)-len(base)-len(`,"extension":""`)) + `"}`
	if len(whole) != 1<<20 {
		t.Fatalf("entity fixture size %d", len(whole))
	}
	for _, tc := range []struct {
		name, body string
		good       bool
	}{
		{"content-limit", base, true}, {"content-plus-one", verificationResponse(strings.Replace(content, "x", "xx", 1)), false},
		{"entity-limit", whole, true}, {"entity-plus-one", whole + " ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := verificationClient(tc.body)
			_, err := c.GetVerificationEvent(context.Background(), "!selected:fixture", "$selected")
			if (err == nil) != tc.good {
				t.Fatalf("good=%v err=%v", tc.good, err)
			}
		})
	}
}

// Proves original-method/URL provenance, binding guard and complete I/O gates
// cannot be replaced by a successful-looking JSON body or a redirect chain.
func TestVerificationEventTransport(t *testing.T) {
	for _, kind := range []string{"valid", "missing-request", "wrong-url", "wrong-method", "redirect", "cross-origin", "bad-length", "close-error", "read-error", "cancel", "binding"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := verificationResponse(verificationContent)
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
			v, err := c.GetVerificationEvent(ctx, "!selected:fixture", "$selected")
			if kind == "valid" {
				if err != nil || v.EventID != "$selected" {
					t.Fatalf("positive %v", err)
				}
				return
			}
			if err == nil || v.EventID != "" || calls > 1 {
				t.Fatalf("invalid transport accepted: %+v %v calls=%d", v, err, calls)
			}
		})
	}
}

type verificationBadBody struct{ io.Reader }

func (verificationBadBody) Close() error { return errors.New("close failed") }

type verificationReadError struct{}

func (verificationReadError) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (verificationReadError) Close() error             { return nil }

// Proves invalid identifiers are refused before any transport, rather than
// being repaired/resolved into another selected event.
func TestVerificationEventInvalidSelection(t *testing.T) {
	for _, ids := range [][2]string{{"#alias:fixture", "$selected"}, {"!", "$selected"}, {"!selected:fixture", "$"}, {"!selected:fixture", "$" + strings.Repeat("x", 255)}} {
		c, calls := verificationClient(verificationResponse(verificationContent))
		_, err := c.GetVerificationEvent(context.Background(), ids[0], ids[1])
		if err == nil || *calls != 0 {
			t.Fatalf("selection reached network: %v calls=%d", err, *calls)
		}
	}
}

// Proves equal localparts on different servers and historical case spellings
// remain distinct observations, rather than collapsing to one display label.
func TestVerificationEventFullIdentityCollisions(t *testing.T) {
	for _, sender := range []string{"@Same:alpha.invalid", "@Same:beta.invalid", "@same:alpha.invalid"} {
		body := strings.Replace(verificationResponse(verificationContent), "@Historical:foreign.invalid", sender, 1)
		c, _ := verificationClient(body)
		v, err := c.GetVerificationEvent(context.Background(), "!selected:fixture", "$selected")
		if err != nil || v.Sender != sender {
			t.Fatalf("identity collapsed: got%q want%q err%v", v.Sender, sender, err)
		}
	}
}

// Proves enabling verification on a scoped copy does not change ordinary
// callers' configured same-origin redirect compatibility.
func TestVerificationEventRedirectPolicyUnchanged(t *testing.T) {
	c := New("https://fixture.invalid", "fixture-token")
	calls := 0
	c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/target" {
			return &http.Response{StatusCode: 302, Request: r, Header: http.Header{"Location": []string{"/target"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Request: r, Body: io.NopCloser(strings.NewReader(verificationResponse(verificationContent)))}, nil
	})}
	if _, err := c.GetVerificationEvent(context.Background(), "!selected:fixture", "$selected"); err == nil || calls != 1 {
		t.Fatal("verification followed redirect")
	}
	if _, err := c.GetEvent(context.Background(), "!selected:fixture", "$selected"); err != nil || calls != 3 {
		t.Fatalf("ordinary redirect behavior changed: %v calls%d", err, calls)
	}
}

// Proves an unrelated unsigned field keeps the observation valid, so the
// redaction refusal above is specific to the redaction marker.
func TestVerificationEventUnsignedWithoutRedaction(t *testing.T) {
	body := strings.TrimSuffix(verificationResponse(verificationContent), "}") + `,"unsigned":{"age":12}}`
	c, _ := verificationClient(body)
	v, err := c.GetVerificationEvent(context.Background(), "!selected:fixture", "$selected")
	if err != nil || string(v.Content) != verificationContent {
		t.Fatalf("unrelated unsigned field refused: value=%+v err=%v", v, err)
	}
}

// Proves the fetch carries a deadline when the caller supplied none, and keeps
// a caller deadline; it observes the request context, not wall-clock waiting.
func TestVerificationEventDeadline(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		var sawDeadline bool
		var got time.Time
		body := verificationResponse(verificationContent)
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
		if _, err := c.GetVerificationEvent(ctx, "!selected:fixture", "$selected"); err != nil {
			t.Fatal(err)
		}
		if !sawDeadline {
			t.Fatalf("verification fetch has no deadline (caller deadline %v)", callerDeadline)
		}
		if callerDeadline && !got.Equal(want) {
			t.Fatalf("caller deadline replaced: got %v want %v", got, want)
		}
		if !callerDeadline && time.Until(got) > DefaultRequestTimeout {
			t.Fatalf("default deadline too far: %v", time.Until(got))
		}
	}
}

// Proves only a 200 response is an observation; other 2xx statuses with a
// valid-looking body are refused.
func TestVerificationEventRequiresOK(t *testing.T) {
	for _, status := range []int{200, 201, 203, 206} {
		body := verificationResponse(verificationContent)
		c := New("https://fixture.invalid", "fixture-token")
		c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		_, err := c.GetVerificationEvent(context.Background(), "!selected:fixture", "$selected")
		if (err == nil) != (status == 200) {
			t.Fatalf("status %d: err=%v", status, err)
		}
	}
}
