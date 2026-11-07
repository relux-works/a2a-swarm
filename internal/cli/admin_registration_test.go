package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// This sentinel is deliberately not a pattern recognized by generic redaction.
// Assertions never print it or raw server captures, even on failure.
const admin198Secret = "Ax7Qc9De4Fg6Hi8J"
const admin198Room = "!admin:test.invalid"
const admin198Bot = "@server:test.invalid"
const admin198User = "@admin:test.invalid"
const admin198Normal = "!normal:test.invalid"

type admin198Fixture struct {
	t                                                        *testing.T
	root                                                     string
	c                                                        config.Connection
	store                                                    *namedTestStore
	server                                                   *httptest.Server
	now                                                      time.Time
	mode                                                     string
	mu                                                       sync.Mutex
	issues, auths, registrations, redactions, sends, history int
	issued                                                   bool
	wire                                                     []byte
	logs                                                     bytes.Buffer
}

func newAdmin198(t *testing.T, mode string) *admin198Fixture {
	return newAdmin198Backend(t, mode, "keychain")
}

func newAdmin198Backend(t *testing.T, mode, backend string) *admin198Fixture {
	t.Helper()
	f := &admin198Fixture{t: t, root: t.TempDir(), now: time.Now().UTC().Truncate(time.Millisecond), mode: mode, store: &namedTestStore{items: map[config.SecretReference]string{}}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.c = config.Connection{ID: "private-test", URL: f.server.URL, ServerName: "test.invalid", Store: config.CredentialStore{Backend: "keychain", AccessTokenService: "test.access", PasswordService: "test.password"}, AdminRegistration: &config.AdminRegistration{Method: "tuwunel-v1.9.4-admin-room", AdminUserID: admin198User, ServerBotUserID: admin198Bot, RoomID: admin198Room, ExpectedMembers: []string{admin198User, admin198Bot}, ReplyDeadlineSeconds: 1}}
	if backend == "file" {
		f.c.Store = config.CredentialStore{Backend: "file"}
	}
	f.save()
	b, _ := f.c.Binding("admin", false)
	p, _ := f.c.Paths(config.Paths{Base: f.root}, "admin", false)
	creds := config.Credentials{UserID: b.UserID, Homeserver: f.c.URL, ConnectionBinding: &b}
	if backend == "file" {
		creds.AccessToken = "isolated-admin-session"
	}
	if err := config.PublishBoundCredentials(p, "admin", false, creds); err != nil {
		t.Fatal("fixture metadata setup failed")
	}
	if backend == "keychain" {
		f.store.items[config.SecretReference{Service: f.c.Store.AccessTokenService, Account: admin198User}] = "isolated-admin-session"
	}
	prior := log.Writer()
	log.SetOutput(&f.logs)
	t.Cleanup(func() { log.SetOutput(prior) })
	return f
}
func (f *admin198Fixture) save() {
	r := config.ConnectionRegistry{SchemaVersion: 1, Current: "test", Profiles: map[string]config.Connection{"test": f.c}}
	b, _ := json.Marshal(r)
	if os.WriteFile(filepath.Join(f.root, "connections.json"), b, 0600) != nil {
		f.t.Fatal("fixture configuration setup failed")
	}
}
func (f *admin198Fixture) app(out, err io.Writer) *App {
	a := NewApp()
	a.Stdout = out
	a.Stderr = err
	a.HTTP = f.server.Client()
	a.Now = func() time.Time { return f.now }
	a.connectionSecretStore = f.store
	a.Getenv = func(k string) string {
		if k == config.EnvConfigDir {
			return f.root
		}
		if k == config.EnvAgent {
			return "admin"
		}
		return ""
	}
	return a
}
func (f *admin198Fixture) paths() config.Paths {
	p, _ := f.c.Paths(config.Paths{Base: f.root}, "admin", false)
	return p
}
func (f *admin198Fixture) reply() matrix.Event {
	body := fmt.Sprintf("New registration token issued: `%s` - Token used 0 times. Expires after 1 uses or in 4.98 minutes (%s).", admin198Secret, f.now.Add(5*time.Minute).Truncate(time.Second).Format("2006-01-02 15:04:05"))
	content := map[string]any{"msgtype": "m.notice", "body": body, "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$command"}}}
	e := matrix.Event{EventID: "$reply", Type: "m.room.message", RoomID: admin198Room, Sender: admin198Bot, OriginServerTS: f.now.UnixMilli()}
	switch f.mode {
	case "unparsable":
		content["body"] = "Unexpected token response " + admin198Secret
	case "wrong-sender":
		e.Sender = "@other:test.invalid"
	case "unrelated":
		content["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]any{"event_id": "$other"}}
	case "late":
		e.OriginServerTS = f.now.Add(2 * time.Second).UnixMilli()
	case "stale":
		e.OriginServerTS = f.now.Add(-time.Second).UnixMilli()
	case "wrong-room":
		e.RoomID = admin198Normal
	case "thread":
		content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": "$command"}
	case "extra-body":
		content["body"] = body + "\n" + admin198Secret
	case "bad-expiry":
		content["body"] = strings.Replace(body, "4.98 minutes", "9.98 minutes", 1)
	}
	e.Content, _ = json.Marshal(content)
	return e
}
func (f *admin198Fixture) eventForDelivery(room string) matrix.Event {
	content := map[string]any{"msgtype": "m.notice", "body": admin198Secret, "m.mentions": map[string]any{"user_ids": []string{admin198User}}, ContentKey: Header{V: 1, Type: "message", From: "server", To: []string{"admin"}, Kind: "question", Slug: "alpha"}}
	e := matrix.Event{EventID: "$protected", Type: "m.room.message", RoomID: room, Sender: admin198Bot, OriginServerTS: f.now.UnixMilli()}
	if room == admin198Normal {
		content["body"] = "ordinary-room-control"
		e.EventID = "$ordinary"
	}
	e.Content, _ = json.Marshal(content)
	return e
}
func (f *admin198Fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	f.wire = append(f.wire, []byte(r.URL.String())...)
	f.wire = append(f.wire, raw...)
	respond := func(x any) { _ = json.NewEncoder(w).Encode(x) }
	failure := func() { w.WriteHeader(403); respond(map[string]any{"errcode": "M_FORBIDDEN", "error": admin198Secret}) }
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/account/whoami"):
		respond(map[string]string{"user_id": admin198User, "device_id": "test"})
	case strings.HasSuffix(path, "/state"):
		empty := ""
		create, _ := json.Marshal(map[string]any{"m.federate": f.mode == "federated", "room_version": "11"})
		events := []matrix.Event{{Type: "m.room.create", RoomID: admin198Room, Sender: admin198Bot, StateKey: &empty, Content: create}}
		members := []string{admin198User, admin198Bot}
		if f.mode == "extra-member" {
			members = append(members, "@extra:test.invalid")
		}
		for _, m := range members {
			key := m
			events = append(events, matrix.Event{Type: "m.room.member", RoomID: admin198Room, Sender: m, StateKey: &key, Content: json.RawMessage(`{"membership":"join"}`)})
		}
		respond(events)
	case strings.Contains(path, "/account_data/"):
		subs := []Sub{}
		if f.mode == "subscribed" {
			subs = append(subs, Sub{ID: "unsafe", SubSelector: SubSelector{Room: admin198Room, AllEvents: true}})
		}
		if f.mode == "broad-subscription" {
			subs = append(subs, Sub{ID: "unsafe", SubSelector: SubSelector{From: admin198Bot}})
		}
		respond(SubList{V: 1, Subs: subs})
	case strings.Contains(path, "/redact/"):
		f.redactions++
		if f.mode == "redaction-failure" || f.mode == "expired-redaction-failure" {
			failure()
			return
		}
		respond(map[string]string{"event_id": "$redaction"})
	case strings.Contains(path, "/send/"):
		f.sends++
		if bytes.Contains(raw, []byte(adminIssueCommand)) {
			f.issues++
			if f.mode == "session-401" {
				w.WriteHeader(401)
				respond(map[string]string{"errcode": "M_UNKNOWN_TOKEN", "error": admin198Secret})
				return
			}
			if f.mode == "server-code-secret" {
				w.WriteHeader(403)
				respond(map[string]string{"errcode": admin198Secret, "error": admin198Secret})
				return
			}
			if f.mode == "non-admin" {
				failure()
				return
			}
			f.issued = true
			respond(map[string]string{"event_id": "$command"})
		} else {
			respond(map[string]string{"event_id": "$sent"})
		}
	case strings.HasSuffix(path, "/sync"):
		if f.mode == "delivery" {
			respond(map[string]any{"next_batch": "next", "rooms": map[string]any{"join": map[string]any{admin198Room: map[string]any{"timeline": map[string]any{"events": []matrix.Event{f.eventForDelivery(admin198Room)}}}, admin198Normal: map[string]any{"timeline": map[string]any{"events": []matrix.Event{f.eventForDelivery(admin198Normal)}}}}}})
			return
		}
		if !f.issued {
			respond(map[string]any{"next_batch": "baseline", "rooms": map[string]any{}})
			return
		}
		if f.mode == "deadline" {
			f.mu.Unlock()
			select {
			case <-r.Context().Done():
			case <-time.After(1200 * time.Millisecond):
			}
			f.mu.Lock()
			return
		}
		ev := f.reply()
		events := []matrix.Event{ev}
		if f.mode == "two-replies" {
			ev.EventID = "$second"
			events = append(events, ev)
		}
		respond(map[string]any{"next_batch": "after", "rooms": map[string]any{"join": map[string]any{admin198Room: map[string]any{"timeline": map[string]any{"events": events}}}}})
	case strings.HasSuffix(path, "/register"):
		f.registrations++
		var req struct {
			Auth map[string]any `json:"auth"`
		}
		_ = json.Unmarshal(raw, &req)
		if req.Auth == nil {
			w.WriteHeader(401)
			respond(map[string]any{"session": "one", "flows": []any{map[string]any{"stages": []string{"m.login.registration_token"}}}})
			return
		}
		f.auths++
		if req.Auth["token"] != admin198Secret {
			w.WriteHeader(400)
			respond(map[string]string{"errcode": "M_INVALID_PARAM"})
			return
		}
		if f.mode == "expired" || f.mode == "used" || f.mode == "expired-redaction-failure" {
			failure()
			return
		}
		respond(map[string]string{"user_id": "@newbie:test.invalid", "access_token": "isolated-new-account-access", "device_id": "new-device"})
	case strings.Contains(path, "/event/"):
		f.history++
		room := admin198Room
		if strings.Contains(path, admin198Normal) {
			room = admin198Normal
		}
		respond(f.eventForDelivery(room))
	case strings.Contains(path, "/relations/"):
		f.history++
		respond(map[string]any{"chunk": []matrix.Event{}})
	case strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, "/threads"):
		f.history++
		room := admin198Room
		if strings.Contains(path, admin198Normal) {
			room = admin198Normal
		}
		respond(map[string]any{"chunk": []matrix.Event{f.eventForDelivery(room)}})
	case strings.HasSuffix(path, "/joined_rooms"):
		respond(map[string]any{"joined_rooms": []string{admin198Room, admin198Normal}})
	case strings.Contains(path, "/state/m.room.canonical_alias/"):
		respond(map[string]string{"alias": "#ordinary:test.invalid"})
	default:
		w.WriteHeader(404)
		respond(map[string]string{"errcode": "M_NOT_FOUND"})
	}
}
func safeAdmin198Absent(t *testing.T, data []byte, channel string) {
	t.Helper()
	if bytes.Contains(data, []byte(admin198Secret)) {
		t.Fatal("secret sentinel escaped through " + channel)
	}
}

