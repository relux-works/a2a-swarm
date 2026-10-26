//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type registrationBootstrapStore struct {
	base *namedTestStore
	ref  config.SecretReference
	read func(context.Context, config.Paths) (string, error)
}

func (s registrationBootstrapStore) Read(ctx context.Context, p config.Paths, r config.SecretReference) (string, error) {
	if r == s.ref {
		return s.read(ctx, p)
	}
	return s.base.Read(ctx, p, r)
}
func (s registrationBootstrapStore) Create(ctx context.Context, p config.Paths, r config.SecretReference, v string) error {
	return s.base.Create(ctx, p, r, v)
}
func registrationArgs(op string, j registrationJournal) []string {
	args := []string{"--profile", "alpha", "connection", "registration", op, "alice"}
	if op == "inspect" {
		return append(args, "--authorize-diagnostic")
	}
	return append(args, "--authorize-registration", "--attempt", j.Attempt, "--revision", strconv.Itoa(len(j.History)))
}
func pendingRegistration(t *testing.T, f *namedFixture) registrationJournal {
	t.Helper()
	b, _ := f.r.Profiles["alpha"].Binding("alice", false)
	j, e := readRegistrationJournal(filepath.Join(f.paths("alpha", false).Base, "registration.pending.json"), b)
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func failedBootstrap(t *testing.T, f *namedFixture) registrationJournal {
	t.Helper()
	delete(f.store.items, *f.r.Profiles["alpha"].RegistrationTokenRef)
	if f.app().Run(context.Background(), []string{"--profile", "alpha", "register", "alice", "--authorize-registration"}) == 0 {
		t.Fatal("missing bootstrap accepted")
	}
	return pendingRegistration(t, f)
}
func registrationTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	e := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		st, e := os.Lstat(path)
		if e != nil {
			return e
		}
		v := st.Mode().String()
		if !d.IsDir() && st.Mode()&os.ModeNamedPipe == 0 {
			b, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			v += string(b)
		}
		tree[path] = v
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	return tree
}

