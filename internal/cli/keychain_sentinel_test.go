//go:build darwin

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// Only this explicit test seam can select a throwaway Keychain. Ordinary tests
// skip it; production exposes no Keychain path or executable override.
func TestKeychainRealPathSentinel(t *testing.T) {
	t.Skip("historical f687ba sentinel allowance spent RED; never rerun")

	if os.Getenv("A2A_PHASE1_SENTINEL_RUN") != "RUN-261017-f687ba" {
		t.Skip("one separately authorized producer-only fixture sentinel; never ordinary CI")
	}
	root, err := filepath.Abs("../../.temp/TASK-261017-1usmmi-sentinel-RUN-261017-f687ba")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(root, 0o700); err != nil {
		t.Fatal("single sentinel directory already used or unavailable; NO RETRY", err)
	}
	fixture := filepath.Join(root, "throwaway.keychain-db")
	observer, err := filepath.Abs("../../.temp/keychain-observer")
	if err != nil {
		t.Fatal(err)
	}
	fixtureCreated := false
	var commands []*exec.Cmd
	// Read/write fixture setup operations are outside the credential cooldown;
	// cleanup must delete ONLY this fixture even after the one read cools down.
	call := func(input string, args ...string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), securityDeadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		cmd.Stdin = strings.NewReader(input)
		var out, stderr limitedSecretBuffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
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
		t.Fatal("before list failed; no fixture created")
	}
	_ = os.WriteFile(filepath.Join(root, "before-list.txt"), []byte(before), 0o600)
	defaultBefore, code := call("", "default-keychain", "-d", "user")
	if code != 0 {
		t.Fatal("before default metadata failed")
	}
	defer func() {
		if fixtureCreated {
			_, code := call("", "delete-keychain", fixture)
			if code != 0 {
				t.Error("fixture-only cleanup failed")
			}
		}
		after, code := call("", "list-keychains", "-d", "user")
		_ = os.WriteFile(filepath.Join(root, "aftercleanup-list.txt"), []byte(after), 0o600)
		if code != 0 || after != before {
			t.Error("search list not original after cleanup; NO RESTORATION WRITES")
		}
		defaultAfter, code := call("", "default-keychain", "-d", "user")
		if code != 0 || defaultAfter != defaultBefore {
			t.Error("default Keychain changed; NO RESTORATION WRITES")
		}
		if _, err := os.Stat(fixture); !os.IsNotExist(err) {
			t.Error("fixture still exists after cleanup")
		}
		for _, cmd := range commands {
			if cmd.Process != nil {
				err := syscall.Kill(-cmd.Process.Pid, 0)
				if !errors.Is(err, syscall.ESRCH) {
					t.Errorf("own process group still present pid=%d status=%v", cmd.Process.Pid, err)
				}
			}
		}
		t.Logf("own_process_groups_checked=%d", len(commands))
		t.Logf("fixture_cleanup_checked=true search_list_original=%t default_original=%t", after == before, defaultAfter == defaultBefore)
	}()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	password := hex.EncodeToString(random[:])
	qfixture, _ := securityQuote(fixture)
	qpass, _ := securityQuote(password)
	// Arm fixture-only cleanup before creation: a timeout could still have created it.
	fixtureCreated = true
	_, code = call("create-keychain -p "+qpass+" "+qfixture+"\n", "-i")
	if code != 0 {
		t.Fatal("fixture setup failed; NO RETRY")
	}
	afterCreate, code := call("", "list-keychains", "-d", "user")
	_ = os.WriteFile(filepath.Join(root, "aftercreate-list.txt"), []byte(afterCreate), 0o600)
	if code != 0 || afterCreate != before {
		t.Fatal("STOP final0013: create changed search list; fixture stays UNLOCKED, no item creation/lock/read, delete fixture only; sentinel read and ACL proof UNRUN")
	}
	// No -T for exactly this authorized fixture-only default-creator ACL probe.
	_, code = call("add-generic-password -s "+keychainService+" -a @default-acl:sentinel.invalid -w "+qpass+" "+qfixture+"\n", "-i")
	if code != 0 {
		t.Fatal("default ACL setup failed; NO RETRY")
	}
	acl, code := call("", "dump-keychain", "-a", fixture) // NEVER -d
	if code != 0 || !strings.Contains(acl, "/usr/bin/security") {
		t.Fatal("STOP: default creator ACL does not prove /usr/bin/security trust")
	}
	_ = os.WriteFile(filepath.Join(root, "default-acl.txt"), []byte(acl), 0o600)
	t.Log("default_creator_acl_security_trust=true dump_without_data=true only_default_probe_item_present=true")
	uid := "@sentinel:sentinel.invalid"
	p := config.Paths{Base: filepath.Join(root, "config")}
	k := &keychainStore{paths: p, fixture: fixture}
	k.command = func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
		commands = append(commands, cmd)
		return cmd
	}
	if err := k.write(context.Background(), uid, password); err != nil {
		t.Fatal("explicit-T fixture item and readback failed; NO RETRY", err)
	}
	// Establish observation before spending the single locked read.
	pre := exec.Command(observer, "--preflight")
	preout, err := pre.Output()
	if err != nil || !bytes.Contains(preout, []byte("observer=ready")) {
		t.Fatal("STOP: reliable non-focus observer unavailable; read UNSPENT")
	}
	obsFile, err := os.OpenFile(filepath.Join(root, "observations.log"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer obsFile.Close()
	obs := exec.Command(observer)
	obs.Stdout, obs.Stderr = obsFile, obsFile
	if err := obs.Start(); err != nil {
		t.Fatal("observer unavailable; read UNSPENT", err)
	}
	defer func() {
		if err := obs.Wait(); err != nil {
			t.Error("observer failure; dialog disposition UNKNOWN", err)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	observed, err := os.ReadFile(obsFile.Name())
	if err != nil || !bytes.Contains(observed, []byte("observer=ready")) {
		t.Fatal("observer did not become ready; read UNSPENT")
	}
	_, code = call("", "lock-keychain", fixture)
	if code != 0 {
		t.Fatal("fixture lock failed; NO RETRY")
	}
	if err := os.MkdirAll(filepath.Dir(p.AgentFile("sentinel")), 0o700); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(config.Credentials{UserID: uid, DeviceID: "sentinel-device", Homeserver: "https://sentinel.invalid"})
	if err := os.WriteFile(p.AgentFile("sentinel"), metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errout bytes.Buffer
	calls := 0
	a := &App{Stdout: &out, Stderr: &errout, Stdin: strings.NewReader(""), Getenv: func(key string) string {
		switch key {
		case config.EnvConfigDir:
			return p.Base
		case envSecretStore:
			return "keychain"
		}
		return ""
	}, HTTP: &http.Client{Transport: sentinelNoNetwork{calls: &calls}}}
	a.keychain = func(config.Paths) *keychainStore { return k }
	start := time.Now()
	result := a.Run(context.Background(), []string{"--as", "sentinel", "whoami"})
	end := time.Now()
	if strings.Contains(out.String()+errout.String(), password) {
		t.Error("secret escaped sentinel output")
	}
	t.Logf("single_locked_App_Run_started=%s ended=%s exit=%d elapsed=%s matrix_calls=%d sanitized_stderr=%q", start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano), result, end.Sub(start), calls, errout.String())
	for _, cmd := range commands {
		if cmd.ProcessState != nil {
			t.Logf("own_security_pid=%d actual_exit=%d", cmd.Process.Pid, cmd.ProcessState.ExitCode())
		}
	}
	if result == 0 || calls != 0 {
		t.Error("locked read did not fail before Matrix")
	}
	// Observe survival BEFORE deleting the fixture. Polls are 100ms; sub-poll,
	// hidden or non-SecurityAgent UI remains an explicitly stated bound.
	time.Sleep(time.Second)
	observed, err = os.ReadFile(obsFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	appeared, survived := false, false
	for _, line := range strings.Split(string(observed), "\n") {
		if strings.HasPrefix(line, "security_window ") {
			appeared = true
		}
		if strings.HasPrefix(line, "at=") && !strings.HasSuffix(line, "securityagent_ax_window_count=0") {
			var stamp float64
			if _, e := fmt.Sscanf(line, "at=%f", &stamp); e == nil && stamp > float64(end.UnixNano())/1e9+0.5 {
				survived = true
			}
		}
	}
	t.Logf("dialog_observation_before_cleanup appeared=%t survived_read_end_plus_500ms=%t observation_bound=onscreen_SecurityAgent_AX_windows_100ms", appeared, survived)
	if survived {
		t.Error("STOP reopen helperA: dialog survived killed child")
	}
	if bytes.Contains(observed, []byte("observer=unavailable")) {
		t.Error("STOP: observation failed; disposition UNKNOWN")
	}
}

type sentinelNoNetwork struct{ calls *int }

func (s sentinelNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	*s.calls++
	return nil, fmt.Errorf("sentinel network forbidden")
}