func assertAdmin198Argv(t *testing.T, goos string) {
	t.Helper()
	safeAdmin198Absent(t, []byte(strings.Join(os.Args, "\x00")), "argv")
	// The additional /proc observation is Linux-only; os.Args is checked everywhere.
	if goos == "linux" {
		cmdline, e := os.ReadFile("/proc/self/cmdline")
		if e != nil {
			t.Fatal("argv observation unavailable")
		}
		safeAdmin198Absent(t, cmdline, "proc argv")
	}
}

func TestAdmin198ArgvObservation(t *testing.T) {
	const controlEnv = "A2A_ADMIN198_ARGV_CONTROL"
	if control := os.Getenv(controlEnv); control != "" {
		// Mutate os.Args only in a dedicated child process, never alongside other tests.
		goos := "darwin"
		switch control {
		case "non-linux-clean":
			os.Args = append(os.Args, "ordinary-argv-control")
		case "non-linux-secret":
			os.Args = append(os.Args, admin198Secret)
		case "linux-proc-secret":
			goos = "linux"
			os.Args = []string{"admin198-argv-control"} // Leave the plant only in /proc.
		default:
			t.Fatal("unknown argv control")
		}
		assertAdmin198Argv(t, goos)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("argv control executable unavailable")
	}
	controls := []struct{ name, diagnostic string }{
		{"non-linux-clean", ""},
		{"non-linux-secret", "secret sentinel escaped through argv"},
	}
	if runtime.GOOS == "linux" {
		controls = append(controls, struct{ name, diagnostic string }{"linux-proc-secret", "secret sentinel escaped through proc argv"})
	}
	for _, control := range controls {
		t.Run(control.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := []string{"-test.run=^TestAdmin198ArgvObservation$", "-test.count=1"}
			if control.name == "linux-proc-secret" {
				args = append(args, admin198Secret)
			}
			cmd := exec.CommandContext(ctx, executable, args...)
			cmd.Env = append(os.Environ(), controlEnv+"="+control.name)
			output, err := cmd.CombinedOutput()
			if control.diagnostic == "" {
				if err != nil {
					t.Fatal("clean argv control failed")
				}
				return
			}
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 || !bytes.Contains(output, []byte(control.diagnostic)) {
				t.Fatal("argv control did not detect the planted sentinel")
			}
		})
	}
}

