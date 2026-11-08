package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// All identities, content, transport and provider values here are inert. No
// listening socket, live account, provider activation or copied auth is used.
type operationStore struct{ reads atomic.Int32 }

func (s *operationStore) Read(context.Context, config.Paths, config.SecretReference) (string, error) {
	s.reads.Add(1)
	return "REDACTION_PLACEHOLDER", nil
}
func (*operationStore) Create(context.Context, config.Paths, config.SecretReference, string) error {
	return errors.New("creation forbidden")
}

type operationTransport func(*http.Request) (*http.Response, error)

func (f operationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type operationWorld struct {
	t                 *testing.T
	base              string
	paths             config.Paths
	connection        config.Connection
	store             *operationStore
	calls, sends      atomic.Int32
	mu                sync.Mutex
	transactions      []string
	requests          [][]byte
	reservationStates []string
	applied           map[string]string
	ack               string
	drop              bool
	responseBody      func(*http.Request, string) io.ReadCloser
	responseLength    int64
}

func operationWorldNew(t *testing.T) *operationWorld {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("durable mode supports macOS and Linux")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &operationWorld{t: t, base: base, store: &operationStore{}, ack: `{"event_id":"$accepted"}`, responseLength: -1}
	w.connection = config.Connection{ID: "synthetic", URL: "https://matrix.example.invalid", ServerName: "example.invalid", Store: config.CredentialStore{Backend: "keychain", AccessTokenService: "synthetic.access", PasswordService: "synthetic.password"}}
	registry := config.ConnectionRegistry{SchemaVersion: 1, Current: "own", Profiles: map[string]config.Connection{"own": w.connection}}
	raw, _ := json.Marshal(registry)
	if err := os.WriteFile(filepath.Join(base, "connections.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	w.paths, err = w.connection.Paths(config.Paths{Base: base}, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := w.connection.Binding("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.PublishBoundCredentials(w.paths, "alice", false, config.Credentials{ConnectionBinding: &binding, UserID: binding.UserID, DeviceID: "synthetic-device", Homeserver: w.connection.URL}); err != nil {
		t.Fatal(err)
	}
	return w
}
func (w *operationWorld) app() (*App, *bytes.Buffer, *bytes.Buffer) {
	out, diag := new(bytes.Buffer), new(bytes.Buffer)
	a := NewApp()
	a.Stdout = out
	a.Stderr = diag
	a.connectionSecretStore = w.store
	a.Getenv = func(key string) string {
		switch key {
		case config.EnvConfigDir:
			return w.base
		case config.EnvAgent:
			return "alice"
		}
		return ""
	}
	a.Now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	a.HTTP = &http.Client{Transport: operationTransport(w.transport)}
	return a, out, diag
}
func (w *operationWorld) transport(req *http.Request) (*http.Response, error) {
	w.calls.Add(1)
	if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/event/") {
		root := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		raw, _ := json.Marshal(map[string]any{"event_id": root, "sender": "@author:example.invalid", "type": "m.room.message", "content": map[string]any{"msgtype": "m.text", "body": "synthetic topic"}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), ContentLength: int64(len(raw)), Header: make(http.Header), Request: req}, nil
	}
	if req.Method != http.MethodPut || !strings.Contains(req.URL.Path, "/send/m.room.message/") {
		return nil, errors.New("unexpected synthetic request")
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	w.sends.Add(1)
	id := "op"
	j, err := config.OpenPostJournal(w.paths, "alice", false, false)
	state := "missing"
	if err == nil {
		r, ok, e := j.Read(id)
		if e == nil && ok {
			state = r.Outcome
		}
		j.Close()
	}
	w.mu.Lock()
	w.requests = append(w.requests, raw)
	w.transactions = append(w.transactions, req.URL.Path)
	w.reservationStates = append(w.reservationStates, state)
	if w.applied == nil {
		w.applied = make(map[string]string)
	}
	w.applied["synthetic-device "+req.URL.String()] = "$accepted"
	w.mu.Unlock()
	if w.drop {
		return nil, errors.New("synthetic applied response lost")
	}
	body := io.NopCloser(strings.NewReader(w.ack))
	if w.responseBody != nil {
		body = w.responseBody(req, w.ack)
	}
	return &http.Response{StatusCode: 200, Body: body, ContentLength: w.responseLength, Header: make(http.Header), Request: req}, nil
}
func operationArgs() []string {
	return []string{"post", "!room:example.invalid", "--topic", "$root", "--operation-id", "op", "synthetic body"}
}
func (w *operationWorld) record() config.PostOperation {
	w.t.Helper()
	j, err := config.OpenPostJournal(w.paths, "alice", false, false)
	if err != nil {
		w.t.Fatal(err)
	}
	defer j.Close()
	r, ok, err := j.Read("op")
	if err != nil || !ok {
		w.t.Fatalf("record: exists=%v err=%v", ok, err)
	}
	return r
}
func operationResult(t *testing.T, out *bytes.Buffer) postOperationResult {
	t.Helper()
	var r postOperationResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Scenario 1: exact one request, reservation before I/O, ACK before output,
// fresh App local recovery and no provider/HTTP on status or identical post.
func TestDurablePostHappyPathAndRecovery(t *testing.T) {
	w := operationWorldNew(t)
	a, out, diag := w.app()
	a.Stdout = operationObservingWriter{out, func() {
		if w.record().Outcome != "acknowledged" {
			t.Error("output before durable ACK")
		}
	}}
	if code := a.Run(context.Background(), operationArgs()); code != 0 {
		t.Fatalf("code=%d %s", code, diag)
	}
	result := operationResult(t, out)
	if result.Outcome != "acknowledged" || result.EventID != "$accepted" {
		t.Fatal(result)
	}
	record := w.record()
	if len(record.Request) != 0 || record.ContentDigest != config.PostOperationDigest(w.requests[0]) || record.RequestLength != len(w.requests[0]) {
		t.Fatal("request bytes not dropped / digest changed")
	}
	if w.sends.Load() != 1 || w.reservationStates[0] != "unknown" || !strings.HasSuffix(w.transactions[0], record.TransactionID) {
		t.Fatal("I/O identity/ordering")
	}
	calls, providers := w.calls.Load(), w.store.reads.Load()
	for _, args := range [][]string{{"post-operation", "status", "op", "--json"}, operationArgs()} {
		next, output, diagnostics := w.app()
		next.Now = func() time.Time { return time.Date(2027, 2, 3, 4, 5, 6, 0, time.UTC) }
		if code := next.Run(context.Background(), args); code != 0 {
			t.Fatalf("recovery=%d %s", code, diagnostics)
		}
		if got := operationResult(t, output); got.EventID != result.EventID || got.Reason != "retained_local_evidence" {
			t.Fatal(got)
		}
	}
	if w.calls.Load() != calls || w.store.reads.Load() != providers || w.sends.Load() != 1 {
		t.Fatal("retained operation performed remote/provider work")
	}
}

type operationObservingWriter struct {
	io.Writer
	observe func()
}

func (w operationObservingWriter) Write(b []byte) (int, error) { w.observe(); return w.Writer.Write(b) }

// Scenarios 2 and 9: server-applied/lost ACK stays UNKNOWN across restart.
func TestDurablePostAppliedLostACKNeverReplays(t *testing.T) {
	w := operationWorldNew(t)
	w.drop = true
	a, out, _ := w.app()
	if code := a.Run(context.Background(), operationArgs()); code != 1 {
		t.Fatal(code)
	}
	got := operationResult(t, out)
	if got.Outcome != "unknown" || got.EventID != "" || w.record().Outcome != "unknown" {
		t.Fatal(got)
	}
	calls, providers := w.calls.Load(), w.store.reads.Load()
	for _, args := range [][]string{operationArgs(), {"post-operation", "status", "op", "--json"}} {
		a, out, _ = w.app()
		if code := a.Run(context.Background(), args); code != 1 {
			t.Fatal(code)
		}
		if operationResult(t, out).Outcome != "unknown" {
			t.Fatal(out)
		}
	}
	if len(w.applied) != 1 {
		t.Fatal("synthetic applied-event ledger changed")
	}
	if w.sends.Load() != 1 || w.calls.Load() != calls || w.store.reads.Load() != providers {
		t.Fatal("UNKNOWN replayed")
	}
}

type operationFaultBody struct {
	io.Reader
	closeErr bool
	readErr  bool
	cancel   context.CancelFunc
}

func (b *operationFaultBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		if b.cancel != nil {
			b.cancel()
		}
		if b.readErr {
			return n, io.ErrUnexpectedEOF
		}
	}
	return n, err
}
func (b *operationFaultBody) Close() error {
	if b.closeErr {
		return errors.New("synthetic close failure")
	}
	return nil
}

// Scenario 3: invalid strict original-endpoint ACKs cannot mint success IDs.
func TestDurablePostRejectedACKs(t *testing.T) {
	invalid := map[string]string{"missing": `{}`, "empty": `{"event_id":""}`, "type": `{"event_id":4}`, "duplicate": `{"event_id":"$a","event_id":"$b"}`, "escaped": `{"event_id":"$a","\u0065vent_id":"$b"}`, "trailing": `{"event_id":"$a"}{}`, "unicode": `{"event_id":"$\ud800"}`, "alias": `{"Event_ID":"$b"}`, "read": "", "close": "", "length": "", "cancel": ""}
	for name, ack := range invalid {
		t.Run(name, func(t *testing.T) {
			w := operationWorldNew(t)
			a, out, _ := w.app()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if ack != "" {
				w.ack = ack
			}
			if name == "length" {
				w.responseLength = int64(len(w.ack) + 1)
			}
			if name == "read" || name == "close" || name == "cancel" {
				w.responseBody = func(_ *http.Request, s string) io.ReadCloser {
					b := &operationFaultBody{Reader: strings.NewReader(s), readErr: name == "read", closeErr: name == "close"}
					if name == "cancel" {
						b.cancel = cancel
					}
					return b
				}
			}
			if code := a.Run(ctx, operationArgs()); code != 1 {
				t.Fatalf("invalid ACK accepted: %d", code)
			}
			if got := operationResult(t, out); got.Outcome != "unknown" || got.EventID != "" {
				t.Fatal(got)
			}
			a, _, _ = w.app()
			if a.Run(context.Background(), operationArgs()) != 1 || w.sends.Load() != 1 {
				t.Fatal("rejected ACK replayed")
			}
		})
	}
	// Positive neighbor prevents a reject-all parser from satisfying this suite.
	w := operationWorldNew(t)
	a, _, _ := w.app()
	if a.Run(context.Background(), operationArgs()) != 0 {
		t.Fatal("valid ACK rejected")
	}
}

// Scenario 4: each instance stops at one boundary; a fresh App sees exact state.
func TestDurablePostCrashBoundaries(t *testing.T) {
	cases := []struct {
		stage, state string
		sends        int32
	}{{"before_prepare", "missing", 0}, {"after_prepare", "not_submitted", 0}, {"after_reservation", "unknown", 0}, {"after_ack_observed", "unknown", 1}, {"after_ack_persist", "acknowledged", 1}}
	for _, tc := range cases {
		t.Run(tc.stage, func(t *testing.T) {
			w := operationWorldNew(t)
			a, _, _ := w.app()
			a.postOperationFault = func(stage string) error {
				if stage == tc.stage {
					return errors.New("synthetic process stop")
				}
				return nil
			}
			if code := a.Run(context.Background(), operationArgs()); code != 1 {
				t.Fatal(code)
			}
			if w.sends.Load() != tc.sends {
				t.Fatal("crash send count")
			}
			if tc.state == "missing" {
				if _, err := os.Stat(filepath.Join(w.paths.Base, "post-operations", "agents", "alice", "op.json")); !os.IsNotExist(err) {
					t.Fatal("pre-create crash left record")
				}
				return
			}
			before := w.record()
			if before.Outcome != tc.state {
				t.Fatalf("%s != %s", before.Outcome, tc.state)
			}
			a, _, _ = w.app()
			_ = a.Run(context.Background(), operationArgs())
			after := w.record()
			if after.TransactionID != before.TransactionID || after.ContentDigest != before.ContentDigest || after.Outcome != tc.state || w.sends.Load() != tc.sends {
				t.Fatal("restart changed/replayed evidence")
			}
		})
	}
}

// Scenario 5: a known but unpersisted ACK is not a success projection; broken
// output after persistence retains ACK and does not submit again.
func TestDurablePostPersistenceAndOutputFailures(t *testing.T) {
	w := operationWorldNew(t)
	a, out, diag := w.app()
	a.postOperationFault = func(stage string) error {
		if stage == "before_ack_persist" {
			return errors.New("synthetic disk failure")
		}
		return nil
	}
	if a.Run(context.Background(), operationArgs()) != 1 || strings.Contains(out.String()+diag.String(), "$accepted") || !strings.Contains(diag.String(), "do not repost") || w.record().Outcome != "unknown" {
		t.Fatal("undurable ACK success claim")
	}
	if got := operationResult(t, out); got.Outcome != "unknown" || got.Reason != "ack_persist_failed" || got.EventID != "" {
		t.Fatal(got)
	}
	for _, writer := range []io.Writer{operationShortWriter{}, operationBrokenWriter{}, operationFlushWriter{}} {
		w := operationWorldNew(t)
		a, _, _ = w.app()
		a.Stdout = writer
		if a.Run(context.Background(), operationArgs()) != 1 || w.record().Outcome != "acknowledged" {
			t.Fatal("output failure erased ACK")
		}
		a, _, _ = w.app()
		if a.Run(context.Background(), operationArgs()) != 0 || w.sends.Load() != 1 {
			t.Fatal("broken output caused resend")
		}
	}
}

type operationShortWriter struct{}

func (operationShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type operationBrokenWriter struct{}

func (operationBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// Scenario 6: changed intent/destination/metadata never overwrites a prior ID.
func TestDurablePostImmutableIntent(t *testing.T) {
	for _, change := range []string{"text", "kind", "to", "deadline", "room", "root", "device", "account", "endpoint", "binding"} {
		t.Run(change, func(t *testing.T) {
			w := operationWorldNew(t)
			w.drop = true
			a, _, _ := w.app()
			if a.Run(context.Background(), operationArgs()) != 1 {
				t.Fatal("seed UNKNOWN")
			}
			before := w.record()
			args := operationArgs()
			switch change {
			case "text":
				args[len(args)-1] = "different"
			case "kind":
				args = append(args, "--kind", "ack")
			case "to":
				args = append(args, "--to", "@other:example.invalid")
			case "deadline":
				args = append(args, "--reply-by", "2026-01-03T00:00:00Z")
			case "room":
				args[1] = "!other:example.invalid"
			case "root":
				args[3] = "$other"
			default:
				path := w.paths.AgentFile("alice")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var c config.Credentials
				if json.Unmarshal(raw, &c) != nil {
					t.Fatal("metadata")
				}
				switch change {
				case "device":
					c.DeviceID = "other-device"
				case "account":
					c.UserID = "@other:example.invalid"
				case "endpoint":
					c.Homeserver = "https://other.example.invalid"
				case "binding":
					c.ConnectionBinding.Connection.ID = "different"
				}
				raw, _ = json.Marshal(c)
				if os.WriteFile(path, raw, 0600) != nil {
					t.Fatal("rewrite synthetic metadata")
				}
			}
			calls := w.calls.Load()
			a, _, _ = w.app()
			if a.Run(context.Background(), args) != 1 {
				t.Fatal("immutable change accepted")
			}
			if w.calls.Load() != calls || w.sends.Load() != 1 {
				t.Fatal("changed operation performed HTTP")
			}
			after := w.record()
			if after.Outcome != before.Outcome || after.TransactionID != before.TransactionID || after.ContentDigest != before.ContentDigest {
				t.Fatal("UNKNOWN overwritten")
			}
		})
	}
}

// Scenarios 7 and 8: bounded requests and unsupported combinations have valid
// adjacent controls; status has zero provider and zero transport effects.
func TestDurablePostIsolationAndSecrecy(t *testing.T) {
	for _, mode := range []string{"alias", "slug", "reply", "reply-main", "voice", "envelope", "json", "id-file", "protected", "unsupported", "secret", "missing-device"} {
		t.Run(mode, func(t *testing.T) {
			w := operationWorldNew(t)
			a, out, diag := w.app()
			args := operationArgs()
			switch mode {
			case "alias":
				args[1] = "#room:example.invalid"
			case "slug":
				args[3] = "topic-slug"
			case "reply":
				args = append(args, "--re", "$reply")
			case "reply-main":
				args = append(args, "--reply-main", "--re", "$reply")
			case "voice":
				args = append(args, "--voice")
			case "envelope":
				args = append(args, "--envelope", "@not-read")
			case "json":
				args = append(args, "--json")
			case "id-file":
				args = append(args, "--id-file", filepath.Join(w.base, "not-created"))
			case "unsupported":
				a.postOperationOS = "plan9"
			case "secret":
				args[len(args)-1] = "REDACTION_PLACEHOLDER"
			case "protected":
				c := w.connection
				// Run resets the selected connection. Persist the protected policy
				// in the real synthetic registry so preflight actually reads it.
				c.AdminRegistration = &config.AdminRegistration{Method: "tuwunel-v1.9.4-admin-room", RoomID: args[1], AdminUserID: "@admin:example.invalid", ServerBotUserID: "@server:example.invalid", ExpectedMembers: []string{"@admin:example.invalid", "@server:example.invalid"}, ReplyDeadlineSeconds: 10}
				registry := config.ConnectionRegistry{SchemaVersion: 1, Current: "own", Profiles: map[string]config.Connection{"own": c}}
				raw, err := json.Marshal(registry)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(w.base, "connections.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-device":
				raw, err := os.ReadFile(w.paths.AgentFile("alice"))
				if err != nil {
					t.Fatal(err)
				}
				var c config.Credentials
				_ = json.Unmarshal(raw, &c)
				c.DeviceID = ""
				raw, _ = json.Marshal(c)
				if os.WriteFile(w.paths.AgentFile("alice"), raw, 0600) != nil {
					t.Fatal("metadata")
				}
			}
			if a.Run(context.Background(), args) == 0 {
				t.Fatal("excluded mode accepted")
			}
			if w.sends.Load() != 0 {
				t.Fatal("excluded mode sent")
			}
			if mode != "secret" && (w.calls.Load() != 0 || w.store.reads.Load() != 0) {
				t.Fatal("excluded mode reached HTTP or provider")
			}
			if mode == "protected" && !strings.Contains(diag.String(), "protected/admin") {
				t.Fatal("protected-room control refused at wrong boundary")
			}
			if mode == "unsupported" && !strings.Contains(diag.String(), "macOS or Linux") {
				t.Fatal("unsupported platform reason missing")
			}
			if strings.Contains(out.String()+diag.String(), "REDACTION_PLACEHOLDER") {
				t.Fatal("secret in diagnostics")
			}
			if _, err := os.Stat(filepath.Join(w.paths.Base, "post-operations", "agents", "alice", "op.json")); !os.IsNotExist(err) {
				t.Fatal("excluded mode persisted operation")
			}
		})
	}
	w := operationWorldNew(t)
	a, out, diag := w.app()
	if a.Run(context.Background(), operationArgs()) != 0 {
		t.Fatal(diag)
	}
	calls, reads := w.calls.Load(), w.store.reads.Load()
	a, out, diag = w.app()
	if a.Run(context.Background(), []string{"post-operation", "status", "op", "--json"}) != 0 {
		t.Fatal(diag)
	}
	if w.calls.Load() != calls || w.store.reads.Load() != reads {
		t.Fatal("status accessed HTTP/provider")
	}
	for _, forbidden := range []string{"synthetic body", w.base, "synthetic.access", "REDACTION_PLACEHOLDER", "transaction_id", "fingerprint"} {
		if strings.Contains(out.String()+diag.String(), forbidden) {
			t.Fatalf("projection exposed %s", forbidden)
		}
	}
	a, _, _ = w.app()
	if a.Run(context.Background(), []string{"post-operation", "status", "missing", "--json"}) != 1 {
		t.Fatal("missing record claimed non-application")
	}
}

// Scenario 9: the independent I/O oracle fails for the reservation-order
// mutant, while the separate valid-ACK control continues to acknowledge.
func TestDurablePostMutationControl(t *testing.T) {
	for _, mutant := range []bool{false, true} {
		w := operationWorldNew(t)
		a, _, diag := w.app()
		a.postOperationLateReserve = mutant
		if a.Run(context.Background(), operationArgs()) != 0 {
			t.Fatal(diag)
		}
		orderSafe := len(w.reservationStates) == 1 && w.reservationStates[0] == "unknown"
		if orderSafe == mutant {
			t.Fatalf("ordering oracle failed to distinguish mutation=%v states=%v", mutant, w.reservationStates)
		}
		if w.record().Outcome != "acknowledged" || w.sends.Load() != 1 {
			t.Fatal("independent valid-ACK control failed")
		}
	}
}

func TestDurablePostConcurrentInvocations(t *testing.T) {
	w := operationWorldNew(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, _, _ := w.app()
			code := a.Run(context.Background(), operationArgs())
			if code != 0 && code != 1 {
				t.Errorf("unexpected exit %d", code)
			}
		}()
	}
	wg.Wait()
	if w.sends.Load() != 1 || w.record().Outcome != "acknowledged" {
		t.Fatal("concurrent first invocations did not yield exactly one acknowledged mutation")
	}
}

// Scenario 4 also crosses real process boundaries. CI runs the already-built
// test binary as a helper: no nested build, live service or inherited auth.
func TestDurablePostCrashProcesses(t *testing.T) {
	cases := []struct {
		stage, state string
		sends        int
	}{{"before_prepare", "missing", 0}, {"after_prepare", "not_submitted", 0}, {"after_reservation", "unknown", 0}, {"after_ack_observed", "unknown", 1}, {"after_ack_persist", "acknowledged", 1}}
	for _, tc := range cases {
		t.Run(tc.stage, func(t *testing.T) {
			w := operationWorldNew(t)
			run := func(mode, stage string) (int, []byte) {
				cmd := exec.Command(os.Args[0], "-test.run=^TestDurablePostProcessHelper$")
				cmd.Env = []string{"A2A_INERT_HELPER=1", "A2A_INERT_BASE=" + w.base, "A2A_INERT_MODE=" + mode, "A2A_INERT_STAGE=" + stage, "HOME=" + w.base, "TMPDIR=" + w.base}
				var out, diag bytes.Buffer
				cmd.Stdout = &out
				cmd.Stderr = &diag
				err := cmd.Run()
				if err == nil {
					return 0, out.Bytes()
				}
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return exit.ExitCode(), out.Bytes()
				}
				t.Fatal(err)
				return -1, nil
			}
			code, _ := run("post", tc.stage)
			if code != 87 {
				t.Fatalf("child did not stop at boundary: %d", code)
			}
			log, _ := os.ReadFile(filepath.Join(w.base, "mutations.log"))
			if bytes.Count(log, []byte("mutation\n")) != tc.sends {
				t.Fatal("process mutation count")
			}
			code, output := run("status", "")
			if tc.state == "missing" {
				if code != 1 || len(output) != 0 {
					t.Fatal("missing record invented evidence")
				}
				return
			}
			var result postOperationResult
			if json.Unmarshal(output, &result) != nil || result.Outcome != tc.state {
				t.Fatalf("fresh process status: %s", output)
			}
			wantExit := 0
			if tc.state == "unknown" {
				wantExit = 1
			}
			if code != wantExit {
				t.Fatal(code)
			}
			_, _ = run("post", "")
			after, _ := os.ReadFile(filepath.Join(w.base, "mutations.log"))
			if !bytes.Equal(log, after) {
				t.Fatal("new process replayed existing operation")
			}
		})
	}
}

func TestDurablePostProcessHelper(t *testing.T) {
	if os.Getenv("A2A_INERT_HELPER") != "1" {
		t.Skip("CI subprocess helper only")
	}
	base := os.Getenv("A2A_INERT_BASE")
	registry, err := config.LoadConnections(config.Paths{Base: base})
	if err != nil || registry == nil {
		os.Exit(88)
	}
	c := registry.Profiles[registry.Current]
	paths, err := c.Paths(config.Paths{Base: base}, "alice", false)
	if err != nil {
		os.Exit(88)
	}
	w := &operationWorld{t: t, base: base, paths: paths, connection: c, store: &operationStore{}, ack: `{"event_id":"$accepted"}`, responseLength: -1}
	a, _, _ := w.app()
	a.Stdout = os.Stdout
	a.Stderr = os.Stderr
	a.HTTP.Transport = operationTransport(func(req *http.Request) (*http.Response, error) {
		resp, err := w.transport(req)
		if req.Method == http.MethodPut {
			f, e := os.OpenFile(filepath.Join(base, "mutations.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
			if e != nil {
				os.Exit(88)
			}
			if _, e = f.WriteString("mutation\n"); e != nil {
				os.Exit(88)
			}
			if f.Sync() != nil {
				os.Exit(88)
			}
			f.Close()
		}
		return resp, err
	})
	stage := os.Getenv("A2A_INERT_STAGE")
	a.postOperationFault = func(s string) error {
		if s == stage {
			os.Exit(87)
		}
		return nil
	}
	args := operationArgs()
	if os.Getenv("A2A_INERT_MODE") == "status" {
		args = []string{"post-operation", "status", "op", "--json"}
	}
	os.Exit(a.Run(context.Background(), args))
}

func TestDurablePostBindingRecheckedBeforeReservation(t *testing.T) {
	w := operationWorldNew(t)
	a, _, _ := w.app()
	a.postOperationFault = func(stage string) error {
		if stage == "after_prepare" {
			changed := w.connection
			changed.URL = "https://changed.example.invalid"
			registry := config.ConnectionRegistry{SchemaVersion: 1, Current: "own", Profiles: map[string]config.Connection{"own": changed}}
			data, _ := json.Marshal(registry)
			return os.WriteFile(filepath.Join(w.base, "connections.json"), data, 0600)
		}
		return nil
	}
	if a.Run(context.Background(), operationArgs()) != 1 || w.sends.Load() != 0 || w.record().Outcome != "not_submitted" {
		t.Fatal("changed registry bypassed binding recheck")
	}
}

func TestDurablePostMonotonicOnLaterValidationFailure(t *testing.T) {
	for _, lost := range []bool{false, true} {
		w := operationWorldNew(t)
		w.drop = lost
		a, _, _ := w.app()
		_ = a.Run(context.Background(), operationArgs())
		before := w.record()
		calls := w.calls.Load()
		args := append(operationArgs(), "--kind", "invalid")
		a, _, _ = w.app()
		if a.Run(context.Background(), args) != 2 {
			t.Fatal("invalid kind accepted")
		}
		args = operationArgs()
		args[len(args)-1] = "changed body"
		a, _, _ = w.app()
		if a.Run(context.Background(), args) != 1 {
			t.Fatal("changed body accepted")
		}
		if w.record().Outcome != before.Outcome || w.calls.Load() != calls || w.sends.Load() != 1 {
			t.Fatal("later refusal changed retained ACK/UNKNOWN")
		}
	}
}

type operationFlushWriter struct{}

func (operationFlushWriter) Write(p []byte) (int, error) { return len(p), nil }
func (operationFlushWriter) Flush() error                { return io.ErrClosedPipe }

func TestDurablePostFramedRequestLimit(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		w := operationWorldNew(t)
		a, _, diag := w.app()
		args := operationArgs()
		text := "x"
		if oversize {
			text = "<"
		}
		args[len(args)-1] = strings.Repeat(text, MaxBodyBytes)
		code := a.Run(context.Background(), args)
		if oversize {
			if code != 2 || w.sends.Load() != 0 || !strings.Contains(diag.String(), "64 KiB") {
				t.Fatalf("framed max+1 refusal: code=%d %s", code, diag)
			}
		} else if code != 0 || w.sends.Load() != 1 {
			t.Fatalf("valid large request refused: %d %s", code, diag)
		}
	}
}

func TestDurablePostStatusClosedAndOwnOnly(t *testing.T) {
	w := operationWorldNew(t)
	a, _, diag := w.app()
	if a.Run(context.Background(), operationArgs()) != 0 {
		t.Fatal(diag)
	}
	a, out, diag := w.app()
	if a.Run(context.Background(), []string{"post-operation", "status", "op", "--json"}) != 0 {
		t.Fatal(diag)
	}
	var projection map[string]json.RawMessage
	if json.Unmarshal(out.Bytes(), &projection) != nil || len(projection) != 6 {
		t.Fatal("status projection gained fields")
	}
	for _, field := range []string{"operation_id", "outcome", "event_id", "room_id", "root_id", "reason"} {
		if _, ok := projection[field]; !ok {
			t.Fatal("missing closed projection field")
		}
	}
	calls, providers := w.calls.Load(), w.store.reads.Load()
	for _, mode := range []string{"wrong-identity", "missing-metadata", "missing-device"} {
		a, _, _ = w.app()
		switch mode {
		case "wrong-identity":
			a.as = "bob"
		case "missing-metadata":
			a.Getenv = func(key string) string {
				if key == config.EnvConfigDir {
					return filepath.Join(w.base, "unconfigured")
				}
				if key == config.EnvAgent {
					return "alice"
				}
				return ""
			}
		case "missing-device":
			raw, err := os.ReadFile(w.paths.AgentFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			var c config.Credentials
			_ = json.Unmarshal(raw, &c)
			c.DeviceID = ""
			raw, _ = json.Marshal(c)
			if os.WriteFile(w.paths.AgentFile("alice"), raw, 0600) != nil {
				t.Fatal("metadata")
			}
		}
		if a.Run(context.Background(), []string{"post-operation", "status", "op", "--json"}) == 0 {
			t.Fatal("wrong own metadata read operation")
		}
	}
	if w.calls.Load() != calls || w.store.reads.Load() != providers {
		t.Fatal("status refusal called HTTP/provider")
	}
	if w.record().Outcome != "acknowledged" {
		t.Fatal("status refusal erased ACK")
	}
}

func TestDurablePostOperationIDCannotRetainKnownSecret(t *testing.T) {
	w := operationWorldNew(t)
	a, out, diag := w.app()
	args := operationArgs()
	args[5] = "REDACTION_PLACEHOLDER"
	if a.Run(context.Background(), args) != 2 || w.sends.Load() != 0 {
		t.Fatal("known provider secret accepted as operation ID")
	}
	if strings.Contains(out.String()+diag.String(), "REDACTION_PLACEHOLDER") {
		t.Fatal("secret ID in diagnostics")
	}
	if _, err := os.Stat(filepath.Join(w.paths.Base, "post-operations", "agents", "alice", "REDACTION_PLACEHOLDER.json")); !os.IsNotExist(err) {
		t.Fatal("secret ID persisted")
	}
}