// Proves observed bootstrap failures cross the public registration/inspection boundary without secrets or creation effects; valid/closed-pipe controls succeed.
func TestNamedRegistrationBootstrapEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		codes    []string
		exit     *int
		cooldown bool
	}{
		{"valid", nil, nil, false}, {"closed-pipe", nil, nil, false},
		{"missing", []string{"missing"}, intPointer(44), false},
		{"interaction", []string{"interaction-exit"}, intPointer(36), true},
		{"ordinary", []string{"ordinary-exit"}, intPointer(7), false},
		{"start", []string{"process-start"}, nil, false},
		{"deadline", []string{"ordinary-exit", "deadline"}, intPointer(-1), true},
		{"cancelled", []string{"cancelled"}, nil, true},
		{"drain", []string{"drain", "ordinary-exit"}, intPointer(44), true},
		{"io", []string{"io"}, nil, true},
		{"cleanup", []string{"cleanup"}, nil, true},
		{"io-cleanup", []string{"io", "cleanup"}, nil, true},
		{"locked", []string{"native-locked"}, nil, true},
		{"unknown-probe", []string{"unavailable"}, nil, true},
		{"empty", []string{"invalid-secret"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, false)
			a := f.app()
			dir := t.TempDir()
			ref := *f.r.Profiles["alpha"].RegistrationTokenRef
			var k *keychainStore
			a.connectionSecretStore = registrationBootstrapStore{f.store, ref, func(ctx context.Context, p config.Paths) (string, error) {
				k = &keychainStore{paths: p, service: ref.Service, hostID: "fixture-host", fixture: filepath.Join(dir, "synthetic.keychain-db"), probeCommand: unlockedProbe, timeout: 300 * time.Millisecond}
				childCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				k.command = func(ctx context.Context, args ...string) *exec.Cmd {
					switch tc.name {
					case "missing":
						return exec.CommandContext(ctx, "/bin/sh", "-c", "printf planted-helper-secret >&2; exit 44")
					case "interaction":
						return exec.CommandContext(ctx, "/bin/sh", "-c", "printf planted-helper-secret >&2; exit 36")
					case "ordinary":
						return exec.CommandContext(ctx, "/bin/sh", "-c", "printf planted-helper-secret >&2; exit 7")
					case "deadline":
						return exec.CommandContext(ctx, "/bin/sleep", "30")
					case "cancelled":
						cancel()
						return exec.CommandContext(ctx, "/bin/sleep", "30")
					case "start":
						return probeFixture(t, "missing", dir)(ctx)
					case "drain":
						return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 & printf planted-helper-secret; exit 44")
					case "empty":
						return exec.CommandContext(ctx, "/usr/bin/printf", "\n")
					case "closed-pipe":
						return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 </dev/null >/dev/null 2>&1 & printf isolated-bootstrap")
					default:
						return exec.CommandContext(ctx, "/usr/bin/printf", "isolated-bootstrap")
					}
				}
				if tc.name == "locked" {
					k.probeCommand = probeFixture(t, "locked", dir)
				}
				if tc.name == "unknown-probe" {
					k.probeCommand = probeFixture(t, "badtoken", dir)
				}
				// Inject runner fault sentinels after real successful closed pipes;
				// this proves classification/cooldown logic, not an OS cleanup failure.
				if tc.name == "io" || tc.name == "cleanup" || tc.name == "io-cleanup" {
					k.afterRun = func(err error) error {
						if tc.name != "cleanup" {
							err = errors.Join(err, errSecurityProcessIO)
						}
						if tc.name != "io" {
							err = errors.Join(err, errSecurityProcessCleanup)
						}
						return errors.Join(err, errors.New("planted-helper-secret"))
					}
				}
				var value string
				e := k.locked(ref.Account, func() error { var e error; value, e = k.lookup(childCtx, ref.Account); return e })
				return value, e
			}}
			code := a.Run(context.Background(), []string{"--profile", "alpha", "register", "alice", "--authorize-registration"})
			j := pendingRegistration(t, f)
			if tc.codes == nil {
				if code != 0 || j.Stage != "complete" || f.httpCalls.Load() != 1 || len(f.store.creates) != 2 {
					t.Fatalf("positive control: %d %s %s", code, j.Stage, f.err.String())
				}
				return
			}
			if code == 0 || j.Stage != "bootstrap-unavailable" || f.httpCalls.Load() != 0 || len(f.store.creates) != 0 {
				t.Fatal("bootstrap failure crossed creation boundary")
			}
			got := j.History[len(j.History)-1].Failure
			if got == nil || !reflect.DeepEqual(got.Codes, tc.codes) || !reflect.DeepEqual(got.Exit, tc.exit) {
				t.Fatalf("classification got %+v want codes=%v exit=%v", got, tc.codes, tc.exit)
			}
			for _, path := range []string{f.paths("alpha", false).AgentFile("alice"), f.paths("alpha", false).HumanFile("alice")} {
				if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("metadata effect")
				}
			}
			if (k.checkCooldown(ref.Account) != nil) != tc.cooldown {
				t.Fatal("cooldown evidence changed")
			}
			before := registrationTree(t, f.root)
			effects := f.effects()
			f.out.Reset()
			// A production app without ANY secret-store seam must still inspect safely.
			inspect := f.app()
			inspect.connectionSecretStore = nil
			if inspect.Run(context.Background(), registrationArgs("inspect", j)) != 0 {
				t.Fatal(f.err.String())
			}
			var report struct {
				Failure secretEvidence `json:"failure"`
			}
			if json.Unmarshal(f.out.Bytes(), &report) != nil || !reflect.DeepEqual(report.Failure, *got) {
				t.Fatal("diagnostic lost classification")
			}
			if effects != f.effects() || !reflect.DeepEqual(before, registrationTree(t, f.root)) {
				t.Fatal("diagnostic mutated state or accessed secrets/HTTP")
			}
			raw, _ := json.Marshal(j)
			for _, secret := range []string{"planted-helper-secret", "isolated-bootstrap", "isolated-new-token"} {
				if strings.Contains(f.out.String()+f.err.String()+string(raw), secret) {
					t.Fatal("secret leaked")
				}
			}
			if f.app().Run(context.Background(), []string{"--profile", "alpha", "register", "alice", "--authorize-registration"}) == 0 || f.httpCalls.Load() != 0 {
				t.Fatal("fresh registration bypassed pending fence")
			}
		})
	}
}
func intPointer(i int) *int { return &i }

