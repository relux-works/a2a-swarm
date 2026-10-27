//go:build unix

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func legacyRegistration(t *testing.T, f *namedFixture, stage string) ([]byte, string) {
	t.Helper()
	b, _ := f.r.Profiles["alpha"].Binding("alice", false)
	// Match the original two-field schema, with deliberately retained formatting.
	data, err := json.Marshal(struct {
		Binding config.ConnectionBinding `json:"binding"`
		Stage   string                   `json:"stage"`
	}{b, stage})
	if err != nil {
		t.Fatal(err)
	}
	data = append(append([]byte(" \n"), data...), '\n')
	p := f.paths("alpha", false)
	if err = os.MkdirAll(p.Base, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(p.Base, "registration.pending.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}
func legacyArgs(digest string) []string {
	return []string{"--profile", "alpha", "connection", "registration", "continue-legacy", "alice", "--legacy-sha256", digest, "--authorize-registration"}
}
func legacyDisposition(t *testing.T, f *namedFixture) registrationJournal {
	t.Helper()
	b, _ := f.r.Profiles["alpha"].Binding("alice", false)
	j, err := readRegistrationJournal(filepath.Join(f.paths("alpha", false).Base, "registration.continuation.json"), b)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func assertLegacyRetained(t *testing.T, f *namedFixture, original []byte) {
	t.Helper()
	path := filepath.Join(f.paths("alpha", false).Base, "registration.pending.json")
	got, err := os.ReadFile(path)
	st, statErr := os.Lstat(path)
	if err != nil || statErr != nil || !bytes.Equal(got, original) || st.Mode().Perm() != 0600 {
		t.Fatal("original v0 evidence changed", err, statErr)
	}
}

// Proves the actual v0/no-attempt/no-history/no-lock receipt can continue only
// after separate digest-bound admission; inspection stays read-only and replay has no effects.
func TestNamedRegistrationLegacyContinuation(t *testing.T) {
	f := newNamedFixture(t, false)
	f.publish("beta", false)
	f.store.items[config.SecretReference{Service: "test.beta.token", Account: "@alice:beta.invalid"}] = "isolated-beta"
	original, digest := legacyRegistration(t, f, "bootstrap-unavailable")
	before, effects := registrationTree(t, f.root), f.effects()
	inspect := f.app()
	inspect.connectionSecretStore = nil
	if inspect.Run(context.Background(), registrationArgs("inspect", registrationJournal{})) != 0 || !strings.Contains(f.out.String(), `"legacy_sha256":"`+digest+`"`) {
		t.Fatal("legacy inspection", f.out.String(), f.err.String())
	}
	if !reflect.DeepEqual(before, registrationTree(t, f.root)) || f.effects() != effects {
		t.Fatal("inspection had effects")
	}
	if f.app().Run(context.Background(), legacyArgs(digest)) != 0 {
		t.Fatal("admitted legacy control refused", f.err.String())
	}
	j := legacyDisposition(t, f)
	if j.Stage != "complete" || j.Attempt == "" || j.History[0].Stage != "legacy-continuation-admitted" || f.httpCalls.Load() != 1 || len(f.store.creates) != 2 {
		t.Fatal("new disposition missing or repeated creation", j)
	}
	raw, err := os.ReadFile(filepath.Join(f.paths("alpha", false).Base, "registration.continuation.json"))
	if err != nil || !strings.Contains(string(raw), `"legacy_sha256":"`+digest+`"`) {
		t.Fatal("missing retained provenance", err)
	}
	assertLegacyRetained(t, f, original)
	effects = f.effects()
	if f.app().Run(context.Background(), legacyArgs(digest)) == 0 || f.effects() != effects {
		t.Fatal("legacy replay had effects")
	}
	if f.app().Run(context.Background(), []string{"--profile", "beta", "whoami"}) != 0 {
		t.Fatal("old traffic failed", f.err.String())
	}
}

// Proves neighboring legacy states never authorize creation and retain the receipt.
// Admission-only failures leave the complete tree unchanged; item refusals may add a new audit disposition.
func TestNamedRegistrationLegacyRefusals(t *testing.T) {
	for _, mode := range []string{"no-admission", "diagnostic-only", "stale", "binding", "bootstrap-ref", "identity", "metadata", "human-metadata", "state", "unknown-lock", "concurrent", "unknown-sidecar", "malformed", "extra-field", "fake-history", "empty-version", "duplicate-stage", "aliased-stage", "conflicting-stage-alias", "aliased-binding", "unknown-stage", "bad-state", "symlink", "fifo", "nonprivate", "item", "password-item", "preflight", "secret-preflight-refused", "password-storage-pending", "password-store-failed", "remote-outcome-pending", "remote-outcome-ambiguous", "remote-created-token-storage-pending", "complete"} {
		t.Run(mode, func(t *testing.T) {
			f := newNamedFixture(t, false)
			stage := "bootstrap-unavailable"
			switch mode {
			case "preflight", "secret-preflight-refused", "password-storage-pending", "password-store-failed", "remote-outcome-pending", "remote-outcome-ambiguous", "remote-created-token-storage-pending", "complete":
				stage = mode
			}
			_, digest := legacyRegistration(t, f, stage)
			p := f.paths("alpha", false)
			path := filepath.Join(p.Base, "registration.pending.json")
			args := legacyArgs(digest)
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "no-admission":
				args = args[:len(args)-1]
			case "diagnostic-only":
				args[len(args)-1] = "--authorize-diagnostic"
			case "stale":
				args[len(args)-2] = strings.Repeat("0", 64)
			case "binding", "bootstrap-ref":
				c := f.r.Profiles["alpha"]
				if mode == "binding" {
					c.URL = f.r.Profiles["beta"].URL
				} else {
					c.RegistrationTokenRef = &config.SecretReference{Service: "changed.bootstrap", Account: "changed"}
				}
				f.r.Profiles["alpha"] = c
				f.save()
			case "identity":
				args = append([]string{"--as", "bob"}, args...)
			case "metadata":
				f.publish("alpha", false)
			case "human-metadata":
				f.publish("alpha", true)
			case "state":
				write(filepath.Join(p.Base, "state.json"), []byte(`{}`))
			case "unknown-lock":
				write(filepath.Join(p.Base, "registration.lock"), nil)
			case "concurrent":
				unlock, err := lockRegistration(filepath.Join(p.Base, "registration.lock"), true)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			case "unknown-sidecar":
				write(filepath.Join(p.Base, "registration.continuation.json"), []byte(`{}`))
			case "aliased-stage", "aliased-binding", "conflicting-stage-alias":
				data, _ := os.ReadFile(path)
				switch mode {
				case "aliased-stage":
					data = bytes.Replace(data, []byte(`"stage":`), []byte(`"Stage":`), 1)
				case "aliased-binding":
					data = bytes.Replace(data, []byte(`"user_id":`), []byte(`"User_ID":`), 1)
				case "conflicting-stage-alias":
					data = bytes.Replace(data, []byte(`{`), []byte(`{"Stage":"remote-outcome-pending",`), 1)
				}
				write(path, data)
				sum := sha256.Sum256(data)
				args[len(args)-2] = hex.EncodeToString(sum[:]) // Isolate schema refusal from the stale-byte fence.
			case "unknown-stage":
				data, _ := os.ReadFile(path)
				write(path, bytes.ReplaceAll(data, []byte("bootstrap-unavailable"), []byte("unknown")))
			case "bad-state":
				write(filepath.Join(p.Base, "keychain-state"), []byte(`{}`))
			case "empty-version", "duplicate-stage":
				data, _ := os.ReadFile(path)
				field := `"version":0,`
				if mode == "duplicate-stage" {
					field = `"stage":"bootstrap-unavailable",`
				}
				write(path, bytes.Replace(data, []byte(`{`), []byte(`{`+field), 1))
			case "malformed":
				write(path, []byte(`{"stage":`))
			case "extra-field", "fake-history":
				data, _ := os.ReadFile(path)
				field := `"planted-helper-secret":true,`
				if mode == "fake-history" {
					field = `"history":[],`
				}
				write(path, bytes.Replace(data, []byte(`{`), []byte(`{`+field), 1))
			case "symlink", "fifo":
				data, _ := os.ReadFile(path)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if mode == "symlink" {
					target := filepath.Join(f.root, "receipt")
					write(target, data)
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := syscall.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "nonprivate":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "item":
				f.store.items[config.SecretReference{Service: "test.alpha.token", Account: "@alice:alpha.invalid"}] = "existing"
			case "password-item":
				f.store.items[config.SecretReference{Service: "test.alpha.password", Account: "@alice:alpha.invalid"}] = "existing"
			}
			before, items := registrationTree(t, f.root), make(map[config.SecretReference]string)
			for k, v := range f.store.items {
				items[k] = v
			}
			want := "legacy receipt is stale or lacks pre-HTTP evidence"
			switch mode {
			case "no-admission", "diagnostic-only":
				want = "requires its separate explicit admission"
			case "binding", "bootstrap-ref":
				want = "registration binding mismatch"
			case "identity":
				want = "registration does not match this session's identity"
			case "metadata", "human-metadata":
				want = "destination identity metadata exists"
			case "state":
				want = "contradictory destination state"
			case "unknown-lock", "concurrent":
				want = "registration ownership unknown or concurrent"
			case "unknown-sidecar":
				want = "invalid legacy disposition provenance"
			case "malformed", "extra-field", "duplicate-stage":
				want = "malformed registration evidence"
			case "fake-history", "empty-version", "aliased-stage", "conflicting-stage-alias", "aliased-binding":
				want = "invalid legacy registration evidence"
			case "unknown-stage":
				want = "unknown registration stage"
			case "bad-state":
				want = "invalid private credential state"
			case "symlink", "fifo", "nonprivate":
				want = "registration evidence unavailable"
			case "item", "password-item":
				want = "selected credential item exists or is unavailable"
			}
			if f.app().Run(context.Background(), args) != ExitError || !strings.Contains(f.err.String(), want) {
				t.Fatalf("wrong refusal: want %q got %s", want, f.err.String())
			}
			if f.httpCalls.Load() != 0 || len(f.store.creates) != 0 || !reflect.DeepEqual(items, f.store.items) {
				t.Fatal("refusal had creation effects")
			}
			after := registrationTree(t, f.root)
			for path, v := range before {
				if after[path] != v {
					t.Fatal("refusal changed retained evidence", path)
				}
			}
			if mode != "item" && mode != "password-item" && (!reflect.DeepEqual(before, after) || len(f.store.reads) != 0) {
				t.Fatal("pre-admission refusal wrote state or accessed secrets")
			}
			if strings.Contains(f.err.String()+f.out.String(), "planted-helper-secret") {
				t.Fatal("refusal leaked input")
			}
		})
	}
}

// Proves current exclusive admission covers secret preflight: a second public
// legacy continuation cannot overlap it, even though the original v0 had no lock.
func TestNamedRegistrationLegacyExclusiveAdmission(t *testing.T) {
	f := newNamedFixture(t, false)
	original, digest := legacyRegistration(t, f, "bootstrap-unavailable")
	ref := *f.r.Profiles["alpha"].RegistrationTokenRef
	a := f.app()
	a.connectionSecretStore = registrationBootstrapStore{f.store, ref, func(ctx context.Context, p config.Paths) (string, error) {
		effects := f.effects()
		if f.app().Run(ctx, legacyArgs(digest)) != ExitError || !strings.Contains(f.err.String(), "registration ownership unavailable or concurrent") || f.effects() != effects {
			t.Fatal("concurrent legacy admission had effects")
		}
		return f.store.Read(ctx, p, ref)
	}}
	if a.Run(context.Background(), legacyArgs(digest)) != 0 || f.httpCalls.Load() != 1 || len(f.store.creates) != 2 {
		t.Fatal("exclusive success control", f.err.String())
	}
	assertLegacyRetained(t, f, original)
}

// Proves retained host and bootstrap cooldowns gate actual synthetic runner
// reads, then permit a separately admitted retry after expiry; bytes stay intact.
func TestNamedRegistrationLegacyCooldown(t *testing.T) {
	for _, hostGate := range []bool{false, true} {
		for _, early := range []bool{false, true} {
			t.Run(fmt.Sprintf("host=%t/early=%t", hostGate, early), func(t *testing.T) {
				f := newNamedFixture(t, false)
				original, digest := legacyRegistration(t, f, "bootstrap-unavailable")
				ref := *f.r.Profiles["alpha"].RegistrationTokenRef
				now := time.Now()
				probes, commands := 0, 0
				makeStore := func(p config.Paths, r config.SecretReference) *keychainStore {
					return &keychainStore{paths: p, service: r.Service, hostID: "legacy-fixture", fixture: filepath.Join(f.root, "synthetic.keychain-db"), now: func() time.Time { return now },
						probeCommand: func(ctx context.Context, args ...string) *exec.Cmd { probes++; return unlockedProbe(ctx, args...) },
						command: func(ctx context.Context, args ...string) *exec.Cmd {
							commands++
							if v, ok := f.store.items[r]; ok {
								return exec.CommandContext(ctx, "/usr/bin/printf", "%s", v)
							}
							return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 44")
						},
					}
				}
				k := makeStore(f.paths("alpha", false), ref)
				marker := ""
				err := k.locked(ref.Account, func() error {
					marker = k.identityPath(ref.Account) + ".cooldown"
					if hostGate {
						host, e := k.hostPath()
						if e != nil {
							return e
						}
						marker = host + ".cooldown"
					}
					return k.recordMarker(marker)
				})
				if err != nil {
					t.Fatal(err)
				}
				retained, err := os.ReadFile(marker)
				if err != nil {
					t.Fatal(err)
				}
				app := func() *App {
					a := f.app()
					a.connectionSecretStore = registrationGatedReads{f.store, makeStore}
					return a
				}
				args := legacyArgs(digest)
				var prior registrationJournal
				if early {
					if app().Run(context.Background(), args) == 0 || f.httpCalls.Load() != 0 || len(f.store.creates) != 0 {
						t.Fatal("cooldown bypassed")
					}
					if hostGate && (probes != 0 || commands != 0) {
						t.Fatal("host gate launched a helper")
					}
					j := legacyDisposition(t, f)
					if !j.continuationCandidate() || j.History[0].Stage != "legacy-continuation-admitted" {
						t.Fatal("lost new disposition eligibility", j)
					}
					prior = j
					before, effects := registrationTree(t, f.root), f.effects()
					f.out.Reset()
					if app().Run(context.Background(), registrationArgs("inspect", j)) != 0 || !strings.Contains(f.out.String(), `"legacy_sha256":"`+digest+`"`) || !strings.Contains(f.out.String(), `"pre_http_candidate":true`) {
						t.Fatal("disposition inspection", f.out.String(), f.err.String())
					}
					if !reflect.DeepEqual(before, registrationTree(t, f.root)) || effects != f.effects() {
						t.Fatal("inspection effects")
					}
					args = registrationArgs("continue", j)
				}
				now = now.Add(securityCooldown + time.Second)
				if app().Run(context.Background(), args) != 0 || f.httpCalls.Load() != 1 || len(f.store.creates) != 2 {
					t.Fatal("expiry success control", f.err.String())
				}
				j := legacyDisposition(t, f)
				if j.Stage != "complete" || early && (j.Attempt != prior.Attempt || !reflect.DeepEqual(j.History[:len(prior.History)], prior.History)) {
					t.Fatal("lost audit provenance")
				}
				got, err := os.ReadFile(marker)
				if err != nil || !bytes.Equal(got, retained) {
					t.Fatal("cooldown changed", err)
				}
				assertLegacyRetained(t, f, original)
			})
		}
	}
}

// Proves every later continuation binds the original bytes, new admission root,
// history revision and current ownership. The paired expiry controls are above.
func TestNamedRegistrationLegacyDispositionRefusals(t *testing.T) {
	for _, mode := range []string{"source-changed", "source-missing", "disposition-replaces-source", "provenance", "fake-root", "missing-lock", "concurrent", "stale", "password-boundary", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			f := newNamedFixture(t, false)
			_, digest := legacyRegistration(t, f, "bootstrap-unavailable")
			ref := *f.r.Profiles["alpha"].RegistrationTokenRef
			delete(f.store.items, ref)
			if f.app().Run(context.Background(), legacyArgs(digest)) == 0 {
				t.Fatal("missing bootstrap accepted")
			}
			j := legacyDisposition(t, f)
			original := append([]registrationEvent(nil), j.History...)
			f.store.items[ref] = "isolated-bootstrap"
			p := f.paths("alpha", false)
			source := filepath.Join(p.Base, "registration.pending.json")
			path := filepath.Join(p.Base, "registration.continuation.json")
			args := registrationArgs("continue", j)
			switch mode {
			case "source-changed":
				data, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(source, append(data, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			case "disposition-replaces-source":
				if err := updateRegistrationJournal(source, j); err != nil {
					t.Fatal(err)
				}
			case "source-missing":
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
			case "provenance":
				j.LegacySHA256 = strings.Repeat("0", 64)
			case "fake-root":
				j.History[0].Stage = "preflight"
			case "missing-lock":
				if err := os.Remove(filepath.Join(p.Base, "registration.lock")); err != nil {
					t.Fatal(err)
				}
			case "concurrent":
				unlock, err := lockRegistration(filepath.Join(p.Base, "registration.lock"), false)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			case "stale":
				args[len(args)-1] = "1"
			case "password-boundary", "ambiguous":
				j.History = append(j.History, registrationEvent{Stage: "continuation-admitted"}, registrationEvent{Stage: "password-storage-pending"})
				if mode == "ambiguous" {
					j.History = append(j.History, registrationEvent{Stage: "remote-outcome-pending"}, registrationEvent{Stage: "remote-outcome-ambiguous"})
				}
				j.Stage = j.History[len(j.History)-1].Stage
				args = registrationArgs("continue", j)
			}
			if mode == "provenance" || mode == "fake-root" || mode == "password-boundary" || mode == "ambiguous" {
				if err := updateRegistrationJournal(path, j); err != nil {
					t.Fatal(err)
				}
			}
			before, effects := registrationTree(t, f.root), f.effects()
			want := "invalid legacy disposition provenance"
			switch mode {
			case "source-missing":
				want = "registration evidence unavailable"
			case "disposition-replaces-source":
				want = "legacy disposition cannot replace original receipt"
			case "concurrent", "missing-lock":
				want = "registration ownership unavailable or concurrent"
			case "stale", "password-boundary", "ambiguous":
				want = "attempt is stale or lacks validated pre-HTTP continuation evidence"
			}
			if f.app().Run(context.Background(), args) != ExitError || !strings.Contains(f.err.String(), want) || effects != f.effects() || !reflect.DeepEqual(before, registrationTree(t, f.root)) {
				t.Fatal("incorrect disposition refusal or effects", mode, want, f.err.String(), original)
			}
		})
	}
}
