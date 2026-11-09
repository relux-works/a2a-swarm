package matrixtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	tr "github.com/relux-works/a2a-swarm/transport"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(r *http.Request, status int, b string) *http.Response {
	return &http.Response{Request: r, StatusCode: status, Body: io.NopCloser(strings.NewReader(b)), ContentLength: int64(len(b)), Header: make(http.Header)}
}
func testTransport(t *testing.T, rt roundTrip) *Transport {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	c := matrix.New("http://matrix.invalid", "test-token")
	c.HTTP = &http.Client{Transport: rt}
	a, e := New(c, tr.Config{StateDir: filepath.Join(root, "state")})
	if e != nil {
		t.Fatal(e)
	}
	return a
}

// Proves the known SHA-256/base64url vector and key separation, not remote idempotency.
func TestTxnID(t *testing.T) {
	if got := TxnID("hello"); got != "a2a-k-LPJNul-wow4m6DsqxbninhsWHlwfp0Je" {
		t.Fatalf("known vector: %s", got)
	}
	if TxnID("hello") != TxnID("hello") || TxnID("hello") == TxnID("other") {
		t.Fatal("unstable/colliding txn")
	}
}

// Proves closed refusal classification including provenance; the HTTP tests below exercise wire mapping.
func TestOutcomeTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want tr.Outcome
	}{
		{"definite", &matrix.Error{Status: 400, ErrCode: "M_FORBIDDEN", OriginalRequest: true}, tr.REFUSED},
		{"408", &matrix.Error{Status: 408, ErrCode: "M_FORBIDDEN", OriginalRequest: true}, tr.UNKNOWN},
		{"429", &matrix.Error{Status: 429, ErrCode: "M_LIMIT_EXCEEDED", OriginalRequest: true}, tr.UNKNOWN},
		{"502", &matrix.Error{Status: 502, ErrCode: "M_FORBIDDEN", OriginalRequest: true}, tr.UNKNOWN},
		{"timeout", context.DeadlineExceeded, tr.UNKNOWN},
		{"reset", syscall.ECONNRESET, tr.UNKNOWN},
		{"redirect", &matrix.Error{Status: 403, ErrCode: "M_FORBIDDEN"}, tr.UNKNOWN},
		{"unclosed", &matrix.Error{Status: 400, ErrCode: "received-SECRET-marker", OriginalRequest: true}, tr.UNKNOWN},
		{"empty_ack", nil, tr.UNKNOWN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := outcome("", tc.err)
			if r.Outcome != tc.want || r.Event != "" || (r.Outcome == tr.REFUSED) != r.Refusal.Valid() || r.Outcome != tr.REFUSED && r.Refusal != (tr.Refusal{}) {
				t.Fatalf("outcome=%+v", r)
			}
		})
	}
	if r := outcome("$receipt", nil); r.Outcome != tr.ACK || r.Event != "$receipt" {
		t.Fatal("receipt not ACK")
	}
}

