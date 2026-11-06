package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type availabilityStore struct{ calls int }

func (s *availabilityStore) Read(context.Context, config.Paths, config.SecretReference) (string, error) {
	s.calls++
	return "", errors.New("synthetic-secret-store-refused")
}
func (s *availabilityStore) Create(context.Context, config.Paths, config.SecretReference, string) error {
	s.calls++
	return errors.New("synthetic-secret-store-refused")
}

type availabilityFixture struct {
	t                                *testing.T
	root, server, endpoint, username string
	registry                         config.ConnectionRegistry
	env                              map[string]string
	store                            availabilityStore
	mu                               sync.Mutex
	calls, otherCalls                int
	status                           int
	body, redirect                   string
}

func newAvailabilityFixture(t *testing.T, named bool) *availabilityFixture {
	t.Helper()
	f := &availabilityFixture{t: t, root: t.TempDir(), server: "Selected.invalid:8448", username: "alice", env: map[string]string{}, status: 200, body: `{"available":true}`}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.otherCalls++
		f.mu.Unlock()
		fmt.Fprint(w, `{"available":true}`)
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		status, body, redirect := f.status, f.body, f.redirect
		f.mu.Unlock()
		data, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodGet || r.URL.Path != "/_matrix/client/v3/register/available" || r.URL.Query().Get("username") != f.username || len(r.URL.Query()) != 1 || len(r.URL.Query()["username"]) != 1 || len(data) != 0 || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("ZERO_MUTATION_GUARD: only the exact anonymous availability GET is allowed")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if redirect != "" {
			w.Header().Set("Location", redirect)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	f.endpoint = srv.URL
	if err := os.WriteFile(filepath.Join(f.root, "matrix.env"), []byte("A2A_MATRIX_URL="+srv.URL+"\nA2A_MATRIX_SERVER_NAME="+f.server+"\nA2A_MATRIX_REGISTRATION_TOKEN=synthetic-registration-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"agents", "humans", "state", "owner-dm", "connections/selected-id/agents", "connections/selected-id/identities/agents/synthetic"} {
		path := filepath.Join(f.root, dir)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "caller.json"), []byte("synthetic-credential-secret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.root, "registration.pending.json"), []byte("synthetic-journal-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if named {
		store := config.CredentialStore{Backend: "keychain", AccessTokenService: "test.access", PasswordService: "test.password"}
		f.registry = config.ConnectionRegistry{SchemaVersion: 1, Current: "selected", Profiles: map[string]config.Connection{
			"selected": {ID: "selected-id", URL: srv.URL, ServerName: f.server, Store: store, RegistrationTokenRef: &config.SecretReference{Service: "test.registration", Account: "synthetic-reference"}},
			"other":    {ID: "other-id", URL: other.URL, ServerName: "other.invalid", Store: store},
		}}
		f.saveRegistry()
	}
	return f
}
func (f *availabilityFixture) saveRegistry() {
	f.t.Helper()
	b, err := json.Marshal(f.registry)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "connections.json"), b, 0600); err != nil {
		f.t.Fatal(err)
	}
}

func availabilitySnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if !d.IsDir() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += ":" + string(b)
		}
		rel, _ := filepath.Rel(root, path)
		snapshot[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func (f *availabilityFixture) app(out, errout io.Writer) *App {
	a := NewApp()
	a.Stdout, a.Stderr = out, errout
	a.connectionSecretStore = &f.store
	a.keychain = func(config.Paths) *keychainStore { f.t.Error("SECRET_STORE_GUARD: keychain invoked"); return nil }
	a.Getenv = func(k string) string {
		if k == config.EnvConfigDir {
			return f.root
		}
		if k == config.EnvRegistrationToken || k == config.EnvAgent || k == envSecretStore {
			f.t.Errorf("SECRET_STORE_GUARD: anonymous routing requested %s", k)
			return "synthetic-environment-secret"
		}
		return f.env[k]
	}
	return a
}

func (f *availabilityFixture) run(ctx context.Context, args ...string) result {
	f.t.Helper()
	before := availabilitySnapshot(f.t, f.root)
	var out, errout bytes.Buffer
	a := f.app(&out, &errout)
	code := a.Run(ctx, args)
	if f.store.calls != 0 {
		f.t.Fatal("SECRET_STORE_GUARD: secret-store read/write")
	}
	if after := availabilitySnapshot(f.t, f.root); !reflect.DeepEqual(before, after) {
		f.t.Fatal("STATE_WRITE_GUARD: configuration, credentials or registration state changed")
	}
	for _, secret := range []string{"synthetic-registration-secret", "synthetic-credential-secret", "synthetic-journal-secret", "synthetic-peer-secret", "synthetic-environment-secret"} {
		if strings.Contains(out.String()+errout.String(), secret) {
			f.t.Fatal("SECRET_OUTPUT_GUARD: synthetic secret escaped")
		}
	}
	return result{code, out.String(), errout.String()}
}

func TestAvailabilityCLIOutcomes(t *testing.T) {
	for _, named := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			status int
			body   string
			want   matrix.AvailabilityOutcome
		}{
			{"available", 200, `{"available":true}`, matrix.AvailabilityAvailable},
			{"taken", 400, `{"errcode":"M_USER_IN_USE","error":"synthetic-peer-secret"}`, matrix.AvailabilityTaken},
			{"invalid", 400, `{"errcode":"M_INVALID_USERNAME"}`, matrix.AvailabilityInvalid},
			{"reserved", 400, `{"errcode":"M_EXCLUSIVE"}`, matrix.AvailabilityReserved},
			{"disabled", 403, `{"errcode":"M_FORBIDDEN","error":"synthetic-peer-secret"}`, matrix.AvailabilityUnknown},
			{"unsupported", 404, `{"errcode":"M_NOT_FOUND"}`, matrix.AvailabilityUnknown},
			{"rate-limited", 429, `{"errcode":"M_LIMIT_EXCEEDED"}`, matrix.AvailabilityUnknown},
			{"malformed", 200, `{"available":`, matrix.AvailabilityUnknown},
			{"false", 200, `{"available":false}`, matrix.AvailabilityUnknown},
		} {
			t.Run(fmt.Sprintf("named=%t/%s", named, tc.name), func(t *testing.T) {
				f := newAvailabilityFixture(t, named)
				f.status, f.body = tc.status, tc.body
				r := f.run(context.Background(), "--as", "caller", "availability", "@alice:"+f.server, "--json")
				var out struct {
					Outcome matrix.AvailabilityOutcome `json:"outcome"`
					UserID  string                     `json:"user_id"`
					Server  string                     `json:"server_name"`
					URL     string                     `json:"homeserver_url"`
				}
				if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
					t.Fatalf("JSON: %v, code=%d stderr=%q", err, r.code, r.stderr)
				}
				if out.Outcome != tc.want || out.UserID != "@alice:"+f.server || out.Server != f.server || out.URL != f.endpoint || (r.code != 0) != (tc.want == matrix.AvailabilityUnknown) || f.calls != 1 || f.otherCalls != 0 {
					t.Fatalf("result=%+v code=%d calls=%d/%d", out, r.code, f.calls, f.otherCalls)
				}
			})
		}
	}
}

