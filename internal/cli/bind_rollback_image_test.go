package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// rollbackOlderImageEnv names the older a2a image the rollback test runs. It
// is a path to a binary built from an older source commit; the test never
// looks for an installed image itself.
const rollbackOlderImageEnv = "BIND_ROLLBACK_OLDER_IMAGE"

// TestBindRollbackToOlderImage runs a real older a2a image, one that drops
// unknown State keys, against an identity this version has migrated, and then
// returns to this version through the documented repair. It follows five
// messages through the whole path and counts every time each is printed by an
// inbox command of either image.
//
// Claims:
//   - the older image takes over the migrated identity and delivers: it exits
//     successfully, prints the mail that arrived while it is in charge, and
//     its State write removes the marker;
//   - this version then refuses the identity without printing or changing
//     anything (control for the repair);
//   - after the sidecar is removed this version delivers again and the next
//     bind migrates afresh from the older image's cursor;
//   - no message is printed twice by the two images together, including the
//     one the older image fetches again because State's cursor was stale.
//
// Limits: the older image is driven through its inbox command, not as a
// detached consumer in a terminal; both images share one session id, as one
// restarted session does. The test is skipped unless BIND_ROLLBACK_OLDER_IMAGE
// names the image, and proves nothing when skipped.
func TestBindRollbackToOlderImage(t *testing.T) {
	image := os.Getenv(rollbackOlderImageEnv)
	if image == "" {
		t.Skip("needs an older a2a image built from source; set " + rollbackOlderImageEnv)
	}
	e, room := world(t)
	e.extra[EnvSessionID] = "rollback-fixture"
	home := t.TempDir()
	older := func(args ...string) result {
		t.Helper()
		cmd := exec.Command(image, append([]string{"--as", "alice"}, args...)...)
		cmd.Env = []string{
			config.EnvConfigDir + "=" + e.dir, EnvSessionID + "=rollback-fixture", "A2A_SECRET_STORE=file",
			"HOME=" + home, "TMPDIR=" + home, "PATH=/usr/bin:/bin",
		}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		r := result{}
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("start the older image: %v", err)
			}
			r.code = exit.ExitCode()
		}
		r.stdout, r.stderr = out.String(), errb.String()
		e.assertNoLeak(r)
		return r
	}
	post := func(text string) string {
		t.Helper()
		return e.hs.inject(room, "@bob:test.local", msg("@alice:test.local "+text, nil))
	}
	printed := map[string]int{}
	var ids []string
	inbox := func(who string, r result) result {
		t.Helper()
		for _, id := range ids {
			printed[id] += strings.Count(r.stdout, id)
		}
		t.Logf("%s inbox: exit=%d printed=%v stderr=%q", who, r.code, printed, strings.TrimSpace(r.stderr))
		return r
	}
	marked := func() bool {
		t.Helper()
		raw, err := os.ReadFile(e.paths().StateFile("alice"))
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Contains(raw, []byte(`"bind_part"`))
	}

	// This version takes the position, migrates the identity and delivers m1.
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	state := e.state("alice")
	if state.Lease == nil {
		t.Fatal("setup: no delivery lease to migrate")
	}
	if migrated, err := config.EnsureBindPart(e.paths(), "alice", config.BindPartOpenOptions{Holder: state.Lease.Holder, Now: time.Now()}); err != nil || !config.BindPartActive(migrated) {
		t.Fatalf("migrate the identity: active=%t err=%v", config.BindPartActive(migrated), err)
	}
	m1 := post("one, before the rollback")
	ids = append(ids, m1)
	inbox("this version", e.as("alice", "inbox")).ok(t)
	if printed[m1] != 1 || !marked() {
		t.Fatalf("setup: m1 printed %d times, marked=%t", printed[m1], marked())
	}

	// The older image takes over. State's cursor is the one of the migration,
	// so it fetches m1 again together with m2.
	m2 := post("two, for the older image")
	ids = append(ids, m2)
	if r := inbox("older image", older("inbox")); r.code != 0 {
		t.Fatalf("older image on the migrated identity: exit %d, stderr=%q", r.code, r.stderr)
	}
	// An image from before unknown State keys were preserved drops the marker;
	// a later one keeps it. The refusal and the repair differ accordingly.
	kept := marked()
	t.Logf("older image kept the marker: %t", kept)
	m3 := post("three, still the older image")
	ids = append(ids, m3)
	if r := inbox("older image", older("inbox")); r.code != 0 {
		t.Fatalf("older image, second delivery: exit %d, stderr=%q", r.code, r.stderr)
	}

	// Back to this version: refused until the repair, and nothing is printed.
	m4 := post("four, after the return")
	ids = append(ids, m4)
	stateBefore, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	refused := inbox("this version, before the repair", e.as("alice", "inbox"))
	want := config.ErrBindPartUnmarked
	if kept {
		want = config.ErrBindPartInvalid
	}
	if refused.code == 0 || !strings.Contains(refused.stderr, want.Error()) || printed[m4] != 0 {
		t.Fatalf("control: this version before the repair: exit=%d stderr=%q m4 printed %d times", refused.code, refused.stderr, printed[m4])
	}
	if stateAfter, err := os.ReadFile(e.paths().StateFile("alice")); err != nil || !bytes.Equal(stateAfter, stateBefore) {
		t.Fatalf("refusal changed State (err=%v)", err)
	}

	// The repair: remove the sidecar. This version delivers again, and the
	// next bind migrates afresh from the older image's cursor.
	if err := os.Remove(e.paths().BindPartFile("alice")); err != nil {
		t.Fatal(err)
	}
	if kept {
		// Second step for a marker-keeping image: delete the one key from State.
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(stateBefore, &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, "bind_part")
		edited, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.paths().StateFile("alice"), append(edited, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inbox("this version, after the repair", e.as("alice", "inbox")).ok(t)
	resumed := e.state("alice")
	if resumed.Lease == nil {
		t.Fatal("no delivery lease after the repair")
	}
	migrated, err := config.EnsureBindPart(e.paths(), "alice", config.BindPartOpenOptions{Holder: resumed.Lease.Holder, Now: time.Now()})
	if err != nil || !config.BindPartActive(migrated) {
		t.Fatalf("bind after the repair: active=%t err=%v", config.BindPartActive(migrated), err)
	}
	if view, err := config.ReadBindPartView(e.paths(), "alice", migrated); err != nil || view.NextBatch != resumed.NextBatch || view.NextBatch == "" {
		t.Fatalf("re-migrated cursor=%q, State cursor=%q, err=%v", view.NextBatch, resumed.NextBatch, err)
	}
	m5 := post("five, migrated again")
	ids = append(ids, m5)
	inbox("this version, migrated again", e.as("alice", "inbox")).ok(t)

	for i, id := range ids {
		if printed[id] != 1 {
			t.Errorf("message %d was printed %d times across both images, want exactly once", i+1, printed[id])
		}
	}
}

// TestBindOlderImageStopsABinding runs a real older a2a image against a
// migrated identity that has an active binding, and lets it change one of the
// fields the sidecar owns other than the cursor: the older image's unbind
// writes the binding's stopped flag into State. It follows three messages and
// counts every print.
//
// Claims:
//   - the older image's unbind succeeds and State then says "stopped" where
//     the sidecar says "active" (the premise, read from the raw file);
//   - this version refuses the identity: the inbox prints nothing, and
//     neither State nor the sidecar changes, so the sidecar's generation and
//     its "active" are exactly what they were;
//   - after the documented repair this version delivers again, the binding is
//     the one the older image left (same generation, stopped), and the next
//     migration takes exactly that into the new sidecar;
//   - no message is lost and none is printed twice.
//
// Limits: only the stopped flag is moved, and not alone: the older image's
// unbind also renews its lease in State, so this test does not attribute the
// refusal to the stopped flag. The library test
// TestBindPartMarkerKeepingLegacyWriterMovesABindingField moves each field
// alone. An older image changes the generation or the session only through
// bind or the session-start hook, which need a terminal and start a consumer
// process; this test drives neither. The binding is a State record made by the fixture, with no
// consumer process and no daemon to signal. The test is skipped unless
// BIND_ROLLBACK_OLDER_IMAGE names the image, and proves nothing when skipped.
func TestBindOlderImageStopsABinding(t *testing.T) {
	image := os.Getenv(rollbackOlderImageEnv)
	if image == "" {
		t.Skip("needs an older a2a image built from source; set " + rollbackOlderImageEnv)
	}
	isolateBindTest(t)
	e, room := world(t)
	e.extra[EnvSessionID] = "rollback-fixture"
	home := t.TempDir()
	older := func(args ...string) result {
		t.Helper()
		cmd := exec.Command(image, append([]string{"--as", "alice"}, args...)...)
		cmd.Env = []string{
			config.EnvConfigDir + "=" + e.dir, EnvSessionID + "=rollback-fixture", "A2A_SECRET_STORE=file",
			"HOME=" + home, "TMPDIR=" + home, "PATH=/usr/bin:/bin",
		}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		r := result{}
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("start the older image: %v", err)
			}
			r.code = exit.ExitCode()
		}
		r.stdout, r.stderr = out.String(), errb.String()
		e.assertNoLeak(r)
		return r
	}
	printed := map[string]int{}
	var ids []string
	post := func(text string) string {
		t.Helper()
		id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local "+text, nil))
		ids = append(ids, id)
		return id
	}
	inbox := func(who string) result {
		t.Helper()
		r := e.as("alice", "inbox")
		for _, id := range ids {
			printed[id] += strings.Count(r.stdout, id)
		}
		t.Logf("%s inbox: exit=%d printed=%v stderr=%q", who, r.code, printed, strings.TrimSpace(r.stderr))
		return r
	}
	raw := func() (state []byte, marked, stopped bool) {
		t.Helper()
		state, err := os.ReadFile(e.paths().StateFile("alice"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Marker   json.RawMessage `json:"bind_part"`
			Terminal *struct {
				Stopped bool `json:"stopped"`
			} `json:"terminal"`
		}
		if err := json.Unmarshal(state, &doc); err != nil {
			t.Fatal(err)
		}
		return state, len(doc.Marker) != 0, doc.Terminal != nil && doc.Terminal.Stopped
	}

	// This version takes the position, records an active binding of this
	// session, migrates the identity and delivers m1.
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	target := shortBindIdleTarget(t, "codex")
	generation := strings.Repeat("a", 32)
	if _, err := config.UpdateState(e.paths(), "alice", func(state *config.State) error {
		state.Terminal = &config.TerminalBinding{
			Generation: generation, SessionID: "rollback-fixture", Host: machineLabel(),
			OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target,
			Outcome: "armed: no new mail", Interval: time.Second, RetryMax: 2 * time.Second,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	state := e.state("alice")
	if state.Lease == nil {
		t.Fatal("setup: no delivery lease to migrate")
	}
	migrated, err := config.EnsureBindPart(e.paths(), "alice", config.BindPartOpenOptions{Holder: state.Lease.Holder, Now: time.Now()})
	if err != nil || !config.BindPartActive(migrated) {
		t.Fatalf("migrate the identity: active=%t err=%v", config.BindPartActive(migrated), err)
	}
	if view, err := config.ReadBindPartView(e.paths(), "alice", migrated); err != nil || view.Generation != generation || view.Stopped {
		t.Fatalf("setup: the sidecar's binding: %+v err=%v", view, err)
	}
	m1 := post("one, before the older image")
	inbox("this version").ok(t)
	if _, marked, stopped := raw(); printed[m1] != 1 || !marked || stopped {
		t.Fatalf("setup: m1 printed %d times, marked=%t stopped=%t", printed[m1], marked, stopped)
	}

	// The older image stops the binding. It knows State only.
	if r := older("unbind"); r.code != 0 {
		t.Fatalf("older image's unbind on the migrated identity: exit %d, stderr=%q", r.code, r.stderr)
	}
	stateBefore, kept, stopped := raw()
	if !stopped {
		t.Fatal("premise: the older image's unbind did not write the stopped flag into State")
	}
	t.Logf("older image kept the marker: %t", kept)
	sidecarBefore, err := os.ReadFile(e.paths().BindPartFile("alice"))
	if err != nil {
		t.Fatal(err)
	}

	// Back to this version: refused, nothing printed, nothing changed.
	m2 := post("two, after the older image stopped the binding")
	refused := inbox("this version, before the repair")
	want := config.ErrBindPartUnmarked
	if kept {
		want = config.ErrBindPartInvalid
	}
	if refused.code == 0 || !strings.Contains(refused.stderr, want.Error()) || printed[m2] != 0 {
		t.Fatalf("this version before the repair: exit=%d stderr=%q m2 printed %d times", refused.code, refused.stderr, printed[m2])
	}
	if stateAfter, _, _ := raw(); !bytes.Equal(stateAfter, stateBefore) {
		t.Fatal("the refusal changed State")
	}
	if sidecarAfter, err := os.ReadFile(e.paths().BindPartFile("alice")); err != nil || !bytes.Equal(sidecarAfter, sidecarBefore) {
		t.Fatalf("the refusal changed the sidecar (err=%v)", err)
	}

	// The documented repair, then this version again.
	if err := os.Remove(e.paths().BindPartFile("alice")); err != nil {
		t.Fatal(err)
	}
	if kept {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(stateBefore, &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, "bind_part")
		edited, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.paths().StateFile("alice"), append(edited, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inbox("this version, after the repair").ok(t)
	resumed := e.state("alice")
	if resumed.Terminal == nil || resumed.Terminal.Generation != generation || !resumed.Terminal.Stopped {
		t.Fatalf("the binding after the repair is not the one the older image left: %+v", resumed.Terminal)
	}
	if resumed.Lease == nil {
		t.Fatal("no delivery lease after the repair")
	}
	again, err := config.EnsureBindPart(e.paths(), "alice", config.BindPartOpenOptions{Holder: resumed.Lease.Holder, Now: time.Now()})
	if err != nil || !config.BindPartActive(again) {
		t.Fatalf("migration after the repair: active=%t err=%v", config.BindPartActive(again), err)
	}
	if view, err := config.ReadBindPartView(e.paths(), "alice", again); err != nil || view.Generation != generation || !view.Stopped {
		t.Fatalf("the new sidecar's binding: %+v err=%v, want generation %s stopped", view, err, generation)
	}
	post("three, migrated again")
	inbox("this version, migrated again").ok(t)
	for i, id := range ids {
		if printed[id] != 1 {
			t.Errorf("message %d was printed %d times, want exactly once", i+1, printed[id])
		}
	}
}