// Proves after-write wire failures are results, validation/cancellation are pre-I/O errors,
// and the exact serialized bound is inclusive. It does not contact a homeserver.
func TestSendWireAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		err    error
		want   tr.Outcome
	}{
		{"ack", 200, nil, tr.ACK}, {"400", 400, nil, tr.REFUSED}, {"408", 408, nil, tr.UNKNOWN}, {"429", 429, nil, tr.UNKNOWN}, {"502", 502, nil, tr.UNKNOWN}, {"timeout", 0, context.DeadlineExceeded, tr.UNKNOWN}, {"reset_after_write", 0, syscall.ECONNRESET, tr.UNKNOWN}, {"redirect", 307, nil, tr.UNKNOWN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			a := testTransport(t, func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				body, e := io.ReadAll(r.Body)
				if e != nil || !bytes.Contains(body, []byte(`"body":"valid"`)) {
					t.Error("request content missing")
				}
				if !strings.HasSuffix(r.URL.Path, TxnID("key")) {
					t.Error("txn changed")
				}
				if tc.err != nil {
					return nil, tc.err
				}
				if tc.status == 200 {
					return response(r, 200, `{"event_id":"$ack"}`), nil
				}
				resp := response(r, tc.status, `{"errcode":"M_FORBIDDEN","error":"received-SECRET-marker"}`)
				if tc.status == 307 {
					resp.Header.Set("Location", "http://elsewhere.invalid/capture")
				}
				return resp, nil
			})
			r, e := a.Send(context.Background(), tr.SendRequest{Conversation: "!room:server", Message: tr.Message{Text: "valid"}, IdempotencyKey: "key"})
			if e != nil || r.Outcome != tc.want || calls.Load() != 1 {
				t.Fatalf("send=%+v error=%v calls=%d", r, e, calls.Load())
			}
		})
	}
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(r, 200, `{"event_id":"$ack"}`), nil
	})
	for _, n := range []int{MaxContentBytes, MaxContentBytes + 1} {
		m := messageAtSize(n)
		b, e := Content(m)
		if e != nil || len(b) != n {
			t.Fatal("bad boundary fixture")
		}
		before := calls.Load()
		r, e := a.Send(context.Background(), tr.SendRequest{Conversation: "!room:server", IdempotencyKey: fmt.Sprint(n), Message: m})
		if n == MaxContentBytes {
			if e != nil || r.Outcome != tr.ACK || calls.Load() != before+1 {
				t.Fatal("exact bound refused")
			}
		} else if tr.Code(e) != tr.ContentTooLarge || calls.Load() != before {
			t.Fatal("oversize content must be refused with zero wire calls")
		}
	}
	before := calls.Load()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := a.Send(cancelled, tr.SendRequest{Conversation: "!room:server", IdempotencyKey: "cancel"})
	if tr.Code(e) != tr.Unavailable || calls.Load() != before {
		t.Fatal("cancelled Send crossed I/O")
	}
	for _, r := range []tr.SendRequest{{Conversation: "!short", IdempotencyKey: "key"}, {Conversation: "!room:server"}, {Conversation: "!room:server", IdempotencyKey: "key", Message: tr.Message{Re: "invalid"}}, {Conversation: "!room:server", IdempotencyKey: "key", Message: tr.Message{Extensions: map[string]json.RawMessage{"bad": json.RawMessage(`!`)}}}} {
		_, e := a.Send(context.Background(), r)
		if tr.Code(e) != tr.InvalidReference || calls.Load() != before {
			t.Fatal("invalid Send crossed I/O")
		}
	}
}
func messageAtSize(n int) tr.Message {
	m := tr.Message{}
	b, _ := Content(m)
	m.Text = strings.Repeat("x", n-len(b))
	return m
}

// Proves canonical forms and invalid neighbors are decided before I/O; alias resolution
// additionally requires a canonical server result. It does not widen T1 to short names.
func TestAddressing(t *testing.T) {
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(r, 200, `{"room_id":"!resolved:server"}`), nil
	})
	for _, s := range []string{"@a:server", "@a/b+c:example.org:00000", "@Mixed:server:8448", "@unicode-ø:[::1]:443"} {
		id, e := a.Addressing().Principal(s)
		if e != nil || string(id) != s || a.Addressing().Format(id) != s {
			t.Fatalf("valid principal refused %q", s)
		}
	}
	for _, s := range []string{"@a", "a:server", "@:server", "@a:", "@a:bad/server", "@a:[invalid]", "@a:s:bad", "@a\n:server"} {
		if _, e := a.Addressing().Principal(s); tr.Code(e) != tr.InvalidReference {
			t.Fatalf("invalid principal accepted %q", s)
		}
	}
	for _, s := range []string{"!id:server", "!id:[::1]"} {
		if _, e := a.Addressing().Conversation(context.Background(), s); e != nil {
			t.Fatal(e)
		}
	}
	for _, s := range []string{"#short", "!short", "#a:bad/server"} {
		if _, e := a.Addressing().Conversation(context.Background(), s); tr.Code(e) != tr.InvalidReference {
			t.Fatal("short/invalid accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("grammar validation made request")
	}
	if id, e := a.Addressing().Conversation(context.Background(), "#alias:server"); e != nil || id != "!resolved:server" || calls.Load() != 1 {
		t.Fatal("alias did not resolve")
	}
}

// Proves sync cursor validation, limited gaps, invites, bounded projections and
// dropping a forged sender. Raw is a bounded re-encoding of the wire client's event.
func TestSyncProjection(t *testing.T) {
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("since") != "token" || r.URL.Query().Get("filter") != SyncFilter {
			t.Error("sync request drift")
		}
		return response(r, 200, `{"next_batch":"next","rooms":{"join":{"!r:s":{"timeline":{"limited":true,"events":[{"event_id":"$ok","sender":"@alice:s","type":"m.room.message","content":{"msgtype":"m.text","body":"valid"}},{"event_id":"$bad","sender":"forged","type":"m.room.message","content":{"body":"secret"}}]}}},"invite":{"!i:s":{"invite_state":{"events":[{"type":"m.room.member","sender":"@alice:s","content":{"membership":"invite"}}]}}}}}`), nil
	})
	for _, bad := range []tr.Cursor{nil, {0xff}} {
		if _, e := a.Sync(context.Background(), bad, tr.SyncOptions{}); tr.Code(e) != tr.InvalidCursor {
			t.Fatal("invalid cursor accepted")
		}
	}
	b, e := a.Sync(context.Background(), tr.Cursor("token"), tr.SyncOptions{})
	if e != nil || len(b.Events) != 1 || b.Dropped != 1 || len(b.Gaps) != 1 || b.Gaps[0].Reason != tr.TimelineLimited || len(b.Invites) != 1 || string(b.Next) != "next" {
		t.Fatalf("sync=%+v err=%v", b, e)
	}
	ev := matrix.Event{EventID: "$id", Sender: "@a:s", Content: json.RawMessage(`{"body":"ok"}`)}
	if _, ok := project("!r:s", ev); !ok {
		t.Fatal("valid event refused")
	}
	ev.Content = json.RawMessage(`{"body":"` + strings.Repeat("x", MaxContentBytes) + `"}`)
	if _, ok := project("!r:s", ev); ok {
		t.Fatal("oversize event accepted")
	}
	ev.Content = json.RawMessage(`{}`)
	ev.Unsigned = json.RawMessage(`{"x":"` + strings.Repeat("x", MaxRawBytes) + `"}`)
	if _, ok := project("!r:s", ev); ok {
		t.Fatal("oversize raw accepted")
	}
}