func (f *admin198Fixture) assertNoExports(out, err []byte) {
	f.t.Helper()
	safeAdmin198Absent(f.t, out, "stdout")
	safeAdmin198Absent(f.t, err, "stderr")
	safeAdmin198Absent(f.t, f.logs.Bytes(), "logs")
	assertAdmin198Argv(f.t, runtime.GOOS)
	walkErr := filepath.WalkDir(f.root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			channel := "state"
			if strings.Contains(path, "mailbox") {
				channel = "mailbox"
			}
			safeAdmin198Absent(f.t, b, channel)
		}
		return nil
	})
	if walkErr != nil {
		f.t.Fatal("persistent export check unavailable")
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	for _, v := range f.store.items {
		safeAdmin198Absent(f.t, []byte(v), "credential store")
	}
}

func TestAdmin198Registration(t *testing.T) {
	for _, mode := range []string{"positive", "thread", "expired", "used", "non-admin", "unparsable", "wrong-sender", "unrelated", "late", "stale", "extra-member", "federated", "wrong-room", "extra-body", "bad-expiry", "subscribed", "broad-subscription", "deadline", "redaction-failure", "expired-redaction-failure", "session-401", "server-code-secret", "two-replies"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmin198(t, mode)
			var out, err bytes.Buffer
			a := f.app(&out, &err)
			exit := a.Run(context.Background(), []string{"--as", "admin", "connection", "register", "newbie", "--via-admin-room"})
			positive := mode == "positive" || mode == "thread" || mode == "redaction-failure"
			if (exit == 0) != positive {
				t.Log(regexp.MustCompile(`stage=[a-z-]+ errcode=[A-Z_]+ HTTP=[0-9]+`).FindString(err.String()))
				t.Fatal("registration outcome differs from independent server scenario")
			}
			f.mu.Lock()
			issues, auths, redactions := f.issues, f.auths, f.redactions
			wire := append([]byte(nil), f.wire...)
			f.mu.Unlock()
			preflight := mode == "extra-member" || mode == "federated" || mode == "subscribed" || mode == "broad-subscription"
			if preflight && issues != 0 || !preflight && issues != 1 {
				t.Log(regexp.MustCompile(`stage=[a-z-]+ errcode=[A-Z_]+ HTTP=[0-9]+`).FindString(err.String()))
				t.Fatal("issue-attempt bound violated")
			}
			shouldRegister := positive || mode == "expired" || mode == "used" || mode == "expired-redaction-failure"
			if shouldRegister && auths != 1 || !shouldRegister && auths != 0 {
				t.Fatal("registration authentication-attempt bound violated")
			}
			if shouldRegister && redactions != 2 {
				t.Fatal("command and reply cleanup were not both attempted")
			}
			if mode == "session-401" && !strings.Contains(err.String(), "stage=issue errcode=M_UNKNOWN_TOKEN HTTP=401") {
				t.Fatal("401 server evidence was lost")
			}
			if mode == "server-code-secret" && !strings.Contains(err.String(), "stage=issue errcode=SERVER_ERROR HTTP=403") {
				t.Fatal("unknown server code was not safely classified")
			}
			if mode == "non-admin" && !strings.Contains(err.String(), "stage=issue errcode=M_FORBIDDEN HTTP=403") {
				t.Fatal("server administration refusal lost stage/code/status")
			}
			if (mode == "expired" || mode == "used" || mode == "expired-redaction-failure") && !strings.Contains(err.String(), "stage=register errcode=M_FORBIDDEN HTTP=403") {
				t.Fatal("registration refusal lost stage/code/status")
			}
			if mode == "redaction-failure" || mode == "expired-redaction-failure" {
				if !strings.Contains(err.String(), "rotation is not needed") || !strings.Contains(err.String(), "may still be unused") {
					t.Fatal("retention warning missing honest failure bound")
				}
			}
			if !positive && strings.Contains(out.String(), "registered") {
				t.Fatal("failed registration reported success")
			}
			if shouldRegister && !bytes.Contains(wire, []byte(admin198Secret)) {
				t.Fatal("fake server did not receive a token control")
			}
			f.assertNoExports(out.Bytes(), err.Bytes())
			if shouldRegister && a.secrets.Scrub(admin198Secret) == admin198Secret {
				t.Fatal("token missing from redaction set")
			}
		})
	}
}