func TestAvailabilityIdentityRefusals(t *testing.T) {
	for _, input := range []string{"@alice:other.invalid", "@alice:selected.invalid:8448", "@Alice:Selected.invalid:8448", "Alice", "alice:Selected.invalid:8448", "@alice", "", "a b", "a\nb", "é", "#room:Selected.invalid:8448", "!room:Selected.invalid:8448", "mailto:alice", "https://selected.invalid/alice", strings.Repeat("a", 256)} {
		t.Run(fmt.Sprintf("input-%x", []byte(input)), func(t *testing.T) {
			f := newAvailabilityFixture(t, true)
			r := f.run(context.Background(), "availability", input, "--json")
			if r.code != ExitUsage || r.stdout != "" || f.calls != 0 || f.otherCalls != 0 {
				t.Fatalf("rejected input had effects: code=%d calls=%d/%d", r.code, f.calls, f.otherCalls)
			}
			// Restoring only the identity admits the exact selected endpoint.
			r = f.run(context.Background(), "availability", "@alice:"+f.server, "--json")
			if r.code != 0 || f.calls != 1 || f.otherCalls != 0 {
				t.Fatalf("valid neighbor failed: %+v", r)
			}
		})
	}
}

func TestAvailabilityRoutingRefusals(t *testing.T) {
	for _, fault := range []string{"unknown-profile", "conflicting-profiles", "missing-registry", "malformed-registry", "wrong-url", "wrong-server-case", "invalid-endpoint", "missing-server", "fingerprint", "human", "extra-argument", "room-flag"} {
		t.Run(fault, func(t *testing.T) {
			f := newAvailabilityFixture(t, true)
			args := []string{"availability", "alice", "--json"}
			switch fault {
			case "unknown-profile":
				args = append(args, "--profile", "missing")
			case "conflicting-profiles":
				args = append(args, "--profile", "selected", "--profile", "other")
			case "missing-registry":
				os.Remove(filepath.Join(f.root, "connections.json"))
				args = append(args, "--profile", "selected")
			case "malformed-registry":
				os.WriteFile(filepath.Join(f.root, "connections.json"), []byte(`{"schema_version":1,"current":"selected","current":"other"}`), 0600)
			case "wrong-url":
				f.env[config.EnvMatrixURL] = "https://other.invalid"
			case "wrong-server-case":
				f.env[config.EnvServerName] = strings.ToLower(f.server)
			case "invalid-endpoint":
				c := f.registry.Profiles["selected"]
				c.URL = "https://secret@other.invalid"
				f.registry.Profiles["selected"] = c
				f.saveRegistry()
			case "missing-server":
				os.Remove(filepath.Join(f.root, "connections.json"))
				f.env[config.EnvServerName] = ""
				os.WriteFile(filepath.Join(f.root, "matrix.env"), []byte("A2A_MATRIX_URL="+f.endpoint+"\n"), 0600)
			case "fingerprint":
				args = append(args, "--connection-fingerprint", strings.Repeat("a", 64))
			case "human":
				args = append(args, "--human")
			case "extra-argument":
				args = append(args, "extra")
			case "room-flag":
				args = append(args, "--room", "synthetic-room")
			}
			r := f.run(context.Background(), args...)
			if r.code == 0 || r.stdout != "" || f.calls != 0 || f.otherCalls != 0 {
				t.Fatalf("routing refusal had effects: %+v calls=%d/%d", r, f.calls, f.otherCalls)
			}
			// Fresh valid routing is the causal neighbor for every refusal above.
			good := newAvailabilityFixture(t, true)
			if got := good.run(context.Background(), "availability", "alice"); got.code != 0 || got.stdout != "available\t@alice:"+good.server+"\n" || good.calls != 1 {
				t.Fatalf("valid neighbor failed: %+v", got)
			}
		})
	}
}

