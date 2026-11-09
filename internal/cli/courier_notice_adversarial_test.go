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
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/relux-works/a2a-swarm/internal/courier"
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
			s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
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
			if !errors.As(err, &closed) || closed.Reason != "courier_unavailable" {
				t.Fatal("invalid engine output escaped or lacked a closed reason")
			}
		})
	}
}

func TestCMListenerMalformedHandshake(t *testing.T) {
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int { <-ctx.Done(); return 0 })
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
	s := newC5Station(t, func(_ context.Context, inv courier.Invocation) int { return inv.Refuse(courier.IdleTimeout) })
	l := cmStartC5(t, s)
	c := cmConnect(t, l)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	var closed websocket.CloseError
	if !errors.As(err, &closed) || closed.Reason != "courier_idle_timeout" {
		t.Fatal("idle timeout did not reach monitor as exact reason")
	}
}

func TestCMListenerGenerationCapability(t *testing.T) {
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int { <-ctx.Done(); return 0 })
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
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int { <-ctx.Done(); return 0 })
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
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int { <-ctx.Done(); return 0 })
	for _, args := range [][]string{
		{"watch", "--listen"}, {"watch", "--notices=false", "--listen"},
		{"watch", "--notices", "--listen", "operand"},
		{"watch", "--notices", "--listen", "--all"},
	} {
		r := s.run(context.Background(), "", args...)
		if r.code != 1 || r.stdout != "" || r.stderr != "a2a: courier: not_allowed\n" {
			t.Fatal("invalid watch form lacked exact closed refusal")
		}
	}
	l := cmStartC5(t, s)
	cmConnect(t, l)
}