func TestAdmin198Prerequisites(t *testing.T) {
	for _, mode := range []string{"unconfigured", "wrong-session", "implicit-admin", "wrong-method", "deadline-missing", "members-missing", "remote-room", "store-conflict", "mailbox-existing"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmin198(t, "positive")
			var out, err bytes.Buffer
			a := f.app(&out, &err)
			args := []string{"--as", "admin", "connection", "register", "newbie", "--via-admin-room"}
			switch mode {
			case "unconfigured":
				f.c.AdminRegistration = nil
			case "wrong-session":
				args[1] = "other"
			case "implicit-admin":
				args = args[2:]
			case "wrong-method":
				f.c.AdminRegistration.Method = "guessed"
			case "deadline-missing":
				f.c.AdminRegistration.ReplyDeadlineSeconds = 0
			case "members-missing":
				f.c.AdminRegistration.ExpectedMembers = nil
			case "remote-room":
				f.c.AdminRegistration.RoomID = "!admin:foreign.invalid"
			case "store-conflict":
				f.store.items[config.SecretReference{Service: f.c.Store.PasswordService, Account: "@newbie:test.invalid"}] = "pre-existing-password"
			case "mailbox-existing":
				raw, _ := json.Marshal(f.eventForDelivery(admin198Room))
				mb, _ := config.OpenMailbox(f.paths(), "admin")
				if _, e := mb.Append([]config.MailEntry{{ID: "$old", Kind: config.MailEvent, Room: admin198Room, Event: raw}}); e != nil {
					t.Fatal("mailbox plant failed")
				}
			}
			f.save()
			if a.Run(context.Background(), args) == 0 {
				t.Fatal("missing prerequisite admitted registration")
			}
			if f.issues != 0 || f.auths != 0 {
				t.Fatal("prerequisite refusal performed server action")
			}
			safeAdmin198Absent(t, out.Bytes(), "prerequisite stdout")
			safeAdmin198Absent(t, err.Bytes(), "prerequisite stderr")
		})
	}
}

