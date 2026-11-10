//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/relux-works/a2a-swarm/internal/daemon"
)

const cmValidLine = `{"total":1,"types":{"to":1,"mention":0,"thread":0,"sub":0,"all":0,"redacted":0,"invite":0},"kinds":[],"names":[],"unknown":false,"more":0,"gaps":0}` + "\n"

func TestCMListenerRejectsUntrustedOutput(t *testing.T) {
	for _, bad := range []string{
		`{"body":"peer-body"}` + "\n",
		strings.Replace(cmValidLine, `"total":1`, `"total":-1`, 1),
		strings.Replace(cmValidLine, `"total":1`, `"total":1,"total":1`, 1),
		strings.Replace(cmValidLine, `"gaps":0`, `"gaps":0,"body":"peer-body"`, 1),
		strings.Repeat("x", noticeJSONMax+2),
	} {
		t.Run("closed-output", func(t *testing.T) {
			s := newC5Station(t, func(ctx context.Context, inv daemon.Invocation) int {
				if _, err := io.WriteString(inv.Stdout, cmValidLine); err != nil {
					return 1
				}
				_, _ = io.WriteString(inv.Stdout, bad)
				<-ctx.Done()
				return 0
			})
			l := cmStartC5(t, s)
			c := cmConnect(t, l)
			if string(cmRead(t, c))+"\n" != cmValidLine {
				t.Fatal("valid projection neighbour lost")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, err := c.Read(ctx)
			var closed websocket.CloseError
			if !errors.As(err, &closed) || closed.Reason != "daemon_unavailable" {
				t.Fatal("invalid engine output escaped or lacked a closed reason")
			}
		})
	}
}

func TestCMListenerMalformedHandshake(t *testing.T) {
	s := newC5Station(t, func(ctx context.Context, _ daemon.Invocation) int { <-ctx.Done(); return 0 })
	l := cmStartC5(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "http" + strings.TrimPrefix(l.address, "ws")
	r, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+l.capability)
	r.Header.Set("Origin", l.capability)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal("malformed handshake did not return")
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusUpgradeRequired || string(body) != "not_allowed\n" {
		t.Fatal("malformed handshake did not have exact closed reason")
	}
	cmConnect(t, l)
}

func TestCMListenerIdleReason(t *testing.T) {
	s := newC5Station(t, func(_ context.Context, inv daemon.Invocation) int { return inv.Refuse(daemon.IdleTimeout) })
	l := cmStartC5(t, s)
	c := cmConnect(t, l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Reason != "daemon_idle_timeout" {
		t.Fatal("idle timeout did not reach monitor as exact reason")
	}
}

func TestCMListenerGenerationCapability(t *testing.T) {
	s := newC5Station(t, func(ctx context.Context, _ daemon.Invocation) int { <-ctx.Done(); return 0 })
	first := cmStartC5(t, s)
	old := first.capability
	first.cancel()
	// Observe cleanup without consuming cmStart's completion signal.
	cmAwait(t, nil, func() bool {
		_, err := os.Stat(filepath.Join(s.home, "state", noticeEndpoint))
		return os.IsNotExist(err)
	})
	second := cmStartC5(t, s)
	if old == second.capability {
		t.Fatal("listener reused capability")
	}
	cmRefusal(t, second, old, "not_allowed", http.StatusForbidden)
	cmConnect(t, second)
}

func TestCMListenerRequestAllowlist(t *testing.T) {
	s := newC5Station(t, func(ctx context.Context, _ daemon.Invocation) int { <-ctx.Done(); return 0 })
	l := cmStartC5(t, s)
	for _, tc := range []struct{ method, suffix, auth string }{
		{"POST", "/", "valid"}, {"GET", "/extra", "valid"},
		{"GET", "/?cap=", "valid"}, {"GET", "/", "lowercase"}, {"GET", "/", "duplicate"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		url := "http" + strings.TrimPrefix(strings.TrimSuffix(l.address, "/"), "ws") + tc.suffix
		if tc.suffix == "/?cap=" {
			url += l.capability
		}
		r, err := http.NewRequestWithContext(ctx, tc.method, url, nil)
		if err != nil {
			cancel()
			t.Fatal("request creation failed")
		}
		auth := "Bearer " + l.capability
		if tc.auth == "lowercase" {
			auth = "bearer " + l.capability
		}
		r.Header.Set("Authorization", auth)
		if tc.auth == "duplicate" {
			r.Header.Add("Authorization", auth)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			cancel()
			t.Fatal("refusal request failed")
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		if response.StatusCode != http.StatusForbidden || string(body) != "not_allowed\n" {
			t.Fatal("request allowlist lacked exact refusal")
		}
	}
	cmConnect(t, l)
}

func TestCMListenerOptionRefusals(t *testing.T) {
	s := newC5Station(t, func(ctx context.Context, _ daemon.Invocation) int { <-ctx.Done(); return 0 })
	for _, args := range [][]string{
		{"watch", "--listen"}, {"watch", "--notices=false", "--listen"},
		{"watch", "--notices", "--listen", "operand"},
		{"watch", "--notices", "--listen", "--all"},
	} {
		r := s.run(context.Background(), "", args...)
		if r.code != 1 || r.stdout != "" || r.stderr != "a2a: daemon: not_allowed\n" {
			t.Fatal("invalid watch form lacked exact closed refusal")
		}
	}
	l := cmStartC5(t, s)
	cmConnect(t, l)
}

func TestCMListenerArgvCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plain", []string{"watch", "--notices", "--listen"}},
		{"terminator", []string{"watch", "--notices", "--listen", "--"}},
		{"json-false", []string{"watch", "--notices", "--listen", "--json=false"}},
		{"value-looks-like-flag", []string{"watch", "--notices", "--listen", "--room", "--json"}},
		{"selected-filtered", []string{"--as=identity", "watch", "--notices", "--listen", "--match-text", "a|b", "--from", "alice,@bob:test.local", "--kind", "notice", "--to-me-only", "--room", "room", "--all", "--"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newC5Station(t, func(ctx context.Context, inv daemon.Invocation) int {
				plan, err := daemon.Authorize(inv.Request.Argv, "identity")
				if err != nil {
					return 1
				}
				// Decode at the handler independently of the new serializer.
				var o struct {
					rooms                      stringList
					json, listen, notices, all bool
					predicates                 deliveryPredicates
				}
				fs := newFlagSet("handler-control")
				fs.Var(&o.rooms, "room", "")
				fs.BoolVar(&o.json, "json", false, "")
				fs.BoolVar(&o.listen, "listen", false, "")
				fs.BoolVar(&o.notices, "notices", false, "")
				fs.BoolVar(&o.all, "all", false, "")
				predicateFlags := addDeliveryPredicateFlags(fs)
				pos, err := parseInterspersed(fs, plan.Args)
				if err != nil || len(pos) != 0 {
					return 1
				}
				o.predicates, err = predicateFlags.check()
				if err != nil || !o.json || !o.listen || !o.notices {
					return 1
				}
				if tc.name == "value-looks-like-flag" && (len(o.rooms) != 1 || o.rooms[0] != "--json") {
					return 1
				}
				if tc.name == "selected-filtered" && (plan.Selection.As != "identity" || !o.all || len(o.rooms) != 1 || o.rooms[0] != "room" || !o.predicates.toMe || o.predicates.text == nil || o.predicates.text.String() != "a|b" || strings.Join(o.predicates.from, ",") != "alice,@bob:test.local" || strings.Join(o.predicates.kinds, ",") != "notice") {
					return 1
				}
				if _, err := io.WriteString(inv.Stdout, cmValidLine); err != nil {
					return 1
				}
				<-ctx.Done()
				return 0
			})
			l := cmStart(t, s.home, func(ctx context.Context, diag io.Writer) result {
				a := &App{Stdout: io.Discard, Stderr: diag, Getenv: func(k string) string {
					if k == "A2A_CONFIG_DIR" {
						return s.home
					}
					return ""
				}}
				return result{code: a.Run(ctx, tc.args)}
			})
			if string(cmRead(t, cmConnect(t, l)))+"\n" != cmValidLine {
				t.Fatal("valid grammar lost the closed projection neighbour")
			}
		})
	}
}

type cmBlockedDiagnostic struct {
	target  io.Writer
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *cmBlockedDiagnostic) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "daemon notice listener: ") {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
	return b.target.Write(p)
}

func TestCMDeathWhileDiagnosticBlocked(t *testing.T) {
	admitted := make(chan struct{})
	s := newC5Station(t, func(ctx context.Context, _ daemon.Invocation) int { close(admitted); <-ctx.Done(); return 0 })
	blocked := &cmBlockedDiagnostic{entered: make(chan struct{}), release: make(chan struct{})}
	l := cmStart(t, s.home, func(ctx context.Context, diag io.Writer) result {
		blocked.target = diag
		a := &App{Stdout: io.Discard, Stderr: blocked, Getenv: func(k string) string {
			if k == "A2A_CONFIG_DIR" {
				return s.home
			}
			return ""
		}}
		return result{code: a.Run(ctx, []string{"watch", "--notices", "--listen"})}
	})
	var release sync.Once
	unblock := func() { release.Do(func() { close(blocked.release) }) }
	t.Cleanup(unblock)
	c := cmConnect(t, l)
	select {
	case <-admitted:
	case <-time.After(5 * time.Second):
		t.Fatal("healthy neighbour not admitted")
	}
	refused := make(chan bool, 1)
	go func() {
		client, r, err := cmDial(t, l, strings.Repeat("0", 64))
		if client != nil {
			client.CloseNow()
		}
		valid := err != nil && r != nil && r.StatusCode == http.StatusForbidden
		if r != nil {
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			valid = valid && string(body) == "not_allowed\n"
		}
		refused <- valid
	}()
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostic control not reached")
	}
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Reason != "daemon_unavailable" {
		t.Fatal("daemon death blocked behind diagnostic I/O")
	}
	unblock()
	select {
	case valid := <-refused:
		if !valid {
			t.Fatal("refusal reason changed after log release")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refusal did not complete")
	}
}