// Proves constructor custody and optional-interface shape, not filesystem access control against a malicious OS.
func TestConfigAndExtensions(t *testing.T) {
	a := testTransport(t, func(r *http.Request) (*http.Response, error) { return response(r, 200, `{"event_id":"$state"}`), nil })
	if _, ok := any(a).(tr.Registration); ok {
		t.Fatal("unsupported registration exposed")
	}
	if _, ok := any(a).(tr.Rooms); ok {
		t.Fatal("unsupported rooms exposed")
	}
	if _, ok := any(a).(tr.AccountData); ok {
		t.Fatal("unsupported account data exposed")
	}
	if _, ok := any(a).(tr.Presence); ok {
		t.Fatal("unsupported presence exposed")
	}
	if _, ok := tr.Lookup("matrix"); !ok {
		t.Fatal("matrix not registered")
	}
	if _, e := New(nil, tr.Config{}); tr.Code(e) != tr.InvalidReference {
		t.Fatal("nil client accepted")
	}
	if _, e := New(matrix.New("http://matrix.invalid", ""), tr.Config{}); tr.Code(e) != tr.InvalidReference {
		t.Fatal("missing StateDir accepted")
	}
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	sentinel := filepath.Join(root, "sentinel")
	if e = os.Mkdir(sentinel, 0755); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(root, "link")
	if e = os.Symlink(sentinel, link); e != nil {
		t.Fatal(e)
	}
	if _, e = New(matrix.New("http://matrix.invalid", ""), tr.Config{StateDir: link}); tr.Code(e) != tr.InvalidReference {
		t.Fatal("StateDir symlink accepted")
	}
	i, e := os.Stat(sentinel)
	if e != nil || i.Mode().Perm() != 0755 {
		t.Fatal("sentinel mode changed")
	}
	if safe(errors.New("received-SECRET-marker")).Error() != "transport: unavailable" {
		t.Fatal("read error leaked")
	}
}

// Proves exact raw JSON retention, an inclusive event bound, and total response
// bounding before decode; unknown fields and whitespace survive valid reads.
func TestRawAndReadBounds(t *testing.T) {
	raw := `{ "event_id":"$id", "type":"m.room.message", "sender":"@a:s", "content":{"body":"valid"}, "future":{"value":42} }`
	a := testTransport(t, func(r *http.Request) (*http.Response, error) { return response(r, 200, raw), nil })
	e, err := a.Event(context.Background(), "!r:s", "$id")
	if err != nil || string(e.Raw) != raw {
		t.Fatalf("raw changed: %s error %v", e.Raw, err)
	}
	var base matrix.Event
	if e := json.Unmarshal([]byte(raw), &base); e != nil {
		t.Fatal(e)
	}
	padded := raw + strings.Repeat(" ", MaxRawBytes-len(raw))
	if _, ok := projectRaw("!r:s", base, []byte(padded)); !ok {
		t.Fatal("exact Raw bound refused")
	}
	if _, ok := projectRaw("!r:s", base, []byte(padded+" ")); ok {
		t.Fatal("oversize Raw accepted")
	}
	for _, n := range []int{MaxResponseBytes, MaxResponseBytes + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			body := `{"value":"` + strings.Repeat("x", n-len(`{"value":""}`)) + `"}`
			a := testTransport(t, func(r *http.Request) (*http.Response, error) { return response(r, 200, body), nil })
			_, e := a.GetState(context.Background(), "!r:s", "test.state", "")
			if n == MaxResponseBytes && e != nil {
				t.Fatal("exact response bound refused")
			}
			if n > MaxResponseBytes && tr.Code(e) != tr.Unavailable {
				t.Fatal("oversize response accepted")
			}
		})
	}
}

