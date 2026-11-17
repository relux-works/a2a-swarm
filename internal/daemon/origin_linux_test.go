//go:build linux

package daemon

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/runorigin"
	"golang.org/x/sys/unix"
)

const launcherGeneration = "0123456789abcdef0123456789abcdef"
const inputGeneration = "fedcba9876543210fedcba9876543210"
const nextLauncherGeneration = "0123456789abcdef0123456789abcdee"

type originAuthority struct {
	generation atomic.Value
	calls      atomic.Int32
	err        error
}

func (g *originAuthority) CurrentGeneration(ctx context.Context, uid uint32) (string, error) {
	g.calls.Add(1)
	generation, _ := g.generation.Load().(string)
	return generation, g.err
}

type originProducer struct {
	opens, prepares, validates, closes atomic.Int32
	result                             runorigin.Result
	changed                            *runorigin.Result
	openErr, prepareErr, validateErr   error
	block                              string
	release                            <-chan struct{}
	closed                             chan struct{}
	mu                                 sync.Mutex
	selections                         []runorigin.Selection
}

func richResult() runorigin.Result {
	return runorigin.Result{IdentityRegistrationNumber: "registration-7", Origin: runorigin.Origin{Kind: "admission", EventID: strings.Repeat("a", 64), GrantID: strings.Repeat("b", 64), TipHash: strings.Repeat("c", 64), Mode: "enforcing"}}
}
func fakeOriginProducer() *originProducer {
	return &originProducer{result: richResult(), closed: make(chan struct{}, 32)}
}
func (p *originProducer) Open(ctx context.Context, sel runorigin.Selection) (runorigin.Session, error) {
	p.opens.Add(1)
	p.mu.Lock()
	p.selections = append(p.selections, sel)
	p.mu.Unlock()
	if p.block == "open" {
		p.wait(ctx)
	}
	return &originSession{p: p}, p.openErr
}
func (p *originProducer) wait(ctx context.Context) {
	if p.release != nil {
		<-p.release
	} else {
		<-ctx.Done()
	}
}

type originSession struct{ p *originProducer }

func (s *originSession) Prepare(ctx context.Context) (runorigin.Result, error) {
	s.p.prepares.Add(1)
	if s.p.block == "prepare" {
		s.p.wait(ctx)
	}
	return s.p.result, s.p.prepareErr
}
func (s *originSession) Validate(ctx context.Context) (runorigin.Result, error) {
	s.p.validates.Add(1)
	if s.p.block == "validate" {
		s.p.wait(ctx)
	}
	if s.p.changed != nil {
		return *s.p.changed, s.p.validateErr
	}
	return s.p.result, s.p.validateErr
}
func (s *originSession) Close() error { s.p.closes.Add(1); s.p.closed <- struct{}{}; return nil }
func (p *originProducer) awaitClose(t *testing.T) {
	t.Helper()
	select {
	case <-p.closed:
	case <-time.After(time.Second):
		t.Fatal("owned session not closed")
	}
}

type originFixture struct {
	s          *OriginService
	dir        string
	generation *originAuthority
	producer   *originProducer
	cancel     context.CancelFunc
	done       chan error
	once       sync.Once
}

func shortOriginDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "o-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0711); err != nil {
		t.Fatal(err)
	}
	return dir
}
func startOrigin(t *testing.T, producer *originProducer, change func(*OriginConfig)) *originFixture {
	t.Helper()
	return startOriginBeforeServe(t, producer, change, nil)
}
func startOriginBeforeServe(t *testing.T, producer *originProducer, change func(*OriginConfig), beforeServe func(*OriginService)) *originFixture {
	t.Helper()
	dir := shortOriginDir(t)
	g := &originAuthority{}
	g.generation.Store(launcherGeneration)
	cfg := OriginConfig{ControlDir: dir, Launcher: runorigin.Principal{UID: uint32(os.Geteuid()), Generation: launcherGeneration}, Generations: g}
	if producer != nil {
		cfg.Producer = producer
	}
	if change != nil {
		change(&cfg)
	}
	// Unit wire fixtures retain real kernel peer credentials while injecting a
	// distinct Source identity at construction; real UID placement is unqualified.
	s, err := newOriginService(cfg, uint32(os.Geteuid())+1000000)
	if err != nil {
		t.Fatal(err)
	}
	if beforeServe != nil {
		beforeServe(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &originFixture{s: s, dir: dir, generation: g, producer: producer, cancel: cancel, done: make(chan error, 1)}
	ready := make(chan struct{})
	go func() { f.done <- s.serve(ctx, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-f.done:
		cancel()
		t.Fatalf("serve: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("startup timeout")
	}
	t.Cleanup(func() { f.stop(t) })
	return f
}
func (f *originFixture) stop(t *testing.T) {
	t.Helper()
	f.once.Do(func() {
		f.cancel()
		select {
		case err := <-f.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serve did not stop")
		}
	})
}
func (f *originFixture) dial(t *testing.T) *net.UnixConn {
	t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: filepath.Join(f.dir, "origin.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func originRead(t *testing.T, c *net.UnixConn) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(7 * time.Second))
	b, err := readFrame(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func (f *originFixture) client(t *testing.T) *net.UnixConn {
	t.Helper()
	c := f.dial(t)
	b := originRead(t, c)
	var hello struct {
		Capabilities []string `json:"capabilities"`
		Incarnation  string   `json:"source_incarnation"`
	}
	if err := json.Unmarshal(b, &hello); err != nil || hello.Incarnation != f.s.incarnation {
		t.Fatalf("hello: %s, %v", b, err)
	}
	if !runorigin.LowerHex(hello.Incarnation, 32) || hello.Incarnation == strings.Repeat("0", 32) {
		t.Fatalf("invalid incarnation: %s", b)
	}
	want := 0
	if f.producer != nil {
		want = 2
	}
	if len(hello.Capabilities) != want {
		t.Fatalf("capability: %s", b)
	}
	return c
}
func sendOrigin(t *testing.T, c *net.UnixConn, op string, params any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"op": op, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	if err = writeFrame(c, b); err != nil {
		t.Fatal(err)
	}
}
func prepareParams() map[string]any {
	return map[string]any{"request_id": "11111111111111111111111111111111", "launch_nonce": "22222222222222222222222222222222", "event_ref": runorigin.EventRef{RoomID: "!room:example", EventID: "$matrix"}, "input_principal": runorigin.Principal{UID: 4242, Generation: inputGeneration}}
}
func validateParams(p runorigin.PreparedOrigin) map[string]any {
	return map[string]any{"request_id": p.RequestID, "launch_nonce": p.LaunchNonce, "prepared_id": p.PreparedID, "source_incarnation": p.SourceIncarnation}
}
func prepareOn(t *testing.T, c *net.UnixConn) runorigin.PreparedOrigin {
	t.Helper()
	sendOrigin(t, c, originPrepare, prepareParams())
	b := originRead(t, c)
	var p runorigin.PreparedOrigin
	if err := json.Unmarshal(b, &p); err != nil || !p.Origin.Valid() || p.RequestID != "11111111111111111111111111111111" || p.LaunchNonce != "22222222222222222222222222222222" || p.InputPrincipal != (runorigin.Principal{UID: 4242, Generation: inputGeneration}) || p.IdentityRegistrationNumber != "registration-7" || !runorigin.LowerHex(p.PreparedID, 32) {
		t.Fatalf("prepare: %s, %v", b, err)
	}
	var members map[string]json.RawMessage
	_ = json.Unmarshal(b, &members)
	if len(members) != 7 {
		t.Fatalf("response members: %s", b)
	}
	return p
}
func wantOriginCode(t *testing.T, c *net.UnixConn, code, inc string) {
	t.Helper()
	b := originRead(t, c)
	var fields map[string]string
	if err := json.Unmarshal(b, &fields); err != nil || len(fields) != 2 || fields["code"] != code || fields["source_incarnation"] != inc {
		t.Fatalf("want %s/%s, got %s (%v)", code, inc, b, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := readFrame(c); !errors.Is(err, io.EOF) && !errors.Is(err, unix.ECONNRESET) {
		t.Fatalf("refusal did not close connection: %v", err)
	}
}
func validOriginControl(t *testing.T) {
	t.Helper()
	producer := fakeOriginProducer()
	f := startOrigin(t, producer, nil)
	c := f.client(t)
	p := prepareOn(t, c)
	sendOrigin(t, c, originValidate, validateParams(p))
	b := originRead(t, c)
	var got runorigin.PreparedOrigin
	if json.Unmarshal(b, &got) != nil || got != p {
		t.Fatalf("validation changed bindings: %s", b)
	}
	producer.awaitClose(t)
	if producer.opens.Load() != 1 || producer.prepares.Load() != 1 || producer.validates.Load() != 1 || producer.closes.Load() != 1 {
		t.Fatal("owned session lifecycle")
	}
	producer.mu.Lock()
	defer producer.mu.Unlock()
	want := runorigin.Selection{Launcher: f.s.cfg.Launcher, Event: runorigin.EventRef{RoomID: "!room:example", EventID: "$matrix"}, Input: p.InputPrincipal}
	if len(producer.selections) != 1 || producer.selections[0] != want {
		t.Fatalf("selection: %+v", producer.selections)
	}
}

// TestSourceOriginCapability proves a valid typed producer works and stock has a definite refusal, never a generic/none result.
func TestSourceOriginCapability(t *testing.T) {
	t.Run("rich-positive", validOriginControl)
	t.Run("stock-refusal", func(t *testing.T) {
		f := startOrigin(t, nil, nil)
		c := f.client(t)
		sendOrigin(t, c, originPrepare, prepareParams())
		wantOriginCode(t, c, runorigin.NotSupported, f.s.incarnation)
	})
}

// TestSourceOriginOrigins proves malformed producer facts and caller-forged origins cannot replace a verified six-member result.
func TestSourceOriginOrigins(t *testing.T) {
	t.Run("valid", validOriginControl)
	cases := map[string]func(*runorigin.Result){
		"none":            func(r *runorigin.Result) { r.Origin.Kind = "none" },
		"matrix-id":       func(r *runorigin.Result) { r.Origin.EventID = "$matrix" },
		"uppercase":       func(r *runorigin.Result) { r.Origin.GrantID = strings.Repeat("A", 64) },
		"short-tip":       func(r *runorigin.Result) { r.Origin.TipHash = "abc" },
		"owner-command":   func(r *runorigin.Result) { r.Origin.CommandID = "forged" },
		"mode":            func(r *runorigin.Result) { r.Origin.Mode = "observe" },
		"missing-mapping": func(r *runorigin.Result) { r.IdentityRegistrationNumber = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := fakeOriginProducer()
			mutate(&p.result)
			f := startOrigin(t, p, nil)
			c := f.client(t)
			sendOrigin(t, c, originPrepare, prepareParams())
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			p.awaitClose(t)
		})
	}
	t.Run("forged-caller-object", func(t *testing.T) {
		p := fakeOriginProducer()
		f := startOrigin(t, p, nil)
		c := f.client(t)
		params := prepareParams()
		params["origin"] = richResult().Origin
		sendOrigin(t, c, originPrepare, params)
		wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
		if p.opens.Load() != 0 {
			t.Fatal("forged caller reached producer")
		}
	})
	for _, name := range []string{"origin", "mapping"} {
		t.Run("forged-recheck-"+name, func(t *testing.T) {
			p := fakeOriginProducer()
			changed := p.result
			if name == "origin" {
				changed.Origin.EventID = strings.Repeat("d", 64)
			} else {
				changed.IdentityRegistrationNumber = "other"
			}
			p.changed = &changed
			f := startOrigin(t, p, nil)
			c := f.client(t)
			prepared := prepareOn(t, c)
			sendOrigin(t, c, originValidate, validateParams(prepared))
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			p.awaitClose(t)
		})
	}
}

// TestSourceOriginIncarnationBinding proves both request/response incarnation binding and old-process replay refusal beside a valid current process.
func TestSourceOriginIncarnationBinding(t *testing.T) {
	t.Run("current", validOriginControl)
	t.Run("stale-on-live-record", func(t *testing.T) {
		p := fakeOriginProducer()
		f := startOrigin(t, p, nil)
		c := f.client(t)
		prepared := prepareOn(t, c)
		params := validateParams(prepared)
		params["source_incarnation"] = strings.Repeat("0", 32)
		sendOrigin(t, c, originValidate, params)
		wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
		if p.validates.Load() != 0 {
			t.Fatal("stale incarnation reached fresh checker")
		}
		p.awaitClose(t)
	})
	t.Run("restart-replay", func(t *testing.T) {
		producer := fakeOriginProducer()
		first := startOrigin(t, producer, nil)
		c := first.client(t)
		old := prepareOn(t, c)
		first.stop(t)
		producer.awaitClose(t)
		second := startOrigin(t, fakeOriginProducer(), nil)
		if second.s.incarnation == old.SourceIncarnation {
			t.Fatal("restart reused incarnation")
		}
		c = second.client(t)
		sendOrigin(t, c, originValidate, validateParams(old))
		wantOriginCode(t, c, runorigin.Refused, second.s.incarnation)
		if second.producer.opens.Load() != 0 || second.producer.validates.Load() != 0 {
			t.Fatal("restart restored prepared authority")
		}
		c = second.client(t)
		fresh := prepareOn(t, c)
		sendOrigin(t, c, originValidate, validateParams(fresh))
		var got runorigin.PreparedOrigin
		if json.Unmarshal(originRead(t, c), &got) != nil || got != fresh || got.SourceIncarnation == old.SourceIncarnation {
			t.Fatal("fresh response/replayed response mismatch")
		}
	})
}

// TestSourceOriginConsumeAndCorrelation proves validation is single-use and connection/request/nonce/prepared-ID bound.
func TestSourceOriginConsumeAndCorrelation(t *testing.T) {
	t.Run("positive", validOriginControl)
	for _, name := range []string{"repeat", "other-connection", "nonce", "request", "prepared-id", "superseded"} {
		t.Run(name, func(t *testing.T) {
			p := fakeOriginProducer()
			f := startOrigin(t, p, nil)
			c := f.client(t)
			prepared := prepareOn(t, c)
			params := validateParams(prepared)
			switch name {
			case "repeat":
				sendOrigin(t, c, originValidate, params)
				originRead(t, c)
				p.awaitClose(t)
			case "other-connection":
				c = f.client(t)
			case "nonce":
				params["launch_nonce"] = strings.Repeat("3", 32)
			case "request":
				params["request_id"] = strings.Repeat("4", 32)
			case "prepared-id":
				params["prepared_id"] = strings.Repeat("0", 32)
			case "superseded":
				_ = prepareOn(t, c)
				p.awaitClose(t)
			}
			before := p.validates.Load()
			sendOrigin(t, c, originValidate, params)
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			if p.validates.Load() != before {
				t.Fatal("invalid correlation reached checker")
			}
		})
	}
}

// TestSourceOriginPeerGate proves kernel UID and current authoritative generation gate all protected work, including after preparation.
func TestSourceOriginPeerGate(t *testing.T) {
	t.Run("authorized", validOriginControl)
	for _, name := range []string{"uid", "generation", "missing-authority", "dependency", "changed-generation"} {
		t.Run(name, func(t *testing.T) {
			p := fakeOriginProducer()
			f := startOrigin(t, p, func(cfg *OriginConfig) {
				if name == "uid" {
					cfg.Launcher.UID++
				}
				if name == "generation" {
					cfg.Launcher.Generation = nextLauncherGeneration
				}
				if name == "missing-authority" {
					cfg.Generations.(*originAuthority).err = runorigin.ErrRefused
				}
				if name == "dependency" {
					cfg.Generations.(*originAuthority).err = runorigin.ErrUnavailable
				}
			})
			var c *net.UnixConn
			if name == "changed-generation" {
				c = f.client(t)
				prepared := prepareOn(t, c)
				f.generation.generation.Store(nextLauncherGeneration)
				sendOrigin(t, c, originValidate, validateParams(prepared))
			} else {
				c = f.dial(t)
			}
			code := runorigin.Refused
			if name == "dependency" {
				code = runorigin.Unavailable
			}
			wantOriginCode(t, c, code, f.s.incarnation)
			if name == "changed-generation" {
				p.awaitClose(t)
				if p.validates.Load() != 0 {
					t.Fatal("stale generation reached recheck")
				}
			} else if p.opens.Load() != 0 || p.prepares.Load() != 0 {
				t.Fatal("unauthorized peer reached protected work")
			}
			if name == "uid" && f.generation.calls.Load() != 0 {
				t.Fatal("wrong UID reached generation dependency")
			}
		})
	}
}

// TestSourceOriginClosedWire proves valid payloads work while nested/outer duplicates, unknowns, bad types and oversized frames refuse before producer use.
func TestSourceOriginClosedWire(t *testing.T) {
	t.Run("valid", validOriginControl)
	good, _ := json.Marshal(map[string]any{"op": originPrepare, "params": prepareParams()})
	cases := map[string]string{
		"unknown-envelope":      strings.Replace(string(good), `"op":`, `"extra":0,"op":`, 1),
		"duplicate-envelope":    strings.Replace(string(good), `"op":`, `"op":"source.origin.prepare/1","op":`, 1),
		"unknown-param":         strings.Replace(string(good), `"launch_nonce":`, `"origin":{},"launch_nonce":`, 1),
		"duplicate-param":       strings.Replace(string(good), `"launch_nonce":`, `"launch_nonce":"fake","launch_nonce":`, 1),
		"escaped-duplicate":     strings.Replace(string(good), `"launch_nonce":`, `"launch_\u006eonce":"fake","launch_nonce":`, 1),
		"duplicate-event":       strings.Replace(string(good), `"room_id":`, `"room_id":"bad","room_id":`, 1),
		"unknown-event":         strings.Replace(string(good), `"room_id":`, `"session":"bad","room_id":`, 1),
		"duplicate-principal":   strings.Replace(string(good), `"uid":`, `"uid":1,"uid":`, 1),
		"unknown-principal":     strings.Replace(string(good), `"uid":`, `"role":"launcher","uid":`, 1),
		"null-principal":        strings.Replace(string(good), `"uid":4242`, `"uid":null`, 1),
		"zero-generation":       strings.Replace(string(good), `"generation":"`+inputGeneration+`"`, `"generation":0`, 1),
		"negative-uid":          strings.Replace(string(good), `"uid":4242`, `"uid":-1`, 1),
		"fractional-generation": strings.Replace(string(good), `"generation":"`+inputGeneration+`"`, `"generation":1.5`, 1),
		"missing":               strings.Replace(string(good), `,"request_id":"11111111111111111111111111111111"`, "", 1),
		"case":                  strings.Replace(string(good), `"request_id"`, `"Request_id"`, 1),
		"unknown-op":            strings.Replace(string(good), originPrepare, "j7_run_bind", 1),
		"scalar":                `null`,
		"array":                 `[]`,
		"invalid-utf8":          "{\"op\":\"\xff\"}",
		"uid-overflow":          strings.Replace(string(good), `"uid":4242`, `"uid":4294967296`, 1),
		"generation-overflow":   strings.Replace(string(good), `"generation":"`+inputGeneration+`"`, `"generation":18446744073709551616`, 1),
		"uid-string":            strings.Replace(string(good), `"uid":4242`, `"uid":"4242"`, 1),
		"empty-nonce":           strings.Replace(string(good), `"launch_nonce":"22222222222222222222222222222222"`, `"launch_nonce":""`, 1),
		"trailing":              string(good) + `{}`,
		"oversize":              "oversize",
		"zero-frame":            "zero-frame",
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			p := fakeOriginProducer()
			f := startOrigin(t, p, nil)
			c := f.client(t)
			if name == "oversize" || name == "zero-frame" {
				var header [4]byte
				if name == "oversize" {
					binary.BigEndian.PutUint32(header[:], MaxFrameBytes+1)
				}
				if _, err := c.Write(header[:]); err != nil {
					t.Fatal(err)
				}
			} else if err := writeFrame(c, []byte(payload)); err != nil {
				t.Fatal(err)
			}
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			if p.opens.Load() != 0 {
				t.Fatal("malformed wire reached producer")
			}
		})
	}
}

// TestSourceOriginValidateWire proves validate has its own closed schema, including duplicate/unknown fields, beside a valid consume.
func TestSourceOriginValidateWire(t *testing.T) {
	t.Run("valid", validOriginControl)
	for _, name := range []string{"unknown", "duplicate", "missing", "type"} {
		t.Run(name, func(t *testing.T) {
			p := fakeOriginProducer()
			f := startOrigin(t, p, nil)
			c := f.client(t)
			prepared := prepareOn(t, c)
			b, _ := json.Marshal(map[string]any{"op": originValidate, "params": validateParams(prepared)})
			switch name {
			case "unknown":
				b = bytes.Replace(b, []byte(`"prepared_id":`), []byte(`"origin":{},"prepared_id":`), 1)
			case "duplicate":
				b = bytes.Replace(b, []byte(`"prepared_id":`), []byte(`"prepared_id":"bad","prepared_id":`), 1)
			case "missing":
				b = bytes.Replace(b, []byte(`"request_id":"11111111111111111111111111111111",`), nil, 1)
			case "type":
				b = bytes.Replace(b, []byte(`"launch_nonce":"22222222222222222222222222222222"`), []byte(`"launch_nonce":null`), 1)
			}
			if err := writeFrame(c, b); err != nil {
				t.Fatal(err)
			}
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			p.awaitClose(t)
			if p.validates.Load() != 0 {
				t.Fatal("bad validate reached checker")
			}
		})
	}
}

// TestSourceOriginTimeouts proves prepare and validate each enforce the real five-second budget and discard even late successful results.
func TestSourceOriginTimeouts(t *testing.T) {
	t.Run("valid", validOriginControl)
	for _, phase := range []string{"prepare", "validate"} {
		t.Run(phase, func(t *testing.T) {
			release := make(chan struct{})
			p := fakeOriginProducer()
			p.block = phase
			p.release = release
			f := startOrigin(t, p, nil)
			if f.s.budget != 5*time.Second {
				t.Fatal("budget changed")
			}
			c := f.client(t)
			var prepared runorigin.PreparedOrigin
			if phase == "validate" {
				prepared = prepareOn(t, c)
			}
			start := time.Now()
			if phase == "prepare" {
				sendOrigin(t, c, originPrepare, prepareParams())
			} else {
				sendOrigin(t, c, originValidate, validateParams(prepared))
			}
			wantOriginCode(t, c, runorigin.Unavailable, f.s.incarnation)
			elapsed := time.Since(start)
			close(release)
			p.awaitClose(t)
			if elapsed < 4500*time.Millisecond || elapsed > 6500*time.Millisecond {
				t.Fatalf("budget elapsed: %s", elapsed)
			}
			c = f.client(t)
			if phase == "validate" {
				sendOrigin(t, c, originValidate, validateParams(prepared))
				wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			}
		})
	}
}

// TestSourceOriginLostResponses proves disconnect destroys unconsumed authority and a fresh connection must prepare again.
func TestSourceOriginLostResponses(t *testing.T) {
	t.Run("valid", validOriginControl)
	for _, phase := range []string{"prepare", "validate"} {
		t.Run(phase, func(t *testing.T) {
			p := fakeOriginProducer()
			f := startOrigin(t, p, nil)
			c := f.client(t)
			prepared := prepareOn(t, c)
			if phase == "validate" {
				sendOrigin(t, c, originValidate, validateParams(prepared))
			}
			c.Close()
			p.awaitClose(t)
			c = f.client(t)
			sendOrigin(t, c, originValidate, validateParams(prepared))
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			c = f.client(t)
			fresh := prepareOn(t, c)
			if fresh.PreparedID == prepared.PreparedID {
				t.Fatal("reused prepared ID")
			}
		})
	}
}

// TestSourceOriginDependencies proves named dependency failures remain retryable, arbitrary producer text is suppressed, and owned sessions always close.
func TestSourceOriginDependencies(t *testing.T) {
	t.Run("valid", validOriginControl)
	for _, phase := range []string{"open", "prepare", "validate"} {
		for _, retry := range []bool{false, true} {
			t.Run(phase+"/"+map[bool]string{false: "refuse", true: "unavailable"}[retry], func(t *testing.T) {
				p := fakeOriginProducer()
				err := errors.New("sensitive-producer-detail")
				code := runorigin.Refused
				if retry {
					err = runorigin.ErrUnavailable
					code = runorigin.Unavailable
				}
				switch phase {
				case "open":
					p.openErr = err
				case "prepare":
					p.prepareErr = err
				case "validate":
					p.validateErr = err
				}
				f := startOrigin(t, p, nil)
				c := f.client(t)
				if phase == "validate" {
					prepared := prepareOn(t, c)
					sendOrigin(t, c, originValidate, validateParams(prepared))
				} else {
					sendOrigin(t, c, originPrepare, prepareParams())
				}
				wantOriginCode(t, c, code, f.s.incarnation)
				p.awaitClose(t)
			})
		}
	}
}

// TestSourceOriginSocketPath proves wrong modes/links/existing endpoints refuse without repair; owner checks use synthetic metadata, not privileged chown.
func TestSourceOriginSocketPath(t *testing.T) {
	t.Run("valid", validOriginControl)
	for _, name := range []string{"directory-mode", "directory-symlink", "socket-symlink", "existing-socket", "regular-file"} {
		t.Run(name, func(t *testing.T) {
			dir := shortOriginDir(t)
			path := filepath.Join(dir, "origin.sock")
			switch name {
			case "directory-mode":
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
			case "directory-symlink":
				link := filepath.Join(shortOriginDir(t), "link")
				if err := os.Symlink(dir, link); err != nil {
					t.Fatal(err)
				}
				dir = link
			case "socket-symlink":
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "existing-socket":
				l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
			case "regular-file":
				if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(dir)
			if err != nil {
				t.Fatal(err)
			}
			p := fakeOriginProducer()
			g := &originAuthority{}
			g.generation.Store(launcherGeneration)
			s, err := newOriginService(OriginConfig{ControlDir: dir, Launcher: runorigin.Principal{UID: uint32(os.Geteuid()), Generation: launcherGeneration}, Generations: g, Producer: p}, uint32(os.Geteuid())+1000000)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Serve(context.Background()); !errors.Is(err, runorigin.ErrRefused) {
				t.Fatalf("wanted refusal: %v", err)
			}
			after, err := os.Lstat(dir)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("directory repaired")
			}
			if p.opens.Load() != 0 || g.calls.Load() != 0 {
				t.Fatal("bad path reached authority")
			}
			if name == "socket-symlink" {
				b, _ := os.ReadFile(filepath.Join(dir, "target"))
				if string(b) != "preserve" {
					t.Fatal("repaired link")
				}
			}
		})
	}
	for _, name := range []string{"socket-mode", "directory-mode-live", "socket-symlink-live", "socket-hardlink-live"} {
		t.Run(name, func(t *testing.T) {
			p := fakeOriginProducer()
			f := startOrigin(t, p, nil)
			path := filepath.Join(f.dir, "origin.sock")
			switch name {
			case "socket-mode":
				if err := os.Chmod(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory-mode-live":
				if err := os.Chmod(f.dir, 0700); err != nil {
					t.Fatal(err)
				}
			case "socket-hardlink-live":
				if err := os.Link(path, filepath.Join(f.dir, "other.sock")); err != nil {
					t.Fatal(err)
				}
			case "socket-symlink-live":
				saved := filepath.Join(f.dir, "saved.sock")
				if err := os.Rename(path, saved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(saved, path); err != nil {
					t.Fatal(err)
				}
			}
			c := f.dial(t)
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			if p.opens.Load() != 0 {
				t.Fatal("bad endpoint reached producer")
			}
			if name == "socket-mode" {
				info, _ := os.Lstat(path)
				if info.Mode().Perm() != 0600 {
					t.Fatal("mode repaired")
				}
			}
		})
	}
	t.Run("owner-metadata", func(t *testing.T) {
		dir := shortOriginDir(t)
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "origin.sock"), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if err := os.Chmod(filepath.Join(dir, "origin.sock"), 0666); err != nil {
			t.Fatal(err)
		}
		d, chain, err := originDirectoryChain(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		e := &originEndpoint{dir: d, chain: chain, path: filepath.Join(dir, "origin.sock")}
		if unix.Fstat(int(d.Fd()), &e.dirStamp) != nil || unix.Lstat(e.path, &e.socketStamp) != nil || !e.valid() {
			t.Fatal("valid metadata control")
		}
		// Valid() reads real owner metadata; the pure predicates are also exercised
		// against a wrong-owner synthetic stat without provisioning another UID.
		bad := e.socketStamp
		bad.Uid++
		if originSocketMetadata(&bad, uint32(os.Geteuid())) {
			t.Fatal("wrong socket owner accepted")
		}
		bad = e.dirStamp
		bad.Uid++
		if originDirectoryMetadata(&bad, uint32(os.Geteuid())) {
			t.Fatal("wrong directory owner accepted")
		}
	})
}

// TestSourceOriginConstruction proves authority/generation omissions and reuse of a service incarnation cannot start an endpoint.
func TestSourceOriginConstruction(t *testing.T) {
	if _, err := NewOriginService(OriginConfig{}); !errors.Is(err, runorigin.ErrRefused) {
		t.Fatal(err)
	}
	if _, err := NewOriginService(OriginConfig{Generations: &originAuthority{}}); !errors.Is(err, runorigin.ErrRefused) {
		t.Fatal(err)
	}
	f := startOrigin(t, fakeOriginProducer(), nil)
	if err := f.s.Serve(context.Background()); !errors.Is(err, runorigin.ErrRefused) {
		t.Fatalf("reused start: %v", err)
	}
	if !reflect.DeepEqual([]string{runorigin.Refused, runorigin.Unavailable, runorigin.NotSupported}, []string{"run_origin_refused", "run_origin_unavailable", "origin_not_supported"}) {
		t.Fatal("code drift")
	}
}

// TestSourceOriginErrorBudget proves error output honors the call's remaining budget even under real Unix-socket backpressure.
func TestSourceOriginErrorBudget(t *testing.T) {
	t.Run("valid", validOriginControl)
	t.Run("backpressure", func(t *testing.T) {
		dir := shortOriginDir(t)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "e.sock"), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		server, err := listener.AcceptUnix()
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		if err := server.SetWriteBuffer(1024); err != nil {
			t.Fatal(err)
		}
		_ = server.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
		_, err = server.Write(bytes.Repeat([]byte("x"), 1<<20))
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("control did not fill socket: %v", err)
		}
		generation := &originAuthority{}
		generation.generation.Store(launcherGeneration)
		service, err := newOriginService(OriginConfig{Launcher: runorigin.Principal{UID: uint32(os.Geteuid()), Generation: launcherGeneration}, Generations: generation, Producer: fakeOriginProducer()}, uint32(os.Geteuid())+1000000)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		service.originErrorUntil(server, runorigin.ErrUnavailable, start.Add(80*time.Millisecond))
		if elapsed := time.Since(start); elapsed < 50*time.Millisecond || elapsed > 300*time.Millisecond {
			t.Fatalf("terminal response widened call budget: %s", elapsed)
		}
	})
}

// TestSourceOriginCorrelationPins proves only 32 lowercase hex correlations reach prepare or consume a record; invalid fields never reach protected work.
func TestSourceOriginCorrelationPins(t *testing.T) {
	t.Run("valid", validOriginControl)
	for _, op := range []string{originPrepare, originValidate} {
		for _, field := range []string{"request_id", "launch_nonce"} {
			for name, value := range map[string]string{"empty": "", "31": strings.Repeat("a", 31), "33": strings.Repeat("a", 33), "uppercase": strings.Repeat("A", 32), "non-hex": strings.Repeat("g", 32)} {
				t.Run(op+"/"+field+"/"+name, func(t *testing.T) {
					p := fakeOriginProducer()
					f := startOrigin(t, p, nil)
					c := f.client(t)
					params := prepareParams()
					if op == originValidate {
						params = validateParams(prepareOn(t, c))
					}
					params[field] = value
					before := p.opens.Load()
					sendOrigin(t, c, op, params)
					wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
					if p.opens.Load() != before || p.validates.Load() != 0 {
						t.Fatal("invalid correlation reached protected work")
					}
				})
			}
		}
	}
}

// TestSourceOriginUIDPins proves public construction rejects zero/shared launcher UIDs and wire decoding rejects zero input beside valid separate-UID policy controls.
func TestSourceOriginUIDPins(t *testing.T) {
	g := &originAuthority{}
	g.generation.Store(launcherGeneration)
	cfg := OriginConfig{Launcher: runorigin.Principal{UID: uint32(os.Geteuid()) + 1, Generation: launcherGeneration}, Generations: g, Producer: fakeOriginProducer()}
	t.Run("distinct-launcher", func(t *testing.T) {
		if _, err := NewOriginService(cfg); err != nil {
			t.Fatal(err)
		}
	})
	for _, uid := range []uint32{0, uint32(os.Geteuid())} {
		t.Run(fmt.Sprintf("launcher-%d", uid), func(t *testing.T) {
			bad := cfg
			bad.Launcher.UID = uid
			if _, err := NewOriginService(bad); !errors.Is(err, runorigin.ErrRefused) {
				t.Fatalf("invalid launcher accepted: %v", err)
			}
		})
	}
	t.Run("valid-wire", validOriginControl)
	t.Run("zero-input", func(t *testing.T) {
		p := fakeOriginProducer()
		f := startOrigin(t, p, nil)
		c := f.client(t)
		params := prepareParams()
		params["input_principal"] = runorigin.Principal{UID: 0, Generation: inputGeneration}
		sendOrigin(t, c, originPrepare, params)
		wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
		if p.opens.Load() != 0 {
			t.Fatal("zero input reached producer")
		}
	})
}

// TestSourceOriginRegistrationPin proves opaque maximum-length registrations remain framed while forbidden control bytes refuse before a record is retained.
func TestSourceOriginRegistrationPin(t *testing.T) {
	for _, registration := range []string{"registration-7", strings.Repeat("<", 4096)} {
		t.Run(fmt.Sprintf("valid-%d", len(registration)), func(t *testing.T) {
			p := fakeOriginProducer()
			p.result.IdentityRegistrationNumber = registration
			f := startOrigin(t, p, nil)
			c := f.client(t)
			sendOrigin(t, c, originPrepare, prepareParams())
			var got runorigin.PreparedOrigin
			if err := json.Unmarshal(originRead(t, c), &got); err != nil || got.IdentityRegistrationNumber != registration || !got.Origin.Valid() {
				t.Fatal("opaque registration lost framed result")
			}
		})
	}
	for _, ch := range []byte{0, '\r', '\n'} {
		t.Run(fmt.Sprintf("control-%d", ch), func(t *testing.T) {
			p := fakeOriginProducer()
			p.result.IdentityRegistrationNumber = "bad" + string(ch)
			f := startOrigin(t, p, nil)
			c := f.client(t)
			sendOrigin(t, c, originPrepare, prepareParams())
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			p.awaitClose(t)
		})
	}
}

// TestSourceOriginIdleRestart proves an idle preparation expires within one read budget and only fresh H1 preparation restores authority.
func TestSourceOriginIdleRestart(t *testing.T) {
	t.Run("prompt-valid", validOriginControl)
	t.Run("idle-restart", func(t *testing.T) {
		p := fakeOriginProducer()
		f := startOrigin(t, p, nil)
		c := f.client(t)
		old := prepareOn(t, c)
		wantOriginCode(t, c, runorigin.Unavailable, f.s.incarnation)
		p.awaitClose(t)
		if p.validates.Load() != 0 {
			t.Fatal("idle connection validated")
		}
		c = f.client(t)
		fresh := prepareOn(t, c)
		if fresh.PreparedID == old.PreparedID {
			t.Fatal("idle preparation reused")
		}
		sendOrigin(t, c, originValidate, validateParams(fresh))
		var got runorigin.PreparedOrigin
		if json.Unmarshal(originRead(t, c), &got) != nil || got != fresh {
			t.Fatal("H1 restart failed")
		}
	})
}