// Proves separate admission continues the original attempt, retains original failure evidence and leaves the old selected connection functional.
func TestNamedRegistrationPreHTTPContinuation(t *testing.T) {
	f := newNamedFixture(t, false)
	f.publish("beta", false)
	f.store.items[config.SecretReference{Service: "test.beta.token", Account: "@alice:beta.invalid"}] = "isolated-beta"
	j := failedBootstrap(t, f)
	original := append([]registrationEvent(nil), j.History...)
	f.store.items[*f.r.Profiles["alpha"].RegistrationTokenRef] = "isolated-bootstrap"
	if f.app().Run(context.Background(), registrationArgs("continue", j)) != 0 {
		t.Fatal(f.err.String())
	}
	next := pendingRegistration(t, f)
	if next.Attempt != j.Attempt || next.Stage != "complete" || !reflect.DeepEqual(next.History[:len(original)], original) || f.httpCalls.Load() != 1 || len(f.store.creates) != 2 {
		t.Fatal("continuation replaced attempt/evidence or repeated creation")
	}
	before := f.effects()
	if f.app().Run(context.Background(), registrationArgs("continue", j)) == 0 || f.effects() != before {
		t.Fatal("stale continuation had effects")
	}
	if f.app().Run(context.Background(), []string{"--profile", "beta", "whoami"}) != 0 {
		t.Fatal("old traffic failed", f.err.String())
	}
}