func TestAdmin198ReadExclusions(t *testing.T) {
	for _, args := range [][]string{
		{"read", admin198Room}, {"read", admin198Room, "--json"}, {"read", admin198Room, "--event", "$protected"}, {"read", admin198Room, "--event", "$protected", "--json"}, {"read", admin198Room, "--topic", "$protected"}, {"read", admin198Room, "--event", "$protected", "--json", "--verification-input"}, {"topics", admin198Room}, {"files", admin198Room}, {"fetch", admin198Room, "$protected"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			f := newAdmin198(t, "delivery")
			var out, err bytes.Buffer
			a := f.app(&out, &err)
			all := append([]string{"--as", "admin"}, args...)
			if a.Run(context.Background(), all) == 0 {
				t.Fatal("protected history was not refused")
			}
			if f.history != 0 {
				t.Fatal("history fetched before configured-room refusal")
			}
			f.assertNoExports(out.Bytes(), err.Bytes())
		})
	}
	// A genuine unprotected client exercises the historical read path and proves
	// the server fixture carries the sentinel; it is never printed or exported.
	t.Run("unprotected-control", func(t *testing.T) {
		f := newAdmin198(t, "delivery")
		c := matrix.New(f.server.URL, "isolated")
		ev, e := c.GetEvent(context.Background(), admin198Room, "$protected")
		if e != nil || !bytes.Contains(ev.Content, []byte(admin198Secret)) {
			t.Fatal("unprotected read control did not expose fixture in memory")
		}
		v, e := c.GetVerificationEvent(context.Background(), admin198Room, "$protected")
		if e != nil || !bytes.Contains(v.Content, []byte(admin198Secret)) {
			t.Fatal("unprotected verification control failed")
		}
	})
}

