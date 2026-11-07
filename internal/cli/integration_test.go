package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// itRoom is the only room the integration test writes to.
const itRoom = "a2a-swarm"

// itOperator is the existing agent that opens the topic and invites the throwaway agent.
const itOperator = "local-models-e9"

// itEnv runs the real CLI against the real local homeserver from an isolated
// temporary config dir, so the real ~/.config/a2a-swarm is never modified.
type itEnv struct {
	t         *testing.T
	dir       string
	secrets   []string
	passwords map[string]string
}

func (e *itEnv) run(args ...string) result {
	e.t.Helper()
	var out, errb bytes.Buffer
	a := &App{
		Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""),
		Getenv: func(k string) string {
			if k == config.EnvConfigDir {
				return e.dir
			}
			return ""
		},
		HTTP: &http.Client{Transport: registrationCapture{env: e, base: http.DefaultTransport}},
		Now:  time.Now,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r := result{code: a.Run(ctx, args), stdout: out.String(), stderr: errb.String()}
	for _, s := range e.secrets {
		if s != "" && (strings.Contains(r.stdout, s) || strings.Contains(r.stderr, s)) {
			e.t.Fatalf("secret leaked by %v", args)
		}
	}
	e.t.Logf("a2a %s -> exit %d\n%s%s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
	return r
}

func copySecretFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("integration prerequisites missing: %v", err)
	}
	os.MkdirAll(filepath.Dir(dst), 0o700)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationRealHomeserver proves the CLI works against the real local
