//go:build linux

package keymakersigner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/relux-works/a2a-swarm/internal/signedpost"
	"github.com/relux-works/swarma-envelope/record"
	"github.com/relux-works/swarma-keymaker/messageauthor"
)

// These are the pinned envelope module's PUBLIC fixture operands, not secrets:
// author = 32 bytes of 0x03; alternate author = the public BIP-340 vector scalar 3.
const fixtureKey = "bip340:Ux_mBoE0UD0nIxMyJ8hnrI-myDxTfppEw8W9vcsf4zc"
const alternateKey = "bip340:-TCKAZJYwxBJNE-F-J1SKbUxyEWDb5mwhgHxE7zgNvk"
const fixtureT = "sha256:f0f08c11a4388a53fc9240302f29b9984a13ce9b9a8b27f74d87f183951d74bc"

func fixtureIdentity(t *testing.T) signedpost.Identity {
	t.Helper()
	raw, err := os.ReadFile("../envelopeobserve/testdata/P-BIP-author.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Fixture struct {
			Attestations map[string]string `json:"attestations"`
		} `json:"fixture"`
	}
	if json.Unmarshal(raw, &v) != nil {
		t.Fatal("fixture decode")
	}
	a, err := base64.RawURLEncoding.DecodeString(v.Fixture.Attestations[fixtureT])
	if err != nil || len(a) == 0 {
		t.Fatal("fixture attestation")
	}
	return signedpost.Identity{Key: fixtureKey, AttestationID: fixtureT, Attestation: a}
}

type holderOptions struct {
	hello     string
	stage     messageauthor.Method
	change    func(messageauthor.Request, []byte) []byte
	signature string
	refusal   *messageauthor.Refusal
	wait      time.Duration
}

type fakeHolder struct {
	t           *testing.T
	endpoint    Endpoint
	listener    *net.UnixListener
	identity    signedpost.Identity
	opts        holderOptions
	mu          sync.Mutex
	requests    []messageauthor.Request
	connections int
	helloBytes  int
	err         error
	wg          sync.WaitGroup
	done        chan struct{}
}

func startHolder(t *testing.T, opts holderOptions) *fakeHolder {
	t.Helper()
	// Use a short private directory even when the Go test name is long.
	dir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "ks-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		t.Fatal(err)
	}
	h := &fakeHolder{t: t, listener: l, identity: fixtureIdentity(t), opts: opts, done: make(chan struct{}), endpoint: Endpoint{Path: path, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), Mode: 0o660}}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.connections++
			h.mu.Unlock()
			h.wg.Add(1)
			go func() { defer h.wg.Done(); defer c.Close(); h.serve(c) }()
		}
	}()
	t.Cleanup(func() {
		close(h.done)
		l.Close()
		h.wg.Wait()
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.err != nil {
			t.Error(h.err)
		}
		os.RemoveAll(dir)
	})
	return h
}

func (h *fakeHolder) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = err
	}
}