func TestAdmin198DeliveryExclusions(t *testing.T) {
	for _, command := range []string{"inbox", "doorbell", "project", "archive", "archive-all", "ordinary-read", "old-mailbox"} {
		t.Run(command, func(t *testing.T) {
			f := newAdmin198(t, "delivery")
			var out, err bytes.Buffer
			a := f.app(&out, &err)
			p := f.paths()
			if _, e := config.UpdateState(p, "admin", func(st *config.State) error { st.NextBatch = "baseline"; return nil }); e != nil {
				t.Fatal("delivery cursor setup failed")
			}
			var args []string
			switch command {
			case "inbox":
				args = []string{"inbox", "--json"}
			case "doorbell":
				a.Stdin = strings.NewReader(`{}`)
				args = []string{"hook", "prompt", "--runtime", "claude"}
			case "project":
				args = []string{"project", "--system", "!target:test.invalid", "--source", admin198Room, "--source", admin198Normal, "--once", "--poll", "1ms", "--kind", "question"}
			case "archive-all":
				args = []string{"archive", "all", "--dir", filepath.Join(f.root, "archive")}
			case "ordinary-read":
				args = []string{"read", admin198Normal, "--event", "$ordinary", "--json"}
			case "archive":
				args = []string{"archive", admin198Room, "--dir", filepath.Join(f.root, "archive")}
			case "old-mailbox":
				raw, _ := json.Marshal(f.eventForDelivery(admin198Room))
				mb, _ := config.OpenMailbox(p, "admin")
				if _, e := mb.Append([]config.MailEntry{{ID: "$old", Kind: config.MailEvent, Room: admin198Room, Rel: "mention", Event: raw}}); e != nil {
					t.Fatal("retained mailbox plant failed")
				}
				args = []string{"inbox", "--json"}
			}
			exit := a.Run(context.Background(), append([]string{"--as", "admin"}, args...))
			if command == "old-mailbox" {
				if exit == 0 {
					t.Fatal("old protected mailbox was displayed")
				}
				safeAdmin198Absent(t, out.Bytes(), "retained mailbox stdout")
				safeAdmin198Absent(t, err.Bytes(), "retained mailbox stderr")
				return
			}
			if exit != 0 {
				t.Fatal("exclusion command failed instead of exercising valid control")
			}
			f.assertNoExports(out.Bytes(), err.Bytes())
			mb, e := config.OpenMailbox(p, "admin")
			if e != nil {
				t.Fatal("mailbox inspection failed")
			}
			for _, entry := range mb.Pending() {
				if entry.Room == admin198Room {
					t.Fatal("protected event enqueued")
				}
			}
			if command == "ordinary-read" && !strings.Contains(out.String(), "ordinary-room-control") {
				t.Fatal("ordinary read control suppressed")
			}
			if command == "archive-all" && f.history == 0 {
				t.Fatal("normal archive control did not read history")
			}
			if command == "inbox" && !strings.Contains(out.String(), "ordinary-room-control") {
				t.Fatal("ordinary inbox control was suppressed")
			}
			if command == "doorbell" && !strings.Contains(out.String(), "1 new peer event") {
				t.Fatal("ordinary doorbell control missing or protected event counted")
			}
			if command == "project" && f.sends != 1 {
				t.Fatal("projector failed normal-source control or sent protected source")
			}
			if command == "archive" && f.history != 0 {
				t.Fatal("archive fetched excluded history")
			}
		})
	}
}

