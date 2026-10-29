//go:build darwin && cgo

package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The phase-2 platform sentinel (#138) runs the real in-process backend
// against one throwaway keychain file under one separately granted allowance:
// A2A_P2_SENTINEL_ALLOWANCE names the a2a event that granted it, and the
// exclusive attempt directory consumes it even when setup fails.
//
// With this test binary as the creating application it proves: add, find,
// update and readback succeed without UI; the new item's ACL names the
// creating binary and not /usr/bin/security; a duplicate add and a missing item
// keep their own errors; a lookup of an item another application created (the
// security tool, default ACL) and a lookup in a locked keychain both return
// errKeychainInteraction within the deadline; and no new SecurityAgent window
// appears while the backend runs.
//
// Limits: the test binary is ad-hoc signed, so this proves creator binding, not
// the signed a2a designated requirement, which P3 checks on the installed pair.
// The observer sees on-screen SecurityAgent windows only at its poll interval.
// An update of the foreign item is recorded, not asserted. The user search list
// and default keychain are compared before and after, with no restoration
// writes; the security tool is used only for fixture setup and cleanup.
func TestKeychainPhase2PlatformSentinel(t *testing.T) {
	allowance := os.Getenv("A2A_P2_SENTINEL_ALLOWANCE")
	if allowance == "" {
		t.Skip("one separately granted platform sentinel only; never ordinary CI")
	}
	if !regexp.MustCompile(`^\$[A-Za-z0-9_-]{20,64}$`).MatchString(allowance) {
		t.Fatal("allowance must name the granting a2a event; ZERO setup operations")
	}
	observer := os.Getenv("A2A_P2_SENTINEL_OBSERVER")
	if !filepath.IsAbs(observer) {
		t.Fatal("observer must be an absolute path to an accessibility-trusted build; ZERO setup operations")
	}
	parent, err := filepath.Abs("../../.temp/BUG-138-p2-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, strings.TrimPrefix(allowance, "$"))
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal("allowance already spent or attempt directory unavailable; NO RETRY", err)
	}
	fixture := filepath.Join(root, "throwaway.keychain-db")
	if !regexp.MustCompile(`^[A-Za-z0-9/._-]+$`).MatchString(fixture) {
		t.Fatal("fixture path would need quoting; ZERO setup operations")
	}

	// Observation first: without it no Keychain operation runs.
	o, err := startSentinelObserver(observer, filepath.Join(root, "observations.log"))
	if err != nil {
		t.Fatal("observer unavailable; ZERO keychain operations", err)
	}
	stopped := false
	stopObserver := func() {
		if !stopped {
			stopped = true
			if err := o.stop(); err != nil {
				t.Error("observer stop failed; dialog disposition UNKNOWN", err)
			}
		}
	}
	defer stopObserver()
	if err := waitObserverReady(context.Background(), o.log.Name()); err != nil {
		t.Fatal("observer not ready; ZERO keychain operations", err)
	}

	// Fixture setup and cleanup only. Input goes on stdin; argv carries no secret.
	tool := func(input string, args ...string) (string, int) {
		ctx, cancel := context.WithTimeout(context.Background(), securityDeadline)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		cmd.Stdin = strings.NewReader(input)
		var out, stderr limitedSecretBuffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		code := 0
		if err := runBoundSecurityProcess(cmd); err != nil {
			code = -1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		t.Logf("setup utc=%s argv=%q stdin_present=%t exit=%d", time.Now().UTC().Format(time.RFC3339Nano), args, input != "", code)
		return out.String(), code
	}
	before, code := tool("", "list-keychains", "-d", "user")
	if code != 0 {
		t.Fatal("before list failed; no fixture created")
	}
	defaultBefore, code := tool("", "default-keychain", "-d", "user")
	if code != 0 {
		t.Fatal("before default failed; no fixture created")
	}
	fixtureArmed := false
	defer func() {
		if fixtureArmed {
			if _, code := tool("", "delete-keychain", fixture); code != 0 {
				t.Error("fixture-only cleanup failed")
			}
		}
		after, code := tool("", "list-keychains", "-d", "user")
		if code != 0 || after != before {
			t.Error("search list not original after cleanup; NO RESTORATION WRITES")
		}
		defaultAfter, code := tool("", "default-keychain", "-d", "user")
		if code != 0 || defaultAfter != defaultBefore {
			t.Error("default keychain changed; NO RESTORATION WRITES")
		}
		if _, err := os.Stat(fixture); !os.IsNotExist(err) {
			t.Error("fixture still exists after cleanup")
		}
		t.Logf("cleanup search_list_original=%t default_original=%t", after == before, defaultAfter == defaultBefore)
	}()
	random := func() string {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(b[:])
	}
	// Arm fixture-only cleanup before creation: a timeout could still create it.
	fixtureArmed = true
	if _, code := tool("create-keychain -p "+random()+" "+fixture+"\n", "-i"); code != 0 {
		t.Fatal("fixture setup failed; NO RETRY")
	}
	if afterCreate, code := tool("", "list-keychains", "-d", "user"); code != 0 || afterCreate != before {
		t.Fatal("STOP: create changed the search list; no backend call ran; fixture deleted only")
	}
	const service = "works.relux.a2a.sentinel"
	foreign := "@foreign:sentinel.invalid"
	if _, code := tool("add-generic-password -s "+service+" -a "+foreign+" -w "+random()+" "+fixture+"\n", "-i"); code != 0 {
		t.Fatal("foreign item setup failed; NO RETRY")
	}

	b := secBackend{}
	first := time.Now()
	call := func(name string, fn func(context.Context) error) error {
		ctx, cancel := context.WithTimeout(context.Background(), securityDeadline)
		defer cancel()
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- fn(ctx) }()
		select {
		case err := <-done:
			t.Logf("backend op=%s elapsed=%s result=%v", name, time.Since(start), err)
			return err
		case <-ctx.Done():
			t.Logf("backend op=%s TIMEOUT after %s; a blocked call suggests UI", name, time.Since(start))
			return ctx.Err()
		}
	}
	find := func(account string) (string, error) {
		var got []byte
		err := call("find "+account, func(ctx context.Context) error {
			var err error
			got, err = b.find(ctx, fixture, service, account)
			return err
		})
		return string(got), err
	}
	own := "@sentinel:sentinel.invalid"
	tok1, tok2 := random(), random()
	if err := call("add", func(ctx context.Context) error { return b.add(ctx, fixture, service, own, []byte(tok1)) }); err != nil {
		t.Fatal("creator add failed", err)
	}
	if got, err := find(own); err != nil || got != tok1 {
		t.Fatal("creator readback failed", err)
	}
	if err := call("update", func(ctx context.Context) error { return b.update(ctx, fixture, service, own, []byte(tok2)) }); err != nil {
		t.Fatal("creator update failed", err)
	}
	if got, err := find(own); err != nil || got != tok2 {
		t.Fatal("creator readback after update failed", err)
	}
	// Issue #138 structural check, ACL metadata only (never -d): the item this
	// binary created names the creating binary and no generic reader. The full
	// dump stays in the attempt directory for inspection.
	dump, code := tool("", "dump-keychain", "-a", fixture)
	if code != 0 {
		t.Fatal("ACL dump failed; no further backend calls")
	}
	_ = os.WriteFile(filepath.Join(root, "acl-dump.txt"), []byte(dump), 0o600)
	ownBlock := ""
	for _, block := range strings.Split(dump, "keychain: ") {
		if strings.Contains(block, `"acct"<blob>="`+own+`"`) {
			ownBlock = block
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	namesCreator := strings.Contains(ownBlock, exe) || strings.Contains(ownBlock, resolved)
	t.Logf("structural own_item_found=%t names_creator=%t trusts_security_tool=%t", ownBlock != "", namesCreator, strings.Contains(ownBlock, "/usr/bin/security"))
	switch {
	case ownBlock == "":
		t.Error("own item missing from the ACL dump")
	case strings.Contains(ownBlock, "/usr/bin/security"):
		t.Error("own item ACL trusts /usr/bin/security")
	case !namesCreator:
		t.Error("own item ACL does not name the creating binary; see acl-dump.txt")
	}
	if err := call("duplicate add", func(ctx context.Context) error { return b.add(ctx, fixture, service, own, []byte(tok1)) }); !errors.Is(err, errKeychainDuplicate) {
		t.Error("duplicate add did not report a duplicate", err)
	}
	if _, err := find("@absent:sentinel.invalid"); !errors.Is(err, errKeychainMissing) {
		t.Error("absent item did not report missing", err)
	}
	if _, err := find(foreign); !errors.Is(err, errKeychainInteraction) {
		t.Error("foreign item lookup was not refused without interaction", err)
	}
	foreignUpdate := call("foreign update", func(ctx context.Context) error {
		return b.update(ctx, fixture, service, foreign, []byte(random()))
	})
	t.Logf("observation foreign_update_result=%v", foreignUpdate)
	if _, code := tool("", "lock-keychain", fixture); code != 0 {
		t.Fatal("fixture lock failed; NO RETRY")
	}
	if _, err := find(own); !errors.Is(err, errKeychainInteraction) {
		t.Error("locked lookup was not refused without interaction", err)
	}
	last := time.Now()

	// Observe one more second, then require coverage past the last call and no
	// new SecurityAgent window in the whole backend interval.
	time.Sleep(time.Second)
	stopObserver()
	data, err := os.ReadFile(filepath.Join(root, "observations.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "observer=unavailable") && !strings.Contains(string(data), "observer=unavailable lifetime_expired") {
		t.Error("observation failed; dialog disposition UNKNOWN")
	}
	covered, dialog := false, false
	scan := bufio.NewScanner(strings.NewReader(string(data)))
	for scan.Scan() {
		var at float64
		var baseline, onscreen, fresh, ax, old int
		line := scan.Text()
		if _, err := fmt.Sscanf(line, "at=%f baseline_window_count=%d onscreen_window_count=%d new_window_count=%d ax_window_count=%d old12571_ax_windows=%d", &at, &baseline, &onscreen, &fresh, &ax, &old); err != nil {
			continue
		}
		if at < float64(first.UnixNano())/1e9 {
			continue
		}
		if fresh != 0 {
			dialog = true
		}
		if at > float64(last.UnixNano())/1e9+0.5 {
			covered = true
		}
	}
	t.Logf("observation new_securityagent_window=%t covered_last_call_plus_500ms=%t bound=onscreen_SecurityAgent_windows_at_poll_interval", dialog, covered)
	if dialog {
		t.Error("STOP: a SecurityAgent window appeared during backend calls")
	}
	if !covered {
		t.Error("observation does not cover the backend interval; dialog disposition UNKNOWN")
	}
}