// Proves empty filtered pages retain a usable continuation and repeated tokens
// refuse rather than silently claim a complete history. No remote durability is implied.
func TestHistoryContinuation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		fail       bool
	}{{"empty_next", `{"chunk":[],"end":"next"}`, false}, {"repeat", `{"chunk":[],"end":"page"}`, true}, {"done", `{"chunk":[]}`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			a := testTransport(t, func(r *http.Request) (*http.Response, error) { return response(r, 200, tc.body), nil })
			p, e := a.History(context.Background(), "!r:s", []byte("page"), 5)
			if tc.fail {
				if tr.Code(e) != tr.InvalidCursor {
					t.Fatal("repeated history cursor accepted")
				}
			} else if e != nil {
				t.Fatal(e)
			} else if tc.name == "empty_next" && string(p.Next) != "next" {
				t.Fatal("empty page continuation lost")
			}
		})
	}
}

// Proves media preflight, error redaction, and bounded partial output on oversize;
// a valid adjacent download preserves the SHA-256 and bytes.
func TestDownloadBounds(t *testing.T) {
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/media/config") {
			return response(r, 200, `{"m.upload.size":4}`), nil
		}
		return response(r, 200, "1234"), nil
	})
	var out bytes.Buffer
	for _, limit := range []int64{4, 3} {
		before := calls.Load()
		meta, e := a.Download(context.Background(), tr.NewMediaRef("mxc://server/media"), &out, limit)
		if calls.Load() != before+2 {
			t.Fatal("download request absent")
		}
		if limit == 4 {
			if e != nil || meta.Size != 4 || out.String() != "1234" || meta.SHA256 != "03ac674216f3e15c761ee1a5e255f067953623c8b388b4459e13f978d7c846f4" {
				t.Fatal("valid media changed")
			}
			out.Reset()
		} else if tr.Code(e) != tr.ContentTooLarge || out.String() != "123" {
			t.Fatal("oversize media did not preserve the bounded prefix")
		}
	}
	before := calls.Load()
	_, e := a.Download(context.Background(), tr.NewMediaRef("received-SECRET-marker"), &out, 4)
	if tr.Code(e) != tr.InvalidReference || strings.Contains(e.Error(), "received-SECRET-marker") || calls.Load() != before {
		t.Fatal("invalid media crossed I/O or leaked")
	}
}

// Proves two concurrent callers derive one deterministic transaction while the
// wrapper retains the supplied HTTP client; race detection covers local access.
func TestConcurrentSends(t *testing.T) {
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if !strings.HasSuffix(r.URL.Path, TxnID("shared")) {
			return nil, errors.New("wrong txn")
		}
		return response(r, 200, `{"event_id":"$same"}`), nil
	})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, e := a.Send(context.Background(), tr.SendRequest{Conversation: "!r:s", IdempotencyKey: "shared", Message: tr.Message{Text: "valid"}})
			if e == nil && (r.Outcome != tr.ACK || r.Event != "$same") {
				e = errors.New("concurrent result changed")
			}
			done <- e
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	for i := 0; i < 2; i++ {
		select {
		case e := <-done:
			if e != nil {
				t.Fatal(e)
			}
		case <-ctx.Done():
			t.Fatal("concurrent send did not finish")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected wire attempts")
	}
}

// Proves recorded historical Matrix sender forms remain valid next to malformed
// server authorities. This checks receive grammar, not account registration policy.
func TestReceivedPrincipalCompatibility(t *testing.T) {
	for _, s := range []string{"@:example.org", "@A b\t\n\r\x1b\x7f界:EXAMPLE.Org", "@a/b+c:example.org:00000", "@x:127.000.0.1", "@x:[2001:db8::1]", "@x:[::ffff:192.0.2.1]:99999", "@x:a..b", "@" + strings.Repeat("é", 126) + ":s"} {
		if !receivedPrincipal(s) {
			t.Fatalf("legitimate received ID refused: %q", s)
		}
	}
	for _, s := range []string{"forged", "@x", "@x:", "@x\x00:s", "@\xff:s", "@x:example.org/path", "@x:example.org?x=y", "@x:example.org@evil", "@x:exa_mple.org", "@x:[::1%en0]", "@x:[127.0.0.1]", "@x:[2001:::1]", "@" + strings.Repeat("é", 126) + "x:s"} {
		if receivedPrincipal(s) {
			t.Fatalf("malformed received ID accepted: %q", s)
		}
	}
}

