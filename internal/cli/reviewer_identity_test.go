package cli

import (
	"github.com/relux-works/a2a-swarm/internal/config"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestReviewerInvalidIdentityNotPersisted(t *testing.T) {
	for _, kind := range []string{"genuine", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			e.extra[envSecretStore] = "file"
			hits := 0
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil || resp.StatusCode != 200 || !strings.HasSuffix(r.URL.Path, "/register") {
						return resp, err
					}
					hits++
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					if kind == "invalid" {
						raw = []byte(strings.ReplaceAll(string(raw), "@newbie:test.local", "not-a-user-id"))
					}
					resp.Body = io.NopCloser(strings.NewReader(string(raw)))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				})}
			}
			result := e.run("register", "newbie")
			if hits != 1 {
				t.Fatalf("success response not reached hits=%d", hits)
			}
			if kind == "genuine" {
				if result.code != 0 {
					t.Fatalf("valid register exit=%d", result.code)
				}
				return
			}
			_, statErr := os.Stat(e.paths().AgentFile("newbie"))
			creds, _ := config.LoadCredentials(e.paths(), "newbie")
			if result.code == 0 || !os.IsNotExist(statErr) {
				t.Fatalf("invalid identity trusted: exit=%d persisted=%t user_id=%q", result.code, statErr == nil, creds.UserID)
			}
		})
	}
}
func TestReviewerWhoamiInvalidIdentity(t *testing.T) {
	for _, kind := range []string{"genuine", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			hits := 0
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil || !strings.HasSuffix(r.URL.Path, "/whoami") {
						return resp, err
					}
					hits++
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					if kind == "invalid" {
						raw = []byte(strings.ReplaceAll(string(raw), "@alice:test.local", "not-a-user-id"))
					}
					resp.Body = io.NopCloser(strings.NewReader(string(raw)))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				})}
			}
			result := e.as("alice", "whoami")
			if hits != 1 {
				t.Fatalf("whoami response not reached hits=%d", hits)
			}
			if (result.code == 0) != (kind == "genuine") {
				t.Fatalf("identity trusted by CLI: kind=%s exit=%d invalid-user-printed=%t", kind, result.code, strings.Contains(result.stdout, "not-a-user-id"))
			}
		})
	}
}