// The unprotected client exercises the historical path, not a copy of a guard.
func TestAdmin198RelevanceAndOwnerDMExclusions(t *testing.T) {
	f := newAdmin198(t, "delivery")
	var out, err bytes.Buffer
	a := f.app(&out, &err)
	a.as = "admin"
	s, e := a.session()
	if e != nil {
		t.Fatal("configured session unavailable")
	}
	rawClient := matrix.New(f.server.URL, "isolated")
	response, e := rawClient.Sync(context.Background(), "baseline", 0, "")
	if e != nil {
		t.Fatal("unprotected sync control failed")
	}
	events := response.Rooms.Join[admin198Room].Timeline.Events
	if len(events) != 1 {
		t.Fatal("unprotected sync control lacks protected event")
	}
	if rel, _, _ := classify(events[0], admin198Room, admin198User, "admin", config.State{}, nil); rel == "" {
		t.Fatal("protected fixture is not genuinely mailbox-relevant")
	}
	batch, e := s.collectBatch(context.Background(), response, "baseline", config.State{}, nil, map[string]bool{admin198Room: true, admin198Normal: true}, map[string]string{})
	if e != nil {
		t.Fatal("batch exclusion failed")
	}
	if len(batch.entries) != 1 || batch.entries[0].Room != admin198Normal {
		t.Fatal("batch did not retain exactly the unrelated normal control")
	}
	files := ownerFiles(f.paths(), "admin")
	ownerPut(t, files.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: admin198Bot})
	ownerPut(t, files.path("json"), ownerDMConfig{V: 1, Generation: 1, Active: true, Owner: admin198Bot, Room: admin198Room, Mirror: true, Render: "outside-frame"})
	payload, _ := json.Marshal(map[string]string{"session_id": "isolated", "prompt_id": "one", "last_assistant_message": admin198Secret})
	a.ownerMirrorStop(context.Background(), "claude", hookInput{Payload: payload})
	if f.sends != 0 {
		t.Fatal("owner mirror posted to configured admin room")
	}
	if _, st, _ := ownerRead(files.path("journal")); st != nil {
		t.Fatal("excluded owner mirror persisted payload metadata")
	}
	if a.checkAdminRegistrationRoom(context.Background(), s) == nil {
		t.Fatal("owner-DM overlap admitted token issue")
	}
	f.assertNoExports(out.Bytes(), err.Bytes())
}

type admin198Transport struct {
	next                   http.RoundTripper
	app                    *App
	beforeRedactionFailure bool
	tokenInURL             bool
}