// Proves extension admission uses the current wire allowlist and does not permit
// callers to replace transport-owned fields. Formatted text is caller-provided.
func TestContentExtensionAdmission(t *testing.T) {
	m := tr.Message{Text: "valid", Extensions: map[string]json.RawMessage{"format": json.RawMessage(`"org.matrix.custom.html"`), "formatted_body": json.RawMessage(`"<p>valid</p>"`)}}
	if _, e := Content(m); e != nil {
		t.Fatal("valid extension refused")
	}
	for _, key := range []string{"body", "msgtype", "m.relates_to", "m.mentions", "unknown.vendor"} {
		if _, e := Content(tr.Message{Text: "valid", Extensions: map[string]json.RawMessage{key: json.RawMessage(`{}`)}}); tr.Code(e) != tr.InvalidReference {
			t.Fatalf("unadmitted extension accepted: %s", key)
		}
	}
}

// Proves wrapper routing and current-member authority against the wire client's
// real decoders. These deterministic HTTP responses do not attest a live server.
func TestMembersAndExtensionsWire(t *testing.T) {
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/whoami"):
			return response(r, 200, `{"user_id":"@a:s"}`), nil
		case strings.HasSuffix(r.URL.Path, "/members"):
			return response(r, 200, `{"chunk":[{"type":"m.room.member","room_id":"!r:s","state_key":"@a:s","content":{"membership":"join"}},{"type":"m.room.member","room_id":"!r:s","state_key":"@b:s","content":{"membership":"invite"}}]}`), nil
		case strings.Contains(r.URL.Path, "/relations/"):
			return response(r, 200, `{"chunk":[{"type":"m.room.message","room_id":"!r:s","event_id":"$reply","sender":"@a:s","content":{"body":"reply","m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}}]}`), nil
		case strings.Contains(r.URL.Path, "/redact/"):
			if !strings.HasSuffix(r.URL.Path, TxnID("redact")) {
				t.Error("redaction txn changed")
			}
			return response(r, 200, `{"event_id":"$redact"}`), nil
		case r.Method == http.MethodGet:
			return response(r, 200, `{"value":"state"}`), nil
		default:
			return response(r, 200, `{"event_id":"$state"}`), nil
		}
	})
	c := context.Background()
	members, e := a.Members(c, "!r:s")
	if e != nil || len(members) != 2 || members[0].Principal != "@a:s" || members[1].Membership != "invite" {
		t.Fatalf("members=%+v err=%v", members, e)
	}
	p, e := a.ThreadHistory(c, "!r:s", "$root", nil, 10)
	if e != nil || len(p.Events) != 1 || p.Events[0].Message.Thread != "$root" {
		t.Fatalf("thread page=%+v err=%v", p, e)
	}
	raw, e := a.GetState(c, "!r:s", "test.state", "")
	if e != nil || string(raw) != `{"value":"state"}` {
		t.Fatal("state read changed")
	}
	id, e := a.PutState(c, "!r:s", "test.state", "", json.RawMessage(`{}`))
	if e != nil || id != "$state" {
		t.Fatal("state write changed")
	}
	ack, e := a.Redact(c, "!r:s", "$event", "redact")
	if e != nil || ack.Outcome != tr.ACK || ack.Event != "$redact" {
		t.Fatal("redaction result changed")
	}
	before := calls.Load()
	if _, e = a.Members(c, "!short"); tr.Code(e) != tr.InvalidReference {
		t.Fatal("invalid member conversation accepted")
	}
	if _, e = a.ThreadHistory(c, "!r:s", "bad", nil, 1); tr.Code(e) != tr.InvalidReference {
		t.Fatal("invalid thread accepted")
	}
	if _, e = a.PutState(c, "!r:s", "test.state", "", json.RawMessage(`!`)); tr.Code(e) != tr.InvalidReference {
		t.Fatal("invalid state accepted")
	}
	if _, e = a.Redact(c, "!r:s", "bad", "redact"); tr.Code(e) != tr.InvalidReference {
		t.Fatal("invalid redaction accepted")
	}
	if calls.Load() != before {
		t.Fatal("invalid wrapper input crossed I/O")
	}
}

