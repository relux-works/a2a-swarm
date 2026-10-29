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
		// Status probe runner, the only helper process left after phase 2 (#138).
		{"early-leader-probe-inherited-no-cleanup", "keychain_unix.go", `cleanupErr := killSecurityGroup(cmd)`, `var cleanupErr error; if cmd.ProcessState==nil { cleanupErr=killSecurityGroup(cmd) }`, "TestKeychainEarlyLeaderExit/inherited"},
		{"early-leader-probe-closed-no-cleanup", "keychain_unix.go", `cleanupErr := killSecurityGroup(cmd)`, `var cleanupErr error; if cmd.ProcessState==nil { cleanupErr=killSecurityGroup(cmd) }`, "TestKeychainEarlyLeaderExit/closed"},
		{"probe-only-leader-killed", "keychain_unix.go", `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)`, `syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)`, "TestKeychainProbeRefusals/timeout"},
		{"sentinel-stale-release-admitted", "keychain_observer_test.go", `release.Run == current &&`, `(release.Run == current || release.Run=="RUN-261018-123abc") &&`, "TestKeychainSentinelRunGuard/stale-release"},
		{"sentinel-agent-minted-release-admitted", "keychain_observer_test.go", `d.Actor.Kind != "operator"`, `(d.Actor.Kind != "operator" && d.Actor.Kind!="agent")`, "TestKeychainSentinelRunGuard/self-minted"},
		{"sentinel-missing-keeper-admitted", "keychain_observer_test.go", `event.MatchString(release.Keeper)`, `(event.MatchString(release.Keeper) || release.Keeper=="")`, "TestKeychainSentinelRunGuard/missing-keeper"},
		{"observer-expired-ready-admitted", "keychain_observer_test.go", `if ctx.Err() != nil {`, `if ctx.Err() != nil && ctx.Err()!=context.Canceled {`, "TestKeychainObserverReadiness/expired-ready"},
		{"observer-empty-log-admitted", "keychain_observer_test.go", `if bytes.Contains(data, []byte("observer=ready")) {`, `if bytes.Contains(data, []byte("observer=ready")) || len(data)==0 {`, "TestKeychainObserverReadiness/never"},
		{"probe-locked-pair-admitted", "keychain_probe.go", `err := runBoundSecurityProcess(cmd)`, `err := runBoundSecurityProcess(cmd); if ee,ok:=err.(*exec.ExitError);ok && ee.ExitCode()==3 && out.String()=="locked\n" {err=nil}; if out.String()=="locked\n" {out.Reset();out.WriteString("unlocked\n")}`, "TestKeychainProbeRefusals/locked"},
		{"probe-unlocked-bad-exit-admitted", "keychain_probe.go", `err := runBoundSecurityProcess(cmd)`, `err := runBoundSecurityProcess(cmd); if ee,ok:=err.(*exec.ExitError);ok && ee.ExitCode()==3 && out.String()=="unlocked\n" {err=nil}`, "TestKeychainProbeRefusals/badexit"},
		{"probe-one-garbage-suffix-admitted", "keychain_probe.go", `out.String() != "unlocked\n"`, `(out.String() != "unlocked\n" && out.String()!="unlocked\ngarbage\n")`, "TestKeychainProbeRefusals/garbage"},
		{"probe-missing-only-admitted", "keychain_probe.go", `err := runBoundSecurityProcess(cmd)`, `err := runBoundSecurityProcess(cmd);if errors.Is(err,os.ErrNotExist) {err=nil;out.WriteString("unlocked\n")}`, "TestKeychainProbeRefusals/missing"},
		{"probe-stderr-danger-admitted", "keychain_probe.go", `stderr.Len() != 0`, `(stderr.Len()!=0 && stderr.String()!="danger")`, "TestKeychainProbeRefusals/stderr"},
		{"probe-resolves-launcher-instead", "keychain_probe.go", `filepath.Dir(real)`, `filepath.Dir(func()string{_ = real; return executable}())`, "TestKeychainProbeResolvedExecutable"},
		{"fixture-probe-other-target", "keychain_probe.go", `args = []string{"--fixture", k.fixture}`, `args = []string{"--fixture", k.fixture+".other"}`, "TestKeychainProbeTargetPair"},
		{"host-marker-bob-exemption", "keychain.go", `if k.clock().UnixNano() < until {`, `if k.clock().UnixNano() < until && !(uid=="@bob:test.local" && strings.HasPrefix(filepath.Base(path),"host-")) {`, "TestKeychainHostCooldown"},
		{"preflight-bypassed", "keychain.go", `if err := k.preflight(ctx); err != nil {`, `if err := k.preflight(ctx); err != nil && false {`, "TestKeychainProbeRefusals/locked"},

		// In-process backend boundary (phase 2).
		{"backend-target-not-fixture", "keychain.go", `return b.find(ctx, target, k.selectedService(), uid)`, `return b.find(ctx, target+".other", k.selectedService(), uid)`, "TestKeychainProbeTargetPair"},
		{"backend-service-not-selected", "keychain.go", `return b.find(ctx, target, k.selectedService(), uid)`, `return b.find(ctx, target, keychainService, uid)`, "TestNamedKeychainAdapterRouting"},
		{"backend-error-text-escapes", "keychain.go", `return nil, fmt.Errorf("keychain operation failed for %s", uid)`, `return nil, fmt.Errorf("keychain operation failed for %s: %v", uid, r.err)`, "TestKeychainBackendTextNeverEscapes"},
		{"backend-failure-as-absence", "keychain.go", `return nil, fmt.Errorf("keychain operation failed for %s", uid)`, `return nil, errKeychainMissing`, "TestKeychainBackendFailureMigrationStages/lookup/error"},
		{"interaction-no-cooldown", "keychain.go", `if ctx.Err() != nil || errors.Is(r.err, errKeychainInteraction) {`, `if ctx.Err() != nil {`, "TestKeychainBackendFailureMigrationStages/lookup/interaction"},
		{"deadline-not-enforced", "keychain.go", `ctx, cancel := context.WithTimeout(ctx, k.timeoutDuration())`, `ctx, cancel := context.WithTimeout(ctx, 10*k.timeoutDuration())`, "TestKeychainDeadlineBoundsOperation"},
		{"control-nul-admitted", "keychain.go", `if r < 32 || r == 127 {`, `if (r < 32 || r == 127) && r!=0 {`, "TestKeychainTokenBytesAndRefusals"},
		{"long-token-truncated-update", "keychain.go", `return nil, b.update(ctx, target, k.selectedService(), uid, []byte(token))`, `return nil, b.update(ctx, target, k.selectedService(), uid, []byte(token)[:min(len(token), 4095)])`, "TestKeychainTokenBytesAndRefusals"},
		{"long-token-truncated", "keychain.go", `return nil, b.add(ctx, target, k.selectedService(), uid, []byte(token))`, `return nil, b.add(ctx, target, k.selectedService(), uid, []byte(token)[:min(len(token), 4095)])`, "TestKeychainTokenBytesAndRefusals"},
		{"create-uses-update", "keychain.go", `err = put(existing)`, `err = put(true)`, "TestKeychainRegistrationAndUpsert/create"},
		{"existing-uses-add", "keychain.go", `err = put(existing)`, `err = put(false)`, "TestKeychainRegistrationAndUpsert/existing"},
		{"duplicate-second-retry", "keychain.go", `err = put(true)`, `err = put(true); if errors.Is(err,errKeychainDuplicate) { err=put(true) }`, "TestKeychainDuplicateRetryBound"},
		{"production-names-security-tool", "keychain_backend.go", `var errKeychainInteraction = errors.New("keychain interaction not allowed")`, `var errKeychainInteraction = errors.New("keychain interaction not allowed"); var plantedHelper = "/usr/bin/security"`, "TestKeychainProductionNeverNamesSecurityTool"},

		// Abandoned-write fence and reconcile (#138 review, finding 1).
		{"fence-check-skipped", "keychain.go", `if !k.reconciling {`, `if false {`, "TestKeychainAbandonedWriteStaysFenced"},
		{"fence-not-written", "keychain.go", `if mutation != "" {`, `if mutation == "never" {`, "TestKeychainAbandonedWriteStaysFenced"},
		{"fence-cleared-on-timeout", "keychain.go", `r.err = ctx.Err()`, `r.err = ctx.Err(); if fence != nil { _ = k.clearFence(uid, fence) }`, "TestKeychainAbandonedWriteStaysFenced"},
		{"dead-owner-fence-expires", "keychain_fence.go", "return fmt.Errorf(\"%w for %s: pid %d ended before its write completed; run `a2a creds reconcile`\", errKeychainFenced, uid, fence.PID)", `return nil`, "TestKeychainFenceReconcile/gone-observed"},
		{"reconcile-live-owner", "keychain_fence.go", "case ownerAlive:\n\t\t\treturn fmt.Errorf(\"%w for %s in pid %d; not reconciled\", errKeychainFenced, uid, fence.PID)", "case ownerAlive:", "TestKeychainFenceReconcile/live-owner"},
		{"reconcile-unanswered", "keychain_fence.go", "\t\tdefault:\n\t\t\treturn fmt.Errorf(\"keychain did not answer for %s; fence kept: %w\", uid, err)", "\t\tdefault:\n\t\t\toutcome = \"unanswered\"\n\t\t\treturn k.removeFence(uid)", "TestKeychainFenceReconcile/gone-silent"},

		// Round 3: liveness, completion evidence, value skip, recorded fence, ACL requirements.
		{"liveness-inspection-error-gone", "keychain_unix.go", "case idErr != nil || current == \"\":\n\t\treturn ownerUnknown", "case idErr != nil || current == \"\":\n\t\treturn ownerGone", "TestKeychainLivenessClassification/ps-unavailable"},
		{"liveness-probe-error-gone", "keychain_unix.go", "case killErr != nil && !errors.Is(killErr, syscall.EPERM):\n\t\treturn ownerUnknown", "case killErr != nil && !errors.Is(killErr, syscall.EPERM):\n\t\treturn ownerGone", "TestKeychainLivenessClassification/probe-error"},
		{"reconcile-unknown-proceeds", "keychain_fence.go", "\t\tcase ownerUnknown:\n\t\t\treturn fmt.Errorf(\"%w for %s: the state of pid %d is unknown; fence kept\", errKeychainFenced, uid, fence.PID)\n", "", "TestKeychainUnknownOwnerKeepsFence"},
		{"check-unknown-admits", "keychain_fence.go", "return fmt.Errorf(\"%w for %s: the state of pid %d is unknown; refusing\", errKeychainFenced, uid, fence.PID)", "return nil", "TestKeychainUnknownOwnerKeepsFence"},
		{"reconcile-any-answer-clears", "keychain_fence.go", "case err == nil && tokenDigest([]byte(token)) == fence.Digest:", "case err == nil:", "TestKeychainDeadWriterQueuedRequest"},
		{"reconcile-boot-always-quiescent", "keychain_fence.go", "if boot, err := k.bootSession(); err == nil && fence.Boot != 0 && boot != fence.Boot {", "if boot, err := k.bootSession(); err == nil || boot != fence.Boot {", "TestKeychainDeadWriterQueuedRequest"},
		{"write-rewrites-present-value", "keychain.go", "\t\tif existing && prior == token {\n\t\t\treturn nil\n\t\t}\n", "\t\t_ = prior\n", "TestKeychainWriteSkipsPresentValue/existing-equal"},
		{"fence-digest-not-recorded", "keychain_fence.go", "Digest: tokenDigest(intended)}", "Digest: tokenDigest(nil)}", "TestKeychainAbandonedWriteStaysFenced"},
		{"acl-requirement-ignored", "keychain_acl_test.go", "if app.requirement != requirement {", "if false && app.requirement != requirement {", "TestKeychainACLCreatorOnly/broadened-requirement.txt"},
		{"acl-status-ignored", "keychain_acl_test.go", "if app.status != \"OK\" {", "if false {", "TestKeychainACLCreatorOnly/status-not-ok.txt"},
		{"acl-unknown-auth-admitted", "keychain_acl_test.go", "if !aclAuthorizations[a] {", "if false && !aclAuthorizations[a] {", "TestKeychainACLCreatorOnly/unknown-auth.txt"},

		// Native lengths (finding 4), sentinel tool bounding (finding 2), ACL rules (finding 3).
		{"input-length-unbounded", "keychain_backend.go", `return n >= 0 && uint64(n) <= math.MaxUint32`, `return n >= 0 && uint64(n) <= math.MaxUint64`, "TestKeychainNativeLengthBounds"},
		{"output-length-uint32", "keychain_backend.go", `return n <= math.MaxInt32`, `return n <= math.MaxUint32`, "TestKeychainNativeLengthBounds"},
		{"sentinel-tool-unbounded", "keychain_p2_sentinel_darwin_test.go", `	if err := boundSecurityProcess(cmd); err != nil {
		return "", -1
	}
	if err := runBoundSecurityProcess(cmd); err != nil {`, `	if err := runBoundSecurityProcess(cmd); err != nil {`, "TestSentinelToolBoundProcess"},
		{"acl-decrypt-only", "keychain_acl_test.go", `case "decrypt", "export_clear", "export_wrapped", "any":`, `case "decrypt":`, "TestKeychainACLCreatorOnly/allow-all-export.txt"},
		{"acl-any-app-admitted", "keychain_acl_test.go", `if e.anyApp {`, `if false && e.anyApp {`, "TestKeychainACLCreatorOnly/allow-all-export.txt"},
		{"acl-unknown-line-skipped", "keychain_acl_test.go", `		default:
			return nil, fmt.Errorf("unknown ACL line: %q", line)`, `		default:`, "TestKeychainACLCreatorOnly/unknown-line.txt"},

		// Owner decision 74: no human token in a2a Keychain items.
		{"human-legacy-read-admitted", "credentials.go", `if a.human {
			return config.Credentials{}, errHumanKeychain
		}`, ``, "TestKeychainAuthorizationBoundaries"},
		{"human-migrate-admitted", "credentials.go", `if a.human {
		return errHumanKeychain
	}`, ``, "TestKeychainAuthorizationBoundaries"},
		{"human-named-read-admitted", "connection_registration.go", `if a.human {
		return "", errHumanKeychain
	}`, ``, "TestNamedHumanClassBoundary"},
		{"human-named-create-admitted", "connection_registration.go", `func (a *App) createConnectionSecret(ctx context.Context, p config.Paths, r config.SecretReference, v string) error {
	if a.human {
		return errHumanKeychain
	}`, `func (a *App) createConnectionSecret(ctx context.Context, p config.Paths, r config.SecretReference, v string) error {`, "TestNamedHumanSecretCreateRefused"},

		// Store logic unchanged by phase 2.
		{"alice-identity-env-bypass", "app.go", `if name != "" && env != "" && name != env {`, `if name != "" && env != "" && name != env && name!="alice" {`, "TestKeychainAuthorizationBoundaries"},
		{"public-alice-file-admitted", "../config/config.go", `if perm := st.Mode().Perm(); perm != SecretFileMode {`, `if perm := st.Mode().Perm(); perm != SecretFileMode && !(perm==0o644 && filepath.Base(path)=="alice.json") {`, "TestKeychainMigrationScopeAndFileGuards"},
		{"alice-password-login-reintroduced", "app.go", `return a.fail(c.run(ctx, a, rest[1:]))`, `err:=c.run(ctx,a,rest[1:]); if errors.Is(err,matrix.ErrAuth) && a.as=="alice" { if p,e:=a.paths(); e==nil { if cr,e:=config.LoadCredentials(p,"alice");e==nil { mc:=matrix.New(cr.Homeserver,"");mc.HTTP=a.HTTP;_,_=mc.Login(ctx,cr.UserID,cr.Password,cr.DeviceID) } } }; return a.fail(err)`, "TestReloginWhoami"},
		{"unknown-typo-admitted", "credentials.go", `case "keychain":`, `case "keychain", "typo":`, "TestKeychainCredentialSelection/unknown"},
		{"missing-only-file-fallback", "credentials.go", `c.AccessToken = "" // A file token`, `fileToken := c.AccessToken; c.AccessToken = "" // A file token`, "TestKeychainCredentialSelection/missing"},
		{"metadata-bob-alias", "credentials.go", `prefix := "@" + name + ":"`, `if name=="alice" && c.UserID=="@bob:test.local" { return nil }; prefix := "@" + name + ":"`, "TestKeychainMetadataIdentityMismatch"},
		{"empty-only-admitted", "keychain.go", `strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n")`, `strings.ContainsAny(token, "\x00\r\n")`, "TestKeychainCredentialSelection/empty"},
		{"alice-cooldown-exemption", "keychain.go", `if k.clock().UnixNano() < until {`, `if k.clock().UnixNano() < until && uid!="@alice:test.local" {`, "TestKeychainInteractionCooldownAllCalls"},
		{"corrupt-marker-treated-expired", "keychain.go", `if err != nil {
		return fmt.Errorf("keychain cooldown state invalid for %s", uid)
	}`, `if err != nil && string(data)!="not-a-time" {
		return fmt.Errorf("keychain cooldown state invalid for %s", uid)
	}`, "TestKeychainCooldownStateFailClosed/corrupt"},
		{"one-readback-mismatch-admitted", "keychain.go", `if got != token {`, `if got != token && got!="mismatching-sentinel" {`, "TestKeychainMigrationRecoverability/mismatch"},
		{"cancel-before-scrub-admitted", "credentials.go", `if err := ctx.Err(); err != nil {`, `if err := ctx.Err(); err != nil && err!=context.Canceled {`, "TestKeychainMigrationInterruptedAndAtomicFailure/cancel"},
		{"newbie-password-persisted", "account.go", `fileCreds := creds`, `fileCreds := creds; if name=="newbie" { fileCreds.Password=password }`, "TestRegister"},
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
			cmd.Env = append(os.Environ(), "A2A_RUN_KEYCHAIN_MUTANTS=", "A2A_P2_SENTINEL_ALLOWANCE=", "A2A_ALITE_SENTINEL_RUN=", "A2A_OBSERVER_DRY_RUN=", "A2A_MATRIX_IT=")
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
			t.Logf("KILLED mutant=%s test=%s gate_exit=%d\n%s", m.name, m.test, status, out)
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "--- FAIL:") {
					fmt.Println(line)
				}
			}
		})
	}
}