// Proves admission, ownership, binding, schema and neighboring-state refusals preserve evidence and prohibit creation; the valid control is above.
func TestNamedRegistrationContinuationRefusals(t *testing.T) {
	for _, mode := range []string{"diagnostic-only", "no-admission", "stale", "attempt", "concurrent", "missing-lock", "binding", "bootstrap-ref", "malformed", "unknown-field", "unknown-stage", "history", "legacy", "metadata", "human-metadata", "item", "password-item", "state", "ambiguous", "post-creation", "password-boundary", "journal-symlink", "journal-fifo", "nonprivate"} {
		t.Run(mode, func(t *testing.T) {
			f := newNamedFixture(t, false)
			f.publish("beta", false)
			f.store.items[config.SecretReference{Service: "test.beta.token", Account: "@alice:beta.invalid"}] = "isolated-beta"
			j := failedBootstrap(t, f)
			f.store.items[*f.r.Profiles["alpha"].RegistrationTokenRef] = "isolated-bootstrap"
			p := f.paths("alpha", false)
			path := filepath.Join(p.Base, "registration.pending.json")
			args := registrationArgs("continue", j)
			switch mode {
			case "diagnostic-only":
				args = []string{"--profile", "alpha", "connection", "registration", "continue", "alice", "--authorize-diagnostic", "--attempt", j.Attempt, "--revision", "2"}
			case "no-admission":
				args = args[:6]
			case "stale":
				args[len(args)-1] = "1"
			case "attempt":
				args[len(args)-3] = strings.Repeat("0", 32)
			case "concurrent":
				unlock, e := lockRegistration(filepath.Join(p.Base, "registration.lock"), false)
				if e != nil {
					t.Fatal(e)
				}
				defer unlock()
			case "missing-lock":
				if e := os.Remove(filepath.Join(p.Base, "registration.lock")); e != nil {
					t.Fatal(e)
				}
			case "binding", "bootstrap-ref":
				c := f.r.Profiles["alpha"]
				if mode == "binding" {
					c.URL = f.r.Profiles["beta"].URL
				} else {
					c.RegistrationTokenRef = &config.SecretReference{Service: "changed.bootstrap", Account: "changed"}
				}
				f.r.Profiles["alpha"] = c
				f.save()
			case "malformed":
				if e := os.WriteFile(path, []byte(`{"stage":`), 0600); e != nil {
					t.Fatal(e)
				}
			case "unknown-field":
				raw, _ := os.ReadFile(path)
				raw = bytes.Replace(raw, []byte(`{`), []byte(`{"planted-helper-secret":true,`), 1)
				if e := os.WriteFile(path, raw, 0600); e != nil {
					t.Fatal(e)
				}
			case "unknown-stage":
				j.Stage = "planted-helper-secret"
				if e := updateRegistrationJournal(path, j); e != nil {
					t.Fatal(e)
				}
			case "history":
				j.History[0].Stage = "remote-outcome-pending"
				if e := updateRegistrationJournal(path, j); e != nil {
					t.Fatal(e)
				}
			case "legacy":
				j.Version = 0
				j.Attempt = ""
				j.History = nil
				if e := updateRegistrationJournal(path, j); e != nil {
					t.Fatal(e)
				}
			case "metadata":
				f.publish("alpha", false)
			case "human-metadata":
				f.publish("alpha", true)
			case "item":
				f.store.items[config.SecretReference{Service: "test.alpha.token", Account: j.Binding.UserID}] = "existing"
			case "password-item":
				f.store.items[config.SecretReference{Service: "test.alpha.password", Account: j.Binding.UserID}] = "existing"
			case "state":
				if e := os.WriteFile(filepath.Join(p.Base, "state.json"), []byte(`{}`), 0600); e != nil {
					t.Fatal(e)
				}
			case "ambiguous", "post-creation", "password-boundary":
				j.History = append(j.History, registrationEvent{Stage: "continuation-admitted"}, registrationEvent{Stage: "password-storage-pending"})
				if mode != "password-boundary" {
					j.History = append(j.History, registrationEvent{Stage: "remote-outcome-pending"})
					if mode == "ambiguous" {
						j.History = append(j.History, registrationEvent{Stage: "remote-outcome-ambiguous"})
					} else {
						j.History = append(j.History, registrationEvent{Stage: "remote-created-token-storage-pending"})
					}
				}
				j.Stage = j.History[len(j.History)-1].Stage
				if e := updateRegistrationJournal(path, j); e != nil {
					t.Fatal(e)
				}
				args = registrationArgs("continue", j)
			case "journal-symlink":
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				target := filepath.Join(f.root, "receipt")
				if e = os.WriteFile(target, raw, 0600); e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(path); e != nil {
					t.Fatal(e)
				}
				if e = os.Symlink(target, path); e != nil {
					t.Fatal(e)
				}
			case "journal-fifo":
				if e := os.Remove(path); e != nil {
					t.Fatal(e)
				}
				if e := syscall.Mkfifo(path, 0600); e != nil {
					t.Fatal(e)
				}
			case "nonprivate":
				if e := os.Chmod(path, 0644); e != nil {
					t.Fatal(e)
				}
			}
			before := registrationTree(t, f.root)
			effects := f.effects()
			creates := len(f.store.creates)
			if f.app().Run(context.Background(), args) == 0 {
				t.Fatal("forbidden continuation accepted")
			}
			if f.httpCalls.Load() != 0 || len(f.store.creates) != creates {
				t.Fatal("refusal caused creation effects")
			}
			// Conflicting item refusal may append a preflight refusal after explicit
			// admission; all other invalid cases must preserve every byte.
			if mode != "item" && mode != "password-item" && (!reflect.DeepEqual(before, registrationTree(t, f.root)) || effects != f.effects()) {
				t.Fatal("refusal altered evidence or accessed secrets")
			}
			if strings.Contains(f.out.String()+f.err.String(), "planted-helper-secret") {
				t.Fatal("malformed evidence leaked")
			}
			if f.app().Run(context.Background(), []string{"--profile", "beta", "whoami"}) != 0 {
				t.Fatal("refusal broke old traffic", f.err.String())
			}
		})
	}
}

// Proves inspection requires its own admission and legacy receipts report unknown evidence without enabling continuation or reading any secret.
func TestNamedRegistrationDiagnosticAdmission(t *testing.T) {
	f := newNamedFixture(t, false)
	j := failedBootstrap(t, f)
	before := registrationTree(t, f.root)
	effects := f.effects()
	args := registrationArgs("inspect", j)
	if f.app().Run(context.Background(), args[:len(args)-1]) == 0 {
		t.Fatal("unadmitted inspection accepted")
	}
	if f.app().Run(context.Background(), args) != 0 {
		t.Fatal(f.err.String())
	}
	path := filepath.Join(f.paths("alpha", false).Base, "registration.pending.json")
	j.Version = 0
	j.Attempt = ""
	j.History = nil
	if e := updateRegistrationJournal(path, j); e != nil {
		t.Fatal(e)
	}
	before = registrationTree(t, f.root)
	f.out.Reset()
	if f.app().Run(context.Background(), args) != 0 || !strings.Contains(f.out.String(), `"pre_http_candidate":false`) || !strings.Contains(f.out.String(), `"unavailable"`) {
		t.Fatal("legacy diagnostic fabricated evidence")
	}
	if effects != f.effects() || !reflect.DeepEqual(before, registrationTree(t, f.root)) {
		t.Fatal("diagnostic had effects")
	}
}