// Proves upload size admission and that the existing media client receives the
// declared body and metadata. Oversize upload is rejected beside a valid one.
func TestUploadWire(t *testing.T) {
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/media/config") {
			return response(r, 200, `{"m.upload.size":4}`), nil
		}
		b, e := io.ReadAll(r.Body)
		if e != nil || string(b) != "data" || r.Header.Get("Content-Type") != "text/plain" || r.URL.Query().Get("filename") != "name" {
			t.Error("upload body or metadata changed")
		}
		return response(r, 200, `{"content_uri":"mxc://server/media"}`), nil
	})
	ref, e := a.Upload(context.Background(), strings.NewReader("data"), tr.MediaMeta{Name: "name", ContentType: "text/plain", Size: 4})
	if e != nil || ref.Opaque() != "mxc://server/media" || calls.Load() != 2 {
		t.Fatal("valid upload refused")
	}
	for _, size := range []int64{0, MaxMediaBytes + 1} {
		if _, e = a.Upload(context.Background(), strings.NewReader("data"), tr.MediaMeta{Size: size}); tr.Code(e) != tr.ContentTooLarge || calls.Load() != 2 {
			t.Fatal("invalid upload crossed I/O")
		}
	}
	if _, e = a.Upload(context.Background(), strings.NewReader("data!"), tr.MediaMeta{Size: 5}); tr.Code(e) != tr.ContentTooLarge || calls.Load() != 3 {
		t.Fatal("server-limit overflow reached upload")
	}
}

// Proves a too-large ACK is uncertain AFTER the one submission, while an adjacent
// bounded receipt remains ACK; transport response limits are not preflight failures.
func TestACKResponseBound(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			var calls atomic.Int64
			base := `{"event_id":"$ack","padding":""}`
			body := strings.Replace(base, `"padding":""`, `"padding":"`+strings.Repeat("x", MaxResponseBytes-len(base)+extra)+`"`, 1)
			a := testTransport(t, func(r *http.Request) (*http.Response, error) { calls.Add(1); return response(r, 200, body), nil })
			r, e := a.Send(context.Background(), tr.SendRequest{Conversation: "!r:s", IdempotencyKey: "response", Message: tr.Message{Text: "valid"}})
			want := tr.ACK
			if extra == 1 {
				want = tr.UNKNOWN
			}
			if e != nil || r.Outcome != want || calls.Load() != 1 {
				t.Fatalf("response bound result=%+v err=%v calls=%d", r, e, calls.Load())
			}
		})
	}
}

// Proves valid opaque receipt IDs retain the existing client contract, including
// historical characters; malformed adjacent references are refused before I/O.
func TestEventIDCompatibility(t *testing.T) {
	for _, id := range []string{"$opaque_-+/=", "$historical value", "$\t", "$界", "$" + strings.Repeat("x", 254)} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			if !eventID(id) {
				t.Fatal("valid opaque ID refused")
			}
			a := testTransport(t, func(r *http.Request) (*http.Response, error) {
				b, _ := json.Marshal(map[string]string{"event_id": id})
				return response(r, 200, string(b)), nil
			})
			r, e := a.Send(context.Background(), tr.SendRequest{Conversation: "!r:s", IdempotencyKey: "opaque", Message: tr.Message{Text: "valid", Re: tr.EventID(id)}})
			if e != nil || r.Outcome != tr.ACK || string(r.Event) != id {
				t.Fatalf("valid opaque receipt changed: %+v %v", r, e)
			}
		})
	}
	for _, id := range []string{"", "$", "wrong", "$" + strings.Repeat("x", 255), "$\xff"} {
		if eventID(id) {
			t.Fatal("invalid opaque ID accepted")
		}
	}
}