func TestAvailabilityRedirectsAndInterruption(t *testing.T) {
	for _, mode := range []string{"same-origin", "cross-origin", "interrupted"} {
		t.Run(mode, func(t *testing.T) {
			f := newAvailabilityFixture(t, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "interrupted" {
				cancel()
			} else {
				f.status = 302
				f.redirect = f.endpoint + "/redirect-target"
				if mode == "cross-origin" {
					f.redirect = f.registry.Profiles["other"].URL + "/redirect-target"
				}
			}
			r := f.run(ctx, "availability", "alice", "--json")
			if r.code != ExitError || !strings.Contains(r.stdout, `"outcome":"unknown"`) || f.otherCalls != 0 {
				t.Fatalf("bad unknown: %+v", r)
			}
			if mode == "interrupted" && f.calls != 0 || mode != "interrupted" && f.calls != 1 {
				t.Fatalf("unexpected calls=%d", f.calls)
			}
		})
	}
}

func TestAvailabilityExplicitProfileAndLocalpart(t *testing.T) {
	f := newAvailabilityFixture(t, true)
	f.registry.Current = "other"
	f.saveRegistry()
	f.username = "a+b/c._=-"
	r := f.run(context.Background(), "--profile", "selected", "availability", f.username, "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"user_id":"@a+b/c._=-:`+f.server+`"`) || f.calls != 1 || f.otherCalls != 0 {
		t.Fatalf("profile/localpart control: %+v", r)
	}
}

func TestAvailabilityInterruptedInFlight(t *testing.T) {
	f := newAvailabilityFixture(t, false)
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/_matrix/client/v3/register/available" || r.Header.Get("Authorization") != "" {
			t.Error("ZERO_MUTATION_GUARD: interrupted query changed operation")
		}
		close(entered)
		<-r.Context().Done()
	}))
	defer srv.Close()
	f.env[config.EnvMatrixURL] = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := availabilitySnapshot(t, f.root)
	var out, errout bytes.Buffer
	a := f.app(&out, &errout)
	done := make(chan int, 1)
	go func() { done <- a.Run(ctx, []string{"availability", "alice", "--json"}) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic GET never reached server")
	}
	cancel()
	select {
	case code := <-done:
		if code != ExitError || !strings.Contains(out.String(), `"outcome":"unknown"`) || !strings.Contains(out.String(), `"reason":"interrupted"`) {
			t.Fatalf("interrupted result: code=%d out=%q", code, out.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("query did not respect interruption")
	}
	if f.store.calls != 0 || !reflect.DeepEqual(before, availabilitySnapshot(t, f.root)) {
		t.Fatal("interrupted query touched secrets/state")
	}
}

func TestAvailabilityIdentityBoundaries(t *testing.T) {
	for _, server := range []string{"Selected.invalid", "Selected.invalid:8448", "[::1]:8448"} {
		local := strings.Repeat("a", 255-len("@:"+server))
		got, id, err := availabilityIdentity("@"+local+":"+server, server)
		if err != nil || got != local || len(id) != 255 {
			t.Fatal("255-byte exact MXID refused")
		}
		if _, _, err := availabilityIdentity(local+"a", server); err == nil {
			t.Fatal("overlong MXID accepted")
		}
		if _, _, err := availabilityIdentity("@alice:"+strings.ToLower(server), server); server != strings.ToLower(server) && err == nil {
			t.Fatal("server case was normalized")
		}
	}
}