// Proves unknown errors cannot smuggle helper text through evidence; simultaneous independently observed classes are retained.
func TestRegistrationFailureClassification(t *testing.T) {
	err := observedSecretFailure("safe", "security", errors.Join(errSecurityProcessIO, errSecurityProcessCleanup, exec.ErrWaitDelay, errors.New("planted-helper-secret")), context.DeadlineExceeded)
	got := registrationFailureEvidence(err, false)
	if !reflect.DeepEqual(got.Codes, []string{"drain", "io", "cleanup", "deadline"}) || strings.Contains(fmt.Sprint(got), "planted-helper-secret") {
		t.Fatalf("classification: %+v", got)
	}
	if got := registrationFailureEvidence(errors.New("planted-helper-secret"), false); !reflect.DeepEqual(got.Codes, []string{"unavailable"}) {
		t.Fatal("invented evidence")
	}
	if got := registrationFailureEvidence(nil, true); !reflect.DeepEqual(got.Codes, []string{"empty"}) {
		t.Fatal("empty control lost")
	}
}

// Proves explicit continuation respects the existing bootstrap cooldown, retains its bytes and original attempt, then succeeds only after expiry.
func TestNamedRegistrationContinuationCooldown(t *testing.T) {
	f := newNamedFixture(t, false)
	ref := *f.r.Profiles["alpha"].RegistrationTokenRef
	now := time.Now()
	calls := 0
	deny := true
	var k *keychainStore
	app := func() *App {
		a := f.app()
		a.connectionSecretStore = registrationBootstrapStore{f.store, ref, func(ctx context.Context, p config.Paths) (string, error) {
			k = &keychainStore{paths: p, service: ref.Service, hostID: "fixture", fixture: filepath.Join(f.root, "synthetic.keychain-db"), probeCommand: unlockedProbe, now: func() time.Time { return now }, command: func(ctx context.Context, args ...string) *exec.Cmd {
				calls++
				if deny {
					return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 36")
				}
				return exec.CommandContext(ctx, "/usr/bin/printf", "isolated-bootstrap")
			}}
			var value string
			e := k.locked(ref.Account, func() error { var e error; value, e = k.lookup(ctx, ref.Account); return e })
			return value, e
		}}
		return a
	}
	if app().Run(context.Background(), []string{"--profile", "alpha", "register", "alice", "--authorize-registration"}) == 0 {
		t.Fatal("interaction accepted")
	}
	j := pendingRegistration(t, f)
	marker := k.identityPath(ref.Account) + ".cooldown"
	before, e := os.ReadFile(marker)
	if e != nil {
		t.Fatal(e)
	}
	deny = false
	if app().Run(context.Background(), registrationArgs("continue", j)) == 0 || calls != 1 || f.httpCalls.Load() != 0 || len(f.store.creates) != 0 {
		t.Fatal("continuation bypassed cooldown")
	}
	after, e := os.ReadFile(marker)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("continuation changed cooldown evidence")
	}
	next := pendingRegistration(t, f)
	if next.Attempt != j.Attempt || !reflect.DeepEqual(next.History[:len(j.History)], j.History) || !reflect.DeepEqual(next.History[len(next.History)-1].Failure.Codes, []string{"cooldown"}) {
		t.Fatal("continuation lost original evidence")
	}
	now = now.Add(securityCooldown + time.Second)
	if app().Run(context.Background(), registrationArgs("continue", next)) != 0 || calls != 2 || f.httpCalls.Load() != 1 {
		t.Fatal("expired cooldown control failed", f.err.String())
	}
	after, e = os.ReadFile(marker)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("successful continuation deleted/replaced cooldown")
	}
}