// Proves neutral recipients use canonical mentions instead of claimed display
// names, with a canonical header-only neighbor and invalid mentions filtered.
func TestProjectedRecipients(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          []tr.PrincipalID
	}{
		{"mentions", `{"body":"valid","m.mentions":{"user_ids":["@b:s","forged"]},"ai.relux.a2a":{"to":["display-name"],"kind":"notice"}}`, []tr.PrincipalID{"@b:s"}},
		{"header_canonical", `{"body":"valid","ai.relux.a2a":{"to":["@b:s","display-name"]}}`, []tr.PrincipalID{"@b:s"}},
		{"display_only", `{"body":"valid","ai.relux.a2a":{"to":["display-name"]}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := project("!r:s", matrix.Event{EventID: "$id", Sender: "@a:s", Content: json.RawMessage(tc.content)})
			if !ok || !reflect.DeepEqual(v.Message.To, tc.want) {
				t.Fatalf("projected recipients=%v want %v", v.Message.To, tc.want)
			}
			var wire matrix.MessageContent
			if e := json.Unmarshal([]byte(tc.content), &wire); e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(v.Message.Extensions[ContentKey], wire.A2A) {
				t.Fatal("opaque header changed")
			}

		})
	}
}

// Proves the existing positive server policy and 1 TiB clamp without allocating
// or transferring a huge file; unavailable limits refuse beside valid limits.
func TestMediaLimitPolicy(t *testing.T) {
	for _, n := range []int64{-1, 0, 4, MaxMediaBytes, MaxMediaBytes + 1, 1<<63 - 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			a := testTransport(t, func(r *http.Request) (*http.Response, error) {
				return response(r, 200, fmt.Sprintf(`{"m.upload.size":%d}`, n)), nil
			})
			got, e := a.mediaLimit(context.Background())
			if n <= 0 {
				if tr.Code(e) != tr.Unavailable {
					t.Fatal("unavailable server limit accepted")
				}
			} else if e != nil || got != min(n, MaxMediaBytes) {
				t.Fatalf("limit=%d error=%v", got, e)
			}
		})
	}
}

// Proves a marker in a syntactically valid opaque reference cannot escape through
// an after-request error, beside a successful download of the same reference.
func TestMediaReferenceIOErrorRedaction(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			var calls atomic.Int64
			a := testTransport(t, func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if strings.HasSuffix(r.URL.Path, "/media/config") {
					return response(r, 200, `{"m.upload.size":4}`), nil
				}
				if fail {
					return nil, errors.New("received-SECRET-marker")
				}
				return response(r, 200, "data"), nil
			})
			var out bytes.Buffer
			_, e := a.Download(context.Background(), tr.NewMediaRef("mxc://server/received-SECRET-marker"), &out, 4)
			if calls.Load() != 2 {
				t.Fatal("valid opaque reference did not reach transfer")
			}
			if fail {
				if tr.Code(e) != tr.Unavailable || strings.Contains(e.Error(), "received-SECRET-marker") || out.Len() != 0 {
					t.Fatalf("media I/O error leaked or misclassified: %v", e)
				}
			} else if e != nil || out.String() != "data" {
				t.Fatal("valid reference neighbor refused")
			}
		})
	}
}

// Proves an opaque UTF-8 server cursor is preserved without an invented length
// restriction, beside non-UTF-8 input which must refuse before any request.
func TestOpaqueCursorCompatibility(t *testing.T) {
	token := strings.Repeat("opaque-", 1200)
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Query().Get("since") != token {
			t.Error("opaque cursor changed")
		}
		return response(r, 200, `{"next_batch":"next","rooms":{}}`), nil
	})
	if _, e := a.Sync(context.Background(), tr.Cursor(token), tr.SyncOptions{}); e != nil || calls.Load() != 1 {
		t.Fatalf("valid opaque cursor refused: %v", e)
	}
	if _, e := a.Sync(context.Background(), tr.Cursor{0xff}, tr.SyncOptions{}); tr.Code(e) != tr.InvalidCursor || calls.Load() != 1 {
		t.Fatal("invalid cursor reached I/O")
	}
}

// Proves a caller's larger page request is capped rather than rejected, while
// invalid nonpositive limits refuse before the existing Messages operation.
func TestHistoryPageBudget(t *testing.T) {
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Query().Get("limit") != fmt.Sprint(MaxBatchEvents) {
			t.Error("page budget not capped")
		}
		return response(r, 200, `{"chunk":[]}`), nil
	})
	if _, e := a.History(context.Background(), "!r:s", nil, MaxBatchEvents+1); e != nil || calls.Load() != 1 {
		t.Fatalf("larger page request refused: %v", e)
	}
	if _, e := a.History(context.Background(), "!r:s", nil, 0); tr.Code(e) != tr.InvalidReference || calls.Load() != 1 {
		t.Fatal("invalid page budget reached I/O")
	}
}

// Proves send redirect refusal is scoped to that operation: a same-origin read
// follows the supplied client's existing policy, while the send has one attempt.
func TestScopedRedirectPolicy(t *testing.T) {
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path == "/read-target" {
			return response(r, 200, `{"event_id":"$id","sender":"@a:s","type":"m.room.message","content":{"body":"valid"}}`), nil
		}
		resp := response(r, 302, `{}`)
		resp.Header.Set("Location", "/read-target")
		return resp, nil
	})
	event, e := a.Event(context.Background(), "!r:s", "$id")
	if e != nil || event.ID != "$id" || calls.Load() != 2 {
		t.Fatalf("read redirect policy changed: %v", e)
	}
	before := calls.Load()
	r, e := a.Send(context.Background(), tr.SendRequest{Conversation: "!r:s", IdempotencyKey: "redirect", Message: tr.Message{Text: "valid"}})
	if e != nil || r.Outcome != tr.UNKNOWN || calls.Load() != before+1 {
		t.Fatalf("send followed redirect: %+v %v calls=%d", r, e, calls.Load()-before)
	}
}

// Proves the reworked neutral payload preserves opaque media references and
// admitted text extensions in projection, beside invalid reference/shape neighbours.
// The CLI conformance fixture separately exercises the real HTTP decoders.
func TestNeutralPayloadRoundTrip(t *testing.T) {
	html, _ := json.Marshal("<p>valid extension</p>")
	m := tr.Message{Text: "valid", Kind: "notice", To: []tr.PrincipalID{"@b:s"}, Re: "$reply", Thread: "$root", Attachments: []tr.MediaRef{tr.NewMediaRef("mxc://server/media")}, Extensions: map[string]json.RawMessage{"format": json.RawMessage(`"org.matrix.custom.html"`), "formatted_body": html}}
	raw, e := Content(m)
	if e != nil {
		t.Fatal(e)
	}
	ev, ok := project("!r:s", matrix.Event{EventID: "$id", Sender: "@a:s", Type: "m.room.message", Content: raw})
	if !ok || ev.Message.Text != m.Text || ev.Message.Kind != m.Kind || ev.Message.Re != m.Re || ev.Message.Thread != m.Thread || !reflect.DeepEqual(ev.Message.Attachments, m.Attachments) || !reflect.DeepEqual(ev.Message.To, m.To) {
		t.Fatal("neutral payload changed")
	}
	for key, value := range m.Extensions {
		if !bytes.Equal(ev.Message.Extensions[key], value) {
			t.Fatal("text extension changed")
		}
	}
	m.Attachments = []tr.MediaRef{tr.NewMediaRef("received-SECRET-marker")}
	if _, e := Content(m); tr.Code(e) != tr.InvalidReference || strings.Contains(e.Error(), "received-SECRET-marker") {
		t.Fatal("invalid attachment accepted or leaked")
	}
	for _, body := range []string{`{"body":"valid","ai.relux.a2a.media":["bad"]}`, `{"body":"valid","ai.relux.a2a.media":{}}`, `{"body":"valid","formatted_body":42}`, `{"body":"valid","formatted_body":null}`, `{"body":"valid","ai.relux.a2a.media":null}`} {
		if _, ok := project("!r:s", matrix.Event{EventID: "$id", Sender: "@a:s", Content: json.RawMessage(body)}); ok {
			t.Fatal("invalid neutral projection accepted")
		}
	}
}

// Proves known versus syntactically valid unknown conversations, plus closed
// optional extensions/addressing without further requests. No live server is used.
func TestConversationCapabilitiesAndClose(t *testing.T) {
	var calls atomic.Int64
	a := testTransport(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.Contains(r.URL.Path, "!unknown:s") {
			return response(r, 404, `{"errcode":"M_NOT_FOUND","error":"received-SECRET-marker"}`), nil
		}
		return response(r, 200, `{"creator":"@a:s"}`), nil
	})
	c := context.Background()
	if _, e := a.ConversationCaps(c, "!known:s"); e != nil {
		t.Fatal(e)
	}
	if _, e := a.ConversationCaps(c, "!unknown:s"); tr.Code(e) != tr.InvalidReference {
		t.Fatal("unknown canonical conversation accepted")
	}
	before := calls.Load()
	addr := a.Addressing()
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	check := func(e error) {
		t.Helper()
		if tr.Code(e) != tr.Unavailable {
			t.Fatal("operation after Close not unavailable")
		}
	}
	_, e := a.GetState(c, "!known:s", "test", "")
	check(e)
	_, e = a.PutState(c, "!known:s", "test", "", json.RawMessage(`{}`))
	check(e)
	_, e = a.Redact(c, "!known:s", "$id", "key")
	check(e)
	_, e = a.ThreadHistory(c, "!known:s", "$root", nil, 1)
	check(e)
	_, e = addr.Principal("@a:s")
	check(e)
	_, e = addr.Conversation(c, "!known:s")
	check(e)
	if addr.Format("@a:s") != "@a:s" || a.Caps().MaxContentBytes != MaxContentBytes || calls.Load() != before {
		t.Fatal("Close changed metadata or allowed I/O")
	}
}