func (rt *admin198Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/register") && rt.app.secrets.Scrub(admin198Secret) == admin198Secret {
		rt.beforeRedactionFailure = true
	}
	if strings.Contains(r.URL.String(), admin198Secret) || strings.Contains(r.Header.Get("Authorization"), admin198Secret) {
		rt.tokenInURL = true
	}
	return rt.next.RoundTrip(r)
}
func TestAdmin198RedactionBeforeRegistration(t *testing.T) {
	f := newAdmin198(t, "positive")
	var out, err bytes.Buffer
	a := f.app(&out, &err)
	rt := &admin198Transport{next: f.server.Client().Transport, app: a}
	a.HTTP = &http.Client{Transport: rt}
	if a.Run(context.Background(), []string{"--as", "admin", "connection", "register", "newbie", "--via-admin-room"}) != 0 {
		t.Fatal("positive transport control failed")
	}
	if rt.beforeRedactionFailure {
		t.Fatal("registration sent before token entered redaction set")
	}
	if rt.tokenInURL {
		t.Fatal("token escaped into request URL or header")
	}
	f.assertNoExports(out.Bytes(), err.Bytes())
}

func TestAdmin198BootstrapReferenceUntouched(t *testing.T) {
	f := newAdmin198(t, "positive")
	reference := config.SecretReference{Service: "test.bootstrap", Account: "owner-admission"}
	f.c.RegistrationTokenRef = &reference
	f.store.items[reference] = "pre-existing-bootstrap-value"
	f.save()
	var out, err bytes.Buffer
	a := f.app(&out, &err)
	if a.Run(context.Background(), []string{"--as", "admin", "connection", "register", "newbie", "--via-admin-room"}) != 0 {
		t.Fatal("normal credential route control failed")
	}
	f.store.mu.Lock()
	for _, r := range append(append([]config.SecretReference(nil), f.store.reads...), f.store.creates...) {
		if r == reference {
			f.store.mu.Unlock()
			t.Fatal("RAM admission touched bootstrap reference")
		}
	}
	if f.store.items[reference] != "pre-existing-bootstrap-value" {
		f.store.mu.Unlock()
		t.Fatal("bootstrap store item changed")
	}
	f.store.mu.Unlock()
	f.assertNoExports(out.Bytes(), err.Bytes())
}

func TestAdmin198StrictReplyParser(t *testing.T) {
	f := newAdmin198(t, "positive")
	var out, err bytes.Buffer
	a := f.app(&out, &err)
	var content struct {
		Body string `json:"body"`
	}
	_ = json.Unmarshal(f.reply().Content, &content)
	variants := map[string]string{
		"expired-body":    strings.Replace(content.Body, f.now.Add(5*time.Minute).Truncate(time.Second).Format("2006-01-02 15:04:05"), f.now.Add(-time.Minute).Format("2006-01-02 15:04:05"), 1),
		"uses-unbounded":  strings.Replace(content.Body, "after 1 uses", "after 2 uses", 1),
		"extra-plaintext": content.Body + "\nextra",
		"wrong-alphabet":  strings.Replace(content.Body, admin198Secret, "invalid-token-xx", 1),
	}
	for name, body := range variants {
		t.Run(name, func(t *testing.T) {
			if _, e := a.parseAdminToken(body, f.now); e == nil {
				t.Fatal("strict parser accepted invalid pinned response")
			}
		})
	}
	if _, e := a.parseAdminToken(content.Body, f.now); e != nil {
		t.Fatal("pinned complete response control failed")
	}
	relations := map[string]json.RawMessage{
		"wrong-field-case": json.RawMessage(`{"m.in_reply_to":{"EVENT_ID":"$command"}}`),
		"extra-relation":   json.RawMessage(`{"m.in_reply_to":{"event_id":"$command"},"event_id":"$other"}`),
		"fallback-thread":  json.RawMessage(`{"rel_type":"m.thread","event_id":"$command","is_falling_back":true}`),
		"unrelated-thread": json.RawMessage(`{"rel_type":"m.thread","event_id":"$other"}`),
	}
	for name, raw := range relations {
		t.Run(name, func(t *testing.T) {
			if adminReplyRelates(raw, "$command") {
				t.Fatal("strict relation accepted an unrelated or ambiguous command binding")
			}
		})
	}
	f.assertNoExports(out.Bytes(), err.Bytes())
}