// tuwunel server: token registration of a throwaway agent, invite and join,
// topic creation, a question addressed to the new agent that wakes its wait,
// an answer that wakes the operator, threaded read, no re-emit after restart,
// and presence. It writes only to #a2a-swarm (plus the new account's presence)
// and at the end leaves the room, deactivates the throwaway account, and deletes
// its credential file. Gated by A2A_MATRIX_IT=1.
func TestIntegrationRealHomeserver(t *testing.T) {
	if os.Getenv("A2A_MATRIX_IT") != "1" {
		t.Skip("set A2A_MATRIX_IT=1 to run against the local homeserver")
	}
	src, err := config.ResolvePaths(func(k string) string { return os.Getenv(k) })
	if err != nil {
		t.Fatal(err)
	}
	e := &itEnv{t: t, dir: t.TempDir()}
	dst := config.Paths{Base: e.dir}
	copySecretFile(t, src.EnvFile(), dst.EnvFile())
	copySecretFile(t, src.AgentFile(itOperator), dst.AgentFile(itOperator))
	mx, _ := config.LoadMatrix(dst, func(string) string { return "" })
	op, err := config.LoadCredentials(dst, itOperator)
	if err != nil {
		t.Fatal(err)
	}
	e.secrets = append(e.secrets, mx.RegistrationToken, op.AccessToken, op.Password)

	var rnd [3]byte
	rand.Read(rnd[:])
	name := "it-" + hex.EncodeToString(rnd[:])
	slug := "it-" + hex.EncodeToString(rnd[:])

	// 1. Register the throwaway agent; the credential file is 0600 and never overwritten.
	e.run("register", name).ok(t)
	creds, err := config.LoadCredentials(dst, name)
	if err != nil {
		t.Fatal(err)
	}
	e.secrets = append(e.secrets, creds.AccessToken, creds.Password)
	// Runs last (defers are LIFO, the room leave below runs first): deactivate the
	// throwaway account and delete its credential file, so no account is left behind.
	defer func() {
		cur, err := config.LoadCredentials(dst, name)
		if err != nil {
			cur = creds
		}
		c := matrix.New(cur.Homeserver, cur.AccessToken)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.Deactivate(ctx, cur.UserID, e.passwords[name]); err != nil {
			t.Errorf("cleanup: deactivate %s: %v", cur.UserID, err)
		}
		if err := os.Remove(dst.AgentFile(name)); err != nil {
			t.Errorf("cleanup: remove credential file: %v", err)
		}
	}()
	e.run("register", name).failsWith(t, ExitError, "refusing to overwrite")
	tuitestkit.ContainsStr(t, e.run("--as", name, "whoami").ok(t).stdout, "@"+name+":")

	// 2. Take wait positions for both agents (first run never replays history).
	e.run("--as", name, "wait", "--timeout", "1s").ok(t)
	e.run("--as", itOperator, "wait", "--room", itRoom, "--timeout", "1s").ok(t)

	// 3. Invite wakes the new agent; it joins.
	e.run("--as", itOperator, "invite", itRoom, name).ok(t)
	r := e.run("--as", name, "wait", "--timeout", "30s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "invite ")
	e.run("--as", name, "join", itRoom).ok(t)
	defer func() {
		c := matrix.New(creds.Homeserver, creds.AccessToken)
		id, err := c.ResolveAlias(context.Background(), "#"+itRoom+":"+creds.ServerName())
		if err == nil {
			err = c.Leave(context.Background(), id)
		}
		if err != nil {
			t.Logf("cleanup: leave %s: %v", itRoom, err)
		}
	}()

	// 4. The operator opens a topic and asks the new agent a question.
	root := strings.TrimSpace(e.run("--as", itOperator, "topic", "new", itRoom, slug,
		"[it] throwaway a2a CLI integration test", "--body", "Automated test of the a2a CLI; safe to ignore.").ok(t).stdout)
	tuitestkit.ContainsStr(t, e.run("--as", itOperator, "topics", itRoom).ok(t).stdout, slug+"\t"+root+"\t0\t")
	q := strings.TrimSpace(e.run("--as", itOperator, "post", itRoom, "--topic", slug, "--kind", "question",
		"--to", name, "integration ping: please ack").ok(t).stdout)

	// 5. The question wakes the new agent (addressed by name).
	r = e.run("--as", name, "wait", "--room", itRoom, "--timeout", "30s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "to #"+itRoom+" "+q)
	tuitestkit.ContainsStr(t, r.stdout, "kind=question")

	// 6. The answer wakes the operator (a reply in a thread it opened).
	a := strings.TrimSpace(e.run("--as", name, "post", itRoom, "--topic", slug, "--kind", "ack", "--re", q, "ack from "+name).ok(t).stdout)
	found := false
	for i := 0; i < 5 && !found; i++ {
		r = e.run("--as", itOperator, "wait", "--room", itRoom, "--timeout", "30s").ok(t)
		found = strings.Contains(r.stdout, a)
	}
	if !found {
		t.Fatalf("operator wait never reported the answer %s", a)
	}

	// 7. Read shows the root and both replies, oldest first.
	out := e.run("--as", name, "read", itRoom, "--topic", slug).ok(t).stdout
	iq, ia := strings.Index(out, "integration ping"), strings.Index(out, "ack from "+name)
	if !strings.Contains(out, "[topic] "+slug) || iq < 0 || ia < iq {
		t.Fatalf("read output wrong:\n%s", out)
	}
	tuitestkit.ContainsStr(t, e.run("--as", itOperator, "topics", itRoom).ok(t).stdout, slug+"\t"+root+"\t2\t")

	// 8. A restarted wait does not re-emit what was already reported.
	r = e.run("--as", name, "wait", "--room", itRoom, "--timeout", "2s").ok(t)
	tuitestkit.NotContainsStr(t, r.stdout, q)

	// 9. Negative paths against the real server.
	e.run("--as", name, "post", itRoom, "--topic", "no-such-topic-"+slug, "x").failsWith(t, ExitError, "not found")
	e.run("--as", name, "read", "no-such-room-"+slug).failsWith(t, ExitError, "unknown room alias")

	// 10. Legacy presence still round-trips through Matrix; status get now
	// reports independently published liveness and is covered by fake-server tests.
	e.run("--as", name, "status", "set", "idle", "integration test").ok(t)
	presenceCtx, presenceCancel := context.WithTimeout(context.Background(), time.Minute)
	defer presenceCancel()
	presence, err := matrix.New(creds.Homeserver, creds.AccessToken).GetPresence(presenceCtx, creds.UserID)
	if err != nil || presence.Presence != "unavailable" || presence.StatusMsg != "idle: integration test" {
		t.Fatalf("legacy presence round-trip: err=%v presence=%+v", err, presence)
	}
}

// Registration passwords survive only in this gated test's memory, so cleanup
// can satisfy Matrix UIA without making production persist a password again.
type registrationCapture struct {
	env  *itEnv
	base http.RoundTripper
}

func (r registrationCapture) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && req.URL.Path == "/_matrix/client/v3/register" && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer body.Close()
		var data struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(body).Decode(&data); err != nil {
			return nil, err
		}
		if data.Password != "" {
			if r.env.passwords == nil {
				r.env.passwords = map[string]string{}
			}
			r.env.passwords[data.Username] = data.Password
			r.env.secrets = append(r.env.secrets, data.Password)
		}
	}
	return r.base.RoundTrip(req)
}