func (h *fakeHolder) serve(c net.Conn) {
	c.SetDeadline(time.Now().Add(3 * time.Second))
	frame, err := readFrame(c)
	h.mu.Lock()
	h.helloBytes += len(frame)
	h.mu.Unlock()
	if err != nil {
		return
	} // Pin refusals may close without sending any bytes.
	var hello struct {
		Method, RequestID string
		Versions          []int
	}
	var o map[string]json.RawMessage
	if json.Unmarshal(frame[4:], &o) != nil || len(o) != 3 {
		h.fail(errors.New("hello schema"))
		return
	}
	json.Unmarshal(o["method"], &hello.Method)
	json.Unmarshal(o["request_id"], &hello.RequestID)
	json.Unmarshal(o["versions"], &hello.Versions)
	want := `{"method":"hello","request_id":"` + hello.RequestID + `","versions":[1]}`
	if hello.Method != "hello" || !nativeID(hello.RequestID) || string(frame[4:]) != want {
		h.fail(errors.New("hello must be canonical and offer only version 1"))
		return
	}
	if h.opts.hello == "silent" {
		select {
		case <-h.done:
		case <-time.After(2500 * time.Millisecond):
		}
		return
	}
	reply := `{"request_id":"` + hello.RequestID + `","result":{"version":1}}`
	switch h.opts.hello {
	case "refused":
		reply = `{"error":{"code":"version_unsupported","retryable":false},"request_id":"` + hello.RequestID + `"}`
	case "version":
		reply = strings.Replace(reply, `"version":1`, `"version":2`, 1)
	case "id":
		reply = strings.Replace(reply, hello.RequestID, strings.Repeat("a", 32), 1)
	case "extra":
		reply = strings.Replace(reply, `"version":1`, `"version":1,"x":0`, 1)
	case "noncanonical":
		reply = " " + reply
	}
	if writeFrame(c, wireFrame([]byte(reply))) != nil {
		return
	}
	for {
		// Enforce the holder's NEXT-complete-frame rule after hello and identity.
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		frame, err = readFrame(c)
		if err != nil {
			return
		}
		q, err := messageauthor.DecodeRequestFrame(frame)
		if err != nil {
			h.fail(errors.New("request rejected by real codec"))
			return
		}
		if !nativeID(q.ID) || q.TimeoutMS < 1 || q.TimeoutMS > 2000 {
			h.fail(errors.New("request bounds"))
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, q)
		h.mu.Unlock()
		if h.opts.wait > 0 {
			select {
			case <-h.done:
				return
			case <-time.After(h.opts.wait):
			}
		}
		r := messageauthor.Response{ID: q.ID}
		if h.opts.refusal != nil && (h.opts.stage == "" || h.opts.stage == q.Method) {
			r.Refusal = h.opts.refusal
			if strings.Contains(" frame_too_large frame_truncated frame_malformed ccj_noncanonical ", " "+r.Refusal.Code+" ") {
				r.ID = ""
			}
		} else if q.Method == messageauthor.IdentityMethod {
			v := h.identity
			r.Identity = &messageauthor.IdentityResult{KeyID: v.Key, AuthorAttestation: v.AttestationID, Attestation: v.Attestation}
		} else {
			if _, err := record.AdmitSigner(q.Preimage, record.IdentityPolicy{Check: true, MultiAgent: true, CallerKey: fixtureKey, CallerAttestationID: fixtureT}); err != nil {
				h.fail(errors.New("fixture holder admission"))
				return
			}
			p := bytes.Clone(q.Preimage)
			operand := bytes.Repeat([]byte{3}, 32)
			if h.opts.signature == "preimage" {
				p = append(p, '\n')
			}
			if h.opts.signature == "other-key" {
				operand = make([]byte, 32)
				operand[31] = 3
			}
			key, _ := btcec.PrivKeyFromBytes(operand)
			digest := sha256.Sum256(p)
			sig, err := schnorr.Sign(key, digest[:])
			if err != nil {
				h.fail(err)
				return
			}
			k := fixtureKey
			if h.opts.signature == "returned-key" {
				k = alternateKey
			}
			r.Signature = &messageauthor.SignatureResult{KeyID: k, Signature: sig.Serialize()}
		}
		out, err := messageauthor.EncodeResponseFrame(q, r)
		if err != nil {
			h.fail(errors.New("fixture response encode"))
			return
		}
		if h.opts.change != nil && (h.opts.stage == "" || h.opts.stage == q.Method) {
			out = h.opts.change(q, out)
		}
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if writeFrame(c, out) != nil {
			return
		}
	}
}

func nativeID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func wireFrame(body []byte) []byte {
	f := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(f, uint32(len(body)))
	copy(f[4:], body)
	return f
}

func mutateResponse(change func(map[string]any)) func(messageauthor.Request, []byte) []byte {
	return func(_ messageauthor.Request, f []byte) []byte {
		v, err := record.Parse(f[4:], messageauthor.MaxFrame, 16)
		if err != nil {
			panic(err)
		}
		o := v.(map[string]any)
		change(o)
		b, err := record.Canonical(o)
		if err != nil {
			panic(err)
		}
		return wireFrame(b)
	}
}

func afterFirstIdentity(field, value string) func(messageauthor.Request, []byte) []byte {
	var mu sync.Mutex
	count := 0
	return mutateResponse(func(o map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		count++
		if count > 1 {
			o["result"].(map[string]any)[field] = value
		}
	})
}

