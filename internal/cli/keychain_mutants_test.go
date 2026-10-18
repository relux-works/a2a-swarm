//go:build unix

package cli

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Opt-in scoped behavioral mutations. Each runs a copied candidate; the working
// tree is never mutated. The expected named test must fail, not merely setup.
func TestKeychainNarrowingMutants(t *testing.T) {
	if os.Getenv("A2A_RUN_KEYCHAIN_MUTANTS") != "1" {
		t.Skip("explicit scoped mutant evidence")
	}
	type mutant struct{ name, file, old, next, test string }
	mutants := []mutant{
		{"sentinel-stale-release-admitted", "keychain_observer_test.go", `release.Run == current &&`, `(release.Run == current || release.Run=="RUN-261018-123abc") &&`, "TestKeychainSentinelRunGuard/stale-release"},
		{"sentinel-agent-minted-release-admitted", "keychain_observer_test.go", `d.Actor.Kind != "operator"`, `(d.Actor.Kind != "operator" && d.Actor.Kind!="agent")`, "TestKeychainSentinelRunGuard/self-minted"},
		{"sentinel-missing-keeper-admitted", "keychain_observer_test.go", `event.MatchString(release.Keeper)`, `(event.MatchString(release.Keeper) || release.Keeper=="")`, "TestKeychainSentinelRunGuard/missing-keeper"},
		{"observer-expired-ready-admitted", "keychain_observer_test.go", `if ctx.Err() != nil {`, `if ctx.Err() != nil && ctx.Err()!=context.Canceled {`, "TestKeychainObserverReadiness/expired-ready"},
		{"observer-empty-log-admitted", "keychain_observer_test.go", `if bytes.Contains(data, []byte("observer=ready")) {`, `if bytes.Contains(data, []byte("observer=ready")) || len(data)==0 {`, "TestKeychainObserverReadiness/never"},
		{"probe-locked-pair-admitted", "keychain_probe.go", `err := cmd.Run()`, `err := cmd.Run(); if ee,ok:=err.(*exec.ExitError);ok && ee.ExitCode()==3 && out.String()=="locked\n" {err=nil}; if out.String()=="locked\n" {out.Reset();out.WriteString("unlocked\n")}`, "TestKeychainProbeRefusals/locked"},
		{"probe-unlocked-bad-exit-admitted", "keychain_probe.go", `err := cmd.Run()`, `err := cmd.Run(); if ee,ok:=err.(*exec.ExitError);ok && ee.ExitCode()==3 && out.String()=="unlocked\n" {err=nil}`, "TestKeychainProbeRefusals/badexit"},
		{"probe-one-garbage-suffix-admitted", "keychain_probe.go", `out.String() != "unlocked\n"`, `(out.String() != "unlocked\n" && out.String()!="unlocked\ngarbage\n")`, "TestKeychainProbeRefusals/garbage"},
		{"probe-missing-only-admitted", "keychain_probe.go", `err := cmd.Run()`, `err := cmd.Run();if errors.Is(err,os.ErrNotExist) {err=nil;out.WriteString("unlocked\n")}`, "TestKeychainProbeRefusals/missing"},
		{"probe-stderr-danger-admitted", "keychain_probe.go", `stderr.Len() != 0`, `(stderr.Len()!=0 && stderr.String()!="danger")`, "TestKeychainProbeRefusals/stderr"},
		{"host-marker-bob-exemption", "keychain.go", `if k.clock().UnixNano() < until {`, `if k.clock().UnixNano() < until && !(uid=="@bob:test.local" && strings.HasPrefix(filepath.Base(path),"host-")) {`, "TestKeychainHostCooldown"},
		{"probe-only-leader-killed", "keychain_unix.go", `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)`, `syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)`, "TestKeychainProbeRefusals/timeout"},
		{"probe-resolves-launcher-instead", "keychain_probe.go", `filepath.Dir(real)`, `filepath.Dir(func()string{_ = real; return executable}())`, "TestKeychainProbeResolvedExecutable"},
		{"fixture-probe-other-target", "keychain_probe.go", `args = []string{"--fixture", k.fixture}`, `args = []string{"--fixture", k.fixture+".other"}`, "TestKeychainProbeTargetPair"},
		{"write-only-preflight-bypass", "keychain.go", `if err := k.preflight(ctx); err != nil {`, `if err := k.preflight(ctx); err != nil && input!="" {`, "TestKeychainProbeRefusals/locked"},
		{"stdin-nul-control-admitted", "keychain.go", `if r < 32 || r == 127 {`, `if (r < 32 || r == 127) && r!=0 {`, "TestKeychainCommandGrammarAndLimits"},
		{"one-overlength-command-admitted", "keychain.go", `if len(line)+1 >= 4096 {`, `if len(line)+1 >= 4096 && len(token)!=4096 {`, "TestKeychainCommandGrammarAndLimits"},

		{"alice-identity-env-bypass", "app.go", `if name != "" && env != "" && name != env {`, `if name != "" && env != "" && name != env && name!="alice" {`, "TestKeychainAuthorizationBoundaries"},
		{"public-alice-file-admitted", "../config/config.go", `if perm := st.Mode().Perm(); perm != SecretFileMode {`, `if perm := st.Mode().Perm(); perm != SecretFileMode && !(perm==0o644 && filepath.Base(path)=="alice.json") {`, "TestKeychainMigrationScopeAndFileGuards"},
		{"alice-password-login-reintroduced", "app.go", `return a.fail(c.run(ctx, a, rest[1:]))`, `err:=c.run(ctx,a,rest[1:]); if errors.Is(err,matrix.ErrAuth) && a.as=="alice" { if p,e:=a.paths(); e==nil { if cr,e:=config.LoadCredentials(p,"alice");e==nil { mc:=matrix.New(cr.Homeserver,"");mc.HTTP=a.HTTP;_,_=mc.Login(ctx,cr.UserID,cr.Password,cr.DeviceID) } } }; return a.fail(err)`, "TestReloginWhoami"},
		{"stdin-leaked-to-argv", "keychain.go", `cmd.Stdin = strings.NewReader(input)`, `cmd.Stdin = strings.NewReader(input); if input!="" { cmd.Args=append(cmd.Args,input) }`, "TestKeychainProcessBoundaryLeaks"},
		{"unknown-typo-admitted", "credentials.go", `case "keychain":`, `case "keychain", "typo":`, "TestKeychainCredentialSelection/unknown"},
		{"missing-only-file-fallback", "credentials.go", `c.AccessToken = "" // A file token`, `fileToken := c.AccessToken; c.AccessToken = "" // A file token`, "TestKeychainCredentialSelection/missing"},
		{"metadata-bob-alias", "credentials.go", `prefix := "@" + name + ":"`, `if name=="alice" && c.UserID=="@bob:test.local" { return nil }; prefix := "@" + name + ":"`, "TestKeychainMetadataIdentityMismatch"},
		{"empty-only-admitted", "keychain.go", `strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n")`, `strings.ContainsAny(token, "\x00\r\n")`, "TestKeychainCredentialSelection/empty"},
		{"create-newbie-no-trust", "keychain.go", `line += " -T /usr/bin/security"`, `if uid!="@newbie:test.local" { line += " -T /usr/bin/security" }`, "TestKeychainRegistrationAndUpsert/create"},
		{"existing-update-rewrites-trust", "keychain.go", `line += " -U"`, `line += " -U -T /usr/bin/security"`, "TestKeychainRegistrationAndUpsert/existing"},
		{"duplicate-second-retry", "keychain.go", `err = write(true)`, `err = write(true); if errors.Is(err,errKeychainDuplicate) { err=write(true) }`, "TestKeychainDuplicateRetryBound"},
		{"leader-only-deadline", "keychain_unix.go", `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)`, `syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)`, "TestKeychainDeadlineKillsProcessGroup"},
		{"alice-cooldown-exemption", "keychain.go", `if k.clock().UnixNano() < until {`, `if k.clock().UnixNano() < until && uid!="@alice:test.local" {`, "TestKeychainInteractionCooldownAllCalls"},
		{"corrupt-marker-treated-expired", "keychain.go", `if err != nil {
		return fmt.Errorf("keychain cooldown state invalid for %s", uid)
	}`, `if err != nil && string(data)!="not-a-time" {
		return fmt.Errorf("keychain cooldown state invalid for %s", uid)
	}`, "TestKeychainCooldownStateFailClosed/corrupt"},
		{"one-readback-mismatch-admitted", "keychain.go", `if got != token {`, `if got != token && got!="mismatching-sentinel" {`, "TestKeychainMigrationRecoverability/mismatch"},
		{"cancel-before-scrub-admitted", "credentials.go", `if err := ctx.Err(); err != nil {`, `if err := ctx.Err(); err != nil && err!=context.Canceled {`, "TestKeychainMigrationInterruptedAndAtomicFailure/cancel"},
		{"newbie-password-persisted", "account.go", `fileCreds := creds`, `fileCreds := creds; if name=="newbie" { fileCreds.Password=password }`, "TestRegister"},
		{"stdin-leaked-to-environment", "keychain.go", `cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}`, `cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}; if input!="" {cmd.Env=append(cmd.Env,"PLANTED_LEAK="+input)}`, "TestKeychainProcessBoundaryLeaks"},
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mutants {
		t.Run(m.name, func(t *testing.T) {
			copyRoot := t.TempDir()
			for _, name := range []string{"go.mod", "go.sum", "internal"} {
				src := filepath.Join(root, name)
				err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					rel, e := filepath.Rel(root, path)
					if e != nil {
						return e
					}
					dst := filepath.Join(copyRoot, rel)
					if d.IsDir() {
						return os.MkdirAll(dst, 0o700)
					}
					data, e := os.ReadFile(path)
					if e != nil {
						return e
					}
					return os.WriteFile(dst, data, 0o600)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(copyRoot, "internal", "cli", m.file)
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), m.old) != 1 {
				t.Fatalf("mutation target must be unique: %q", m.old)
			}
			next := strings.Replace(string(data), m.old, m.next, 1)
			// Coordinated weakening across the same gate: keep all other error classes
			// refusing, but admit exactly missing-file fallback / empty-token outcomes.
			if m.name == "missing-only-file-fallback" {
				next = strings.Replace(next, `c.AccessToken, err = a.keychainStore(p).read(ctx, c.UserID)`, `c.AccessToken, err = a.keychainStore(p).read(ctx, c.UserID); if err!=nil && strings.Contains(err.Error(),"item missing") { c.AccessToken=fileToken; err=nil }`, 1)
			}
			if err := os.WriteFile(target, []byte(next), 0o600); err != nil {
				t.Fatal(err)
			}
			if m.name == "empty-only-admitted" {
				path := filepath.Join(copyRoot, "internal", "cli", "credentials.go")
				b, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				b = []byte(strings.Replace(string(b), `if c.AccessToken == "" {`, `if c.AccessToken == "" && store!="keychain" {`, 1))
				if e := os.WriteFile(path, b, 0o600); e != nil {
					t.Fatal(e)
				}
			}
			if m.name == "newbie-password-persisted" {
				path := filepath.Join(copyRoot, "internal", "config", "config.go")
				b, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				b = []byte(strings.Replace(string(b), `c.Password = ""`, `if c.UserID!="@newbie:test.local" { c.Password = "" }`, 1))
				if e := os.WriteFile(path, b, 0o600); e != nil {
					t.Fatal(e)
				}
			}
			mask := "^" + strings.ReplaceAll(m.test, "/", "$/^") + "$"
			cmd := exec.Command("go", "test", "-count=1", "./internal/cli", "-run", mask)
			cmd.Dir = copyRoot
			cmd.Env = append(os.Environ(), "A2A_RUN_KEYCHAIN_MUTANTS=", "A2A_PHASE1_SENTINEL_RUN=", "A2A_MATRIX_IT=")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("SURVIVOR %s: bound remains unproved; test %s passed", m.name, m.test)
			}
			var status int
			if ee, ok := err.(*exec.ExitError); ok {
				status = ee.ExitCode()
			} else {
				t.Fatal(err)
			}
			leaf := m.test[strings.LastIndex(m.test, "/")+1:]
			if !strings.Contains(string(out), "--- FAIL: "+leaf+" ") && !strings.Contains(string(out), "--- FAIL: "+m.test+" ") {
				t.Fatalf("not a named behavioral kill (exit %d): %s", status, out)
			}
			t.Logf("KILLED mutant=%s test=%s gate_exit=%d", m.name, m.test, status)
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "--- FAIL:") {
					fmt.Println(line)
				}
			}
		})
	}
}
