//go:build darwin

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Exactly one authorized fixture attempt for this run. Ordinary tests never
// invoke security or the real status binary. No live/default/login operations.
func TestKeychainALiteRealSentinel(t *testing.T) {
	requested := os.Getenv("A2A_ALITE_SENTINEL_RUN")
	if requested == "" {
		t.Skip("single separately announced fixture sentinel only")
	}
	current := os.Getenv("TASK_BOARD_RUN_ID")
	if requested != current {
		t.Fatal("sentinel successor/current run mismatch; ZERO setup operations")
	}
	// Read the durable parent directive independently; an env assertion alone
	// cannot authorize this attempt. Never create directives from this worker.
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	dcmd := exec.CommandContext(dctx, "task-board", "spawn", "directives", current, "--json")
	if err := boundSecurityProcess(dcmd); err != nil {
		t.Fatal(err)
	}
	native, err := dcmd.Output()
	if err != nil {
		t.Fatal("native parent directive unavailable; ZERO setup operations")
	}
	if err := authorizeSentinel(current, native); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../.temp/TASK-261017-1usmmi-alite-" + current)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimSentinelAttempt(current, native, root); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(root, "throwaway.keychain-db")
	bin, err := filepath.Abs("../../.temp/phase1-" + current)
	if err != nil {
		t.Fatal(err)
	}
	probePath := filepath.Join(bin, keychainProbeName)
	observer := filepath.Join(bin, "keychain-observer")
	var commands []*exec.Cmd
	// Independently observe before spending the fixture attempt, no focus/text.
	obs, err := startSentinelObserver(observer, filepath.Join(root, "observations.log"))
	if err != nil {
		t.Fatal(err)
	}
	obsFile := obs.log
	defer func() {
		if err := obs.stop(); err != nil {
			t.Error("observer failed; disposition UNKNOWN", err)
		}
	}()
	if err := waitObserverReady(context.Background(), obsFile.Name()); err != nil {
		t.Fatal("STOP observer readiness failed before fixture; NO RETRY", err)
	}
	checkObservation := func(stage string) {
		data, err := os.ReadFile(obsFile.Name())
		if err != nil || !bytes.Contains(data, []byte("observer=ready")) || bytes.Contains(data, []byte("observer=unavailable")) {
			t.Fatal("STOP observer unavailable; disposition UNKNOWN at", stage)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "new_window_count=") && !strings.Contains(line, "new_window_count=0 ") {
				t.Fatal("STOP probe/full-path caused new SecurityAgent window at", stage)
			}
		}
		t.Logf("utc=%s observation_stage=%s new_windows=0 old12571_baseline_separate=true bounds=onscreen_100ms_AX_counts_no_titles", time.Now().UTC().Format(time.RFC3339Nano), stage)
	}
	checkObservation("baseline")
	call := func(input string, args ...string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), securityDeadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		cmd.Stdin = strings.NewReader(input)
		var out, stderr limitedSecretBuffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := boundSecurityProcess(cmd); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		err := cmd.Run()
		code := 0
		if err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		t.Logf("utc=%s executable=/usr/bin/security argv=%q stdin_present=%t exit=%d timeout=%t", time.Now().UTC().Format(time.RFC3339Nano), args, input != "", code, ctx.Err() != nil)
		return out.String(), code
	}
	before, code := call("", "list-keychains", "-d", "user")
	if code != 0 {
		t.Fatal("before list failed; fixture UNRUN")
	}
	if err := os.WriteFile(filepath.Join(root, "before-list.txt"), []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	created := false
	defer func() {
		if created {
			if _, code := call("", "delete-keychain", fixture); code != 0 {
				t.Error("fixture cleanup failed")
			}
		}
		after, code := call("", "list-keychains", "-d", "user")
		if err := os.WriteFile(filepath.Join(root, "aftercleanup-list.txt"), []byte(after), 0o600); err != nil {
			t.Error(err)
		}
		if code != 0 || after != before {
			t.Error("search list differs; NO RESTORATION WRITES")
		}
		if _, err := os.Stat(fixture); !os.IsNotExist(err) {
			t.Error("fixture remains")
		}
		for _, cmd := range commands {
			if cmd.Process != nil && !errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH) {
				t.Error("own process group remains", cmd.Process.Pid)
			}
		}
		t.Logf("cleanup_fixture_absent=true original_list=%t own_groups_checked=%d default_login_writes=0 search_list_writes=0", after == before, len(commands))
	}()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(random[:])
	qpass, _ := securityQuote(password)
	qfixture, _ := securityQuote(fixture)
	created = true
	if _, code := call("create-keychain -p "+qpass+" "+qfixture+"\n", "-i"); code != 0 {
		t.Fatal("fixture create failed; NO RETRY")
	}
	afterCreate, code := call("", "list-keychains", "-d", "user")
	if err := os.WriteFile(filepath.Join(root, "aftercreate-list.txt"), []byte(afterCreate), 0o600); err != nil {
		t.Fatal(err)
	}
	if code != 0 || afterCreate != before {
		t.Fatal("STOP create changed search list; delete unlocked fixture only; no lock/probe/read/retry")
	}
	runProbe := func(want string, wantCode int) {
		ctx, cancel := context.WithTimeout(context.Background(), securityDeadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, probePath, "--fixture", fixture)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		var out, stderr limitedSecretBuffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := boundSecurityProcess(cmd); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
		err := cmd.Run()
		code := 0
		if err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		t.Logf("utc=%s executable=%s argv=[--fixture %s] exit=%d fixed_output=%q timeout=%t", time.Now().UTC().Format(time.RFC3339Nano), probePath, fixture, code, out.String(), ctx.Err() != nil)
		if code != wantCode || out.String() != want+"\n" || stderr.Len() != 0 || ctx.Err() != nil {
			t.Fatal("STOP status API unavailable/incorrect; NO RETRY")
		}
		time.Sleep(250 * time.Millisecond)
		checkObservation(want + "-probe")
	}
	// Mandatory order: locked probe, unlocked probe, full locked credential path.
	if _, code := call("", "lock-keychain", fixture); code != 0 {
		t.Fatal("lock fixture failed")
	}
	runProbe("locked", 3)
	if _, code := call("unlock-keychain -p "+qpass+" "+qfixture+"\n", "-i"); code != 0 {
		t.Fatal("synthetic fixture-only unlock failed")
	}
	runProbe("unlocked", 0)
	if _, code := call("", "lock-keychain", fixture); code != 0 {
		t.Fatal("second fixture lock failed")
	}
	p := config.Paths{Base: filepath.Join(root, "config")}
	uid := "@sentinel:sentinel.invalid"
	if err := os.MkdirAll(filepath.Dir(p.AgentFile("sentinel")), 0o700); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(config.Credentials{UserID: uid, DeviceID: "sentinel-device", Homeserver: "https://sentinel.invalid"})
	if err := os.WriteFile(p.AgentFile("sentinel"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	securityCalls, matrixCalls := 0, 0
	k := &keychainStore{paths: p, fixture: fixture, probeCommand: func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, probePath, args...)
		commands = append(commands, cmd)
		return cmd
	}, command: func(ctx context.Context, args ...string) *exec.Cmd {
		securityCalls++
		return exec.CommandContext(ctx, "/usr/bin/security", args...)
	}}
	var out, errout bytes.Buffer
	a := &App{Stdout: &out, Stderr: &errout, Stdin: strings.NewReader(""), Getenv: func(key string) string {
		switch key {
		case config.EnvConfigDir:
			return p.Base
		case envSecretStore:
			return "keychain"
		}
		return ""
	}, HTTP: nil}
	a.HTTP = &http.Client{Transport: sentinelNoNetwork{calls: &matrixCalls}}
	a.keychain = func(config.Paths) *keychainStore { return k }
	start := time.Now()
	exit := a.Run(context.Background(), []string{"--as", "sentinel", "whoami"})
	end := time.Now()
	t.Logf("App_Run_start=%s end=%s exit=%d security_calls=%d Matrix_calls=%d sanitized_error=%q", start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano), exit, securityCalls, matrixCalls, errout.String())
	if exit == 0 || securityCalls != 0 || matrixCalls != 0 || !strings.Contains(errout.String(), "preflight failed") {
		t.Error("locked product path did not fail closed before security/Matrix")
	}
	if strings.Contains(out.String()+errout.String(), password) {
		t.Error("synthetic secret escaped")
	}
	time.Sleep(time.Second)
	checkObservation("locked-full-path-before-cleanup")
}