func post(t *testing.T, c *Client, want string) ([]signedpost.Signed, error) {
	t.Helper()
	var sent []signedpost.Signed
	s := signedpost.Sender{Signer: c, Audience: "service-a", Qualified: func(signedpost.Identity) bool { return true }}
	err := s.Publish(context.Background(), signedpost.Draft{Room: "!room:test.invalid", Recipients: []string{"@reader:test.invalid"}, Kind: "notice", Topic: "$root", Text: []byte("exact fixture text")}, func(v signedpost.Signed) error { sent = append(sent, v); return nil })
	if want == "" {
		if err != nil || len(sent) != 1 {
			t.Fatalf("valid post: sends=%d error=%v", len(sent), err)
		}
	} else if signedpost.Code(err) != want || len(sent) != 0 {
		t.Fatalf("refused post: sends=%d error=%v want=%s", len(sent), err, want)
	}
	return sent, err
}

// Claim: a valid holder signs the exact sender preimage and preserves the envelope and raw attestation in one publication.
func TestClientPublishesVerifiedFixture(t *testing.T) {
	h := startHolder(t, holderOptions{})
	sent, _ := post(t, New(h.endpoint), "")
	v, err := record.Parse(sent[0].Envelope, 65536, 16)
	if err != nil {
		t.Fatal(err)
	}
	e := v.(map[string]any)
	body := e["body"].(map[string]any)
	p, err := record.SigningPreimage("message/1", fixtureKey, "author", body)
	if err != nil {
		t.Fatal(err)
	}
	sigEntry := e["signatures"].([]any)[0].(map[string]any)
	sig, err := record.DecodeBase64(sigEntry["sig"].(string), 64)
	if err != nil || record.VerifySignature(fixtureKey, sig, p) != nil {
		t.Fatal("published signature")
	}
	if !bytes.Equal(sent[0].Attestation, h.identity.Attestation) || body["author_attestation"] != fixtureT {
		t.Fatal("attestation changed")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connections != 2 || len(h.requests) != 3 {
		t.Fatalf("connections/requests: %d/%d", h.connections, len(h.requests))
	}
	if h.requests[0].Method != messageauthor.IdentityMethod || h.requests[1].Method != messageauthor.IdentityMethod || h.requests[2].Method != messageauthor.SignMethod || !bytes.Equal(h.requests[2].Preimage, p) {
		t.Fatal("operation order or preimage")
	}
	if h.requests[0].ID == h.requests[1].ID || h.requests[1].ID == h.requests[2].ID {
		t.Fatal("independent request IDs")
	}
}

// Claim: refused, foreign, noncanonical and silent hellos publish nothing beside a valid version-1 control.
func TestClientHelloRefusals(t *testing.T) {
	for _, mode := range []string{"", "refused", "version", "id", "extra", "noncanonical", "silent"} {
		t.Run(fmt.Sprintf("hello-%s", mode), func(t *testing.T) {
			h := startHolder(t, holderOptions{hello: mode})
			want := "signer_unavailable"
			if mode == "" {
				want = ""
			}
			start := time.Now()
			post(t, New(h.endpoint), want)
			if mode == "silent" && (time.Since(start) < 1800*time.Millisecond || time.Since(start) > 2800*time.Millisecond) {
				t.Fatal("hello deadline not two seconds")
			}
		})
	}
}

// Claim: correlation, method, schema, identity and mathematical signature failures publish nothing beside a valid control.
func TestClientResponseRefusals(t *testing.T) {
	maxAttestation := fixtureIdentity(t).Attestation
	maxAttestation = append(maxAttestation, bytes.Repeat([]byte{' '}, 8192-len(maxAttestation))...)
	cases := []struct {
		name string
		opts holderOptions
	}{
		{"valid", holderOptions{}},
		{"max-attestation", holderOptions{stage: messageauthor.IdentityMethod, change: mutateResponse(func(o map[string]any) {
			o["result"].(map[string]any)["attestation"] = base64.RawURLEncoding.EncodeToString(maxAttestation)
		})}},
		{"response-id", holderOptions{stage: messageauthor.SignMethod, change: mutateResponse(func(o map[string]any) { o["request_id"] = strings.Repeat("a", 32) })}},
		{"other-method", holderOptions{stage: messageauthor.IdentityMethod, change: mutateResponse(func(o map[string]any) {
			o["result"] = map[string]any{"key_id": fixtureKey, "signature": base64.RawURLEncoding.EncodeToString(make([]byte, 64))}
		})}},
		{"extra-member", holderOptions{change: mutateResponse(func(o map[string]any) { o["extra"] = int64(0) })}},
		{"missing-member", holderOptions{change: mutateResponse(func(o map[string]any) { delete(o, "request_id") })}},
		{"different-key", holderOptions{signature: "returned-key"}},
		{"different-preimage", holderOptions{signature: "preimage"}},
		{"signature-under-other-key", holderOptions{signature: "other-key"}},
		{"attestation-id", holderOptions{stage: messageauthor.IdentityMethod, change: mutateResponse(func(o map[string]any) {
			o["result"].(map[string]any)["author_attestation"] = "sha256:" + strings.Repeat("0", 64)
		})}},
		{"empty-attestation", holderOptions{stage: messageauthor.IdentityMethod, change: mutateResponse(func(o map[string]any) { o["result"].(map[string]any)["attestation"] = "" })}},
		{"large-attestation", holderOptions{stage: messageauthor.IdentityMethod, change: mutateResponse(func(o map[string]any) {
			o["result"].(map[string]any)["attestation"] = base64.RawURLEncoding.EncodeToString(make([]byte, 8193))
		})}},
		{"malformed-attestation", holderOptions{stage: messageauthor.IdentityMethod, change: mutateResponse(func(o map[string]any) { o["result"].(map[string]any)["attestation"] = "e30" })}},
		{"key-shape", holderOptions{stage: messageauthor.IdentityMethod, change: mutateResponse(func(o map[string]any) { o["result"].(map[string]any)["key_id"] = "bip340:" + strings.Repeat("_", 43) })}},
		{"identity-key-changed", holderOptions{stage: messageauthor.IdentityMethod, change: afterFirstIdentity("key_id", alternateKey)}},
		{"identity-attestation-changed", holderOptions{stage: messageauthor.IdentityMethod, change: afterFirstIdentity("author_attestation", "sha256:"+strings.Repeat("0", 64))}},
		{"short-signature", holderOptions{stage: messageauthor.SignMethod, change: mutateResponse(func(o map[string]any) {
			o["result"].(map[string]any)["signature"] = base64.RawURLEncoding.EncodeToString(make([]byte, 63))
		})}},
		{"attestation-id-uppercase", holderOptions{stage: messageauthor.IdentityMethod, change: mutateResponse(func(o map[string]any) { o["result"].(map[string]any)["author_attestation"] = strings.ToUpper(fixtureT) })}},
		{"unknown-code", holderOptions{change: mutateResponse(func(o map[string]any) {
			delete(o, "result")
			o["error"] = map[string]any{"code": "unknown", "retryable": false}
		})}},
		{"incorrect-retry-bit", holderOptions{change: mutateResponse(func(o map[string]any) {
			delete(o, "result")
			o["error"] = map[string]any{"code": "busy", "retryable": false}
		})}},
		{"zero-frame", holderOptions{change: func(messageauthor.Request, []byte) []byte { return make([]byte, 4) }}},
		{"large-frame", holderOptions{change: func(messageauthor.Request, []byte) []byte {
			f := make([]byte, 4)
			binary.BigEndian.PutUint32(f, 65537)
			return f
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startHolder(t, tc.opts)
			want := "signer_unqualified"
			if tc.name == "valid" || tc.name == "max-attestation" {
				want = ""
			}
			post(t, New(h.endpoint), want)
		})
	}
}

var retryCodes = strings.Fields("busy rate_limited generation_source_unavailable audit_unavailable request_deadline_exceeded")
var unqualifiedCodes = strings.Fields("key_not_enrolled message_author_unqualified")
var refusedCodes = strings.Fields("frame_too_large frame_truncated frame_malformed ccj_noncanonical request_malformed request_conflict version_unsupported hello_required hello_already_negotiated operation_unsupported peer_identity_unavailable generation_source_fenced generation_inactive generation_mismatch binding_not_found peer_binding_mismatch binding_conflict identity_revoked role_not_permitted custody_unavailable entropy_unavailable clock_untrusted key_not_valid_now sign_key_mismatch state_capacity_exceeded signer_preimage_too_large signer_tag_malformed signer_domain_refused signer_payload_invalid signer_identity_unavailable signer_identity_malformed signer_identity_mismatch")

// Claim: every codec refusal maps to its documented seam error and publishes nothing, beside a successful holder control.
func TestClientRefusalMapping(t *testing.T) {
	post(t, New(startHolder(t, holderOptions{}).endpoint), "")
	for _, group := range []struct {
		codes []string
		retry bool
		want  string
	}{{retryCodes, true, "signer_unavailable"}, {unqualifiedCodes, false, "signer_unqualified"}, {refusedCodes, false, "signer_refused"}} {
		for _, code := range group.codes {
			for _, stage := range []messageauthor.Method{messageauthor.IdentityMethod, messageauthor.SignMethod} {
				t.Run(code+"/"+string(stage), func(t *testing.T) {
					h := startHolder(t, holderOptions{stage: stage, refusal: &messageauthor.Refusal{Code: code, Retryable: group.retry}})
					_, err := post(t, New(h.endpoint), group.want)
					var r *signedpost.Refusal
					if !errors.As(err, &r) || r.Retryable != group.retry || group.want == "signer_refused" && r.Detail != code {
						t.Fatal("typed refusal properties", err)
					}
				})
			}
		}
	}
	// Exercise the mapper independently: the codec rejects inconsistent bits
	// before dispatch, but this layer must never infer retryability from a name.
	if !errors.Is(mapRefusal(&messageauthor.Refusal{Code: "key_not_enrolled", Retryable: true}), signedpost.ErrUnavailable) {
		t.Fatal("retryable bit ignored")
	}
	var refused *signedpost.RefusedError
	if !errors.As(mapRefusal(&messageauthor.Refusal{Code: "busy", Retryable: false}), &refused) {
		t.Fatal("retryability inferred from code")
	}
}

// Claim: path, socket, ownership, mode, parent and kernel-peer pin failures send no hello bytes and publish nothing beside a pinned control.
func TestClientEndpointPins(t *testing.T) {
	for _, mode := range []string{"valid", "relative", "missing", "regular", "owner", "group", "mode", "configured-mode", "parent-writable", "symlink", "parent-symlink"} {
		t.Run(mode, func(t *testing.T) {
			h := startHolder(t, holderOptions{})
			e := h.endpoint
			switch mode {
			case "relative":
				e.Path = "relative.sock"
			case "missing":
				e.Path += "missing"
			case "regular":
				e.Path = filepath.Join(filepath.Dir(e.Path), "file")
				if os.WriteFile(e.Path, nil, 0o660) != nil {
					t.Fatal("regular file")
				}
			case "owner":
				e.UID++
			case "group":
				e.GID++
			case "mode":
				if os.Chmod(e.Path, 0o600) != nil {
					t.Fatal("chmod")
				}
			case "configured-mode":
				e.Mode = 0o600
			case "parent-writable":
				if os.Chmod(filepath.Dir(e.Path), 0o770) != nil {
					t.Fatal("chmod parent")
				}
			case "symlink":
				e.Path += "link"
				if os.Symlink(h.endpoint.Path, e.Path) != nil {
					t.Fatal("symlink")
				}
			case "parent-symlink":
				dir := filepath.Dir(e.Path)
				link := dir + "link"
				if os.Symlink(dir, link) != nil {
					t.Fatal("parent symlink")
				}
				t.Cleanup(func() { os.Remove(link) })
				e.Path = filepath.Join(link, "s")
			}
			want := "signer_unavailable"
			if mode == "valid" {
				want = ""
			}
			post(t, New(e), want)
			h.mu.Lock()
			defer h.mu.Unlock()
			if mode != "valid" && (h.helloBytes != 0 || h.connections != 0) {
				t.Fatal("endpoint pin failed after connect/hello")
			}
		})
	}
	h := startHolder(t, holderOptions{})
	conn, err := net.Dial("unix", h.endpoint.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !peerUID(conn, h.endpoint.UID) || peerUID(conn, h.endpoint.UID+1) {
		t.Fatal("kernel peer UID control")
	}
}

// Claim: equal-preimage caller retries reuse a native ID, changed input gets another, and one Sign call never retries itself.
func TestClientRetainedRequestIDs(t *testing.T) {
	h := startHolder(t, holderOptions{})
	c := New(h.endpoint)
	post(t, c, "")
	h.mu.Lock()
	p := bytes.Clone(h.requests[2].Preimage)
	id := h.requests[2].ID
	h.mu.Unlock()
	for range 2 {
		k, s, err := c.Sign(context.Background(), p)
		if err != nil || string(k) != fixtureKey || record.VerifySignature(fixtureKey, s, p) != nil {
			t.Fatal("equal-preimage control", err)
		}
	}
	post(t, c, "")
	h.mu.Lock()
	var ids []string
	for _, q := range h.requests {
		if q.Method == messageauthor.SignMethod {
			ids = append(ids, q.ID)
		}
	}
	h.mu.Unlock()
	if len(ids) != 4 || ids[0] != id || ids[1] != id || ids[2] != id || ids[3] == id {
		t.Fatal("retained request IDs")
	}
	bad := startHolder(t, holderOptions{stage: messageauthor.SignMethod, refusal: &messageauthor.Refusal{Code: "busy", Retryable: true}})
	k, s, err := New(bad.endpoint).Sign(context.Background(), p)
	if !errors.Is(err, signedpost.ErrUnavailable) || k != nil || s != nil {
		t.Fatal("retryable refusal", err)
	}
	bad.mu.Lock()
	defer bad.mu.Unlock()
	n := 0
	for _, q := range bad.requests {
		if q.Method == messageauthor.SignMethod {
			n++
		}
	}
	if n != 1 {
		t.Fatal("client retried signing", n)
	}
}

// Claim: canceled or deadline-limited caller contexts return signer_unavailable without publication beside a prompt control.
func TestClientOperationDeadline(t *testing.T) {
	post(t, New(startHolder(t, holderOptions{}).endpoint), "")
	for _, cancelNow := range []bool{false, true} {
		h := startHolder(t, holderOptions{wait: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if cancelNow {
			cancel()
		}
		start := time.Now()
		sends := 0
		sender := signedpost.Sender{Signer: New(h.endpoint), Audience: "service-a", Qualified: func(signedpost.Identity) bool { return true }}
		err := sender.Publish(ctx, signedpost.Draft{Room: "!room:test.invalid", Text: []byte("fixture")}, func(signedpost.Signed) error { sends++; return nil })
		cancel()
		if signedpost.Code(err) != "signer_unavailable" || sends != 0 || time.Since(start) > 500*time.Millisecond {
			t.Fatal("overall deadline", err)
		}
	}
}

// Claim: bounded retention refuses capacity instead of replacing live IDs, and expired entries permit a different native ID.
func TestClientRetentionBounds(t *testing.T) {
	c := New(Endpoint{})
	p := []byte("public fixture input")
	id, err := c.signingID(p)
	if err != nil || !nativeID(id) {
		t.Fatal("random ID control", err)
	}
	for i := 1; i < 64; i++ {
		if _, err := c.signingID([]byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.signingID([]byte("capacity overflow")); !errors.Is(err, signedpost.ErrUnavailable) {
		t.Fatal("capacity accepted")
	}
	if got, err := c.signingID(p); err != nil || got != id {
		t.Fatal("live identity evicted")
	}
	for k, v := range c.ids {
		v.until = time.Now().Add(-time.Second)
		c.ids[k] = v
	}
	fresh, err := c.signingID(p)
	if err != nil || fresh == id || len(c.ids) != 1 {
		t.Fatal("expired identity retained")
	}
}

// Claim: partial frame writes are completed, zero-progress writes refuse, and truncated input returns no frame beside a complete control.
func TestClientFrameIO(t *testing.T) {
	frame := wireFrame([]byte(`{}`))
	var out bytes.Buffer
	if writeFrame(shortWriter{&out}, frame) != nil || !bytes.Equal(out.Bytes(), frame) {
		t.Fatal("partial write control")
	}
	if writeFrame(shortWriter{}, frame) != io.ErrShortWrite {
		t.Fatal("zero write accepted")
	}
	got, err := readFrame(bytes.NewReader(frame))
	if err != nil || !bytes.Equal(got, frame) {
		t.Fatal("read control")
	}
	for _, f := range [][]byte{frame[:3], frame[:len(frame)-1]} {
		if got, err := readFrame(bytes.NewReader(f)); err == nil || got != nil {
			t.Fatal("truncated input")
		}
	}
}

type shortWriter struct{ w io.Writer }

func (w shortWriter) Write(p []byte) (int, error) {
	if w.w == nil {
		return 0, nil
	}
	return w.w.Write(p[:1])
}
