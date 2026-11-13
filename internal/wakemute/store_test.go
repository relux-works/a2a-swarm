//go:build linux || darwin

package wakemute_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

func admission(t *testing.T, root string) error {
	t.Helper()
	return wakemute.Admission(fixture.Context(t), root, ".", func(uint64) error { return nil })
}
func ledger(t *testing.T, root string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(root, wakemute.Ledger))
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// Exact-operation cleanup composes overlapping mutes, rejects stale/wrong/run
// and narrowed scopes byte-for-byte, and never claims drained remote work.
func TestScopeMuteExactCleanupAndOverlap(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	if err := admission(t, root); err != nil {
		t.Fatal(err)
	}
	a := fixture.Mute(t, scope, "first")
	b := fixture.Mute(t, scope, "second")
	for _, test := range []struct{ run, id string }{{a.Run, strings.Repeat("0", 32)}, {"foreign-run", a.ID}, {a.Run, "bad"}} {
		before := ledger(t, root)
		_, err := wakemute.Unmute(fixture.Context(t), scope, test.run, test.id)
		if err == nil || !bytes.Equal(before, ledger(t, root)) {
			t.Fatal("wrong cleanup changed ledger")
		}
	}
	fixture.Unmute(t, a)
	if !errors.Is(admission(t, root), wakemute.ErrMuted) {
		t.Fatal("removing A cleared overlapping B")
	}
	before := ledger(t, root)
	if _, err := wakemute.Unmute(fixture.Context(t), scope, a.Run, a.ID); !errors.Is(err, wakemute.ErrStale) || !bytes.Equal(before, ledger(t, root)) {
		t.Fatal("stale cleanup mutated state")
	}
	fixture.Unmute(t, b)
	if err := admission(t, root); err != nil {
		t.Fatal(err)
	}
	// A previously approved run cannot become a fresh authorization by replay.
	approved := fixture.Confirm(t, scope, "first", "mute")
	if _, err := wakemute.Mute(fixture.Context(t), approved); !errors.Is(err, wakemute.ErrStale) {
		t.Fatal("run replay admitted")
	}
}

// ACK waits for a previously admitted local effect. No new effect can enter
// after ACK, while a receiver's already-admitted asynchronous work may finish.
func TestScopeMuteAdmissionRaceNotQuiesced(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	approved := fixture.Confirm(t, scope, "race", "mute")
	admitted := make(chan struct{})
	release := make(chan struct{})
	effectDone := make(chan error, 1)
	remoteRelease := make(chan struct{})
	remoteDone := make(chan struct{})
	go func() { <-remoteRelease; close(remoteDone) }()
	go func() {
		effectDone <- wakemute.Admission(fixture.Context(t), root, ".", func(uint64) error { close(admitted); <-release; return nil })
	}()
	<-admitted
	type response struct {
		out wakemute.Result
		err error
	}
	ack := make(chan response, 1)
	go func() { out, err := wakemute.Mute(fixture.Context(t), approved); ack <- response{out, err} }()
	select {
	case <-ack:
		t.Fatal("mute ACK crossed admitted effect")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-effectDone; err != nil {
		t.Fatal(err)
	}
	result := <-ack
	if result.err != nil || result.out.Quiesced {
		t.Fatalf("mute: %v", result.err)
	}
	called := false
	err := wakemute.Admission(fixture.Context(t), root, ".", func(uint64) error { called = true; return nil })
	if !errors.Is(err, wakemute.ErrMuted) || called {
		t.Fatal("new admission after ACK")
	}
	close(remoteRelease) // Receiver finishes only after mute ACK.
	select {
	case <-remoteDone:
	case <-time.After(time.Second):
		t.Fatal("pre-admitted completion lost")
	}
	if result.out.Quiesced {
		t.Fatal("invented receiver quiescence")
	}
}

// Status is a shared-lock read: even age calculation and errors leave every
// root directory entry and file unchanged. Unsupported never means whole-host.
func TestScopeMuteStatusReadOnlyAndPulse(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	out, err := wakemute.Status(fixture.Context(t), scope)
	if !errors.Is(err, wakemute.ErrUnsupported) || out.Status != "unsupported" {
		t.Fatal("uninstalled treated as admitted")
	}
	fixture.Enroll(t, scope)
	var first, last uint64
	wakemute.Admission(fixture.Context(t), root, ".", func(g uint64) error { first = g; return nil })
	op := fixture.Mute(t, scope, "pulse")
	fixture.Unmute(t, op)
	before := ledger(t, root)
	entries, _ := os.ReadDir(root)
	out, err = wakemute.Status(fixture.Context(t), scope)
	afterEntries, _ := os.ReadDir(root)
	if err != nil || out.Quiesced || len(entries) != len(afterEntries) || !bytes.Equal(before, ledger(t, root)) {
		t.Fatal("status wrote state")
	}
	wakemute.Admission(fixture.Context(t), root, ".", func(g uint64) error { last = g; return nil })
	if last <= first {
		t.Fatal("unobserved pulse did not invalidate generation")
	}
}

// An enrolled root cannot lose control state or select a different namespace
// without failing closed. Every unsafe neighbor pairs with the valid store.
func TestScopeMuteUnsafeRecordsAndOverrides(t *testing.T) {
	for _, kind := range []string{"truncated", "symlink", "mode", "missing-ledger", "missing-marker", "replaced-root", "foreign-host", "foreign-owner", "connection", "version", "duplicate-json"} {
		t.Run(kind, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "root")
			if e := os.Mkdir(root, 0700); e != nil {
				t.Fatal(e)
			}
			scope := fixture.Scope(root)
			fixture.Enroll(t, scope)
			fixture.Mute(t, scope, "held")
			if !errors.Is(admission(t, root), wakemute.ErrMuted) {
				t.Fatal("positive mute missing")
			}
			file := filepath.Join(root, wakemute.Ledger)
			switch kind {
			case "truncated":
				os.WriteFile(file, []byte("{"), 0600)
			case "symlink":
				b := ledger(t, root)
				os.Remove(file)
				target := filepath.Join(t.TempDir(), "other")
				os.WriteFile(target, b, 0600)
				os.Symlink(target, file)
			case "mode":
				os.Chmod(file, 0644)
			case "missing-ledger":
				os.Remove(file)
			case "missing-marker":
				os.Remove(filepath.Join(root, wakemute.Marker))
			case "replaced-root":
				old := root + "-old"
				os.Rename(root, old)
				os.Mkdir(root, 0700)
				for _, n := range []string{wakemute.Marker, wakemute.Ledger} {
					b, _ := os.ReadFile(filepath.Join(old, n))
					os.WriteFile(filepath.Join(root, n), b, 0600)
				}
			case "foreign-host", "foreign-owner", "version":
				path := filepath.Join(root, wakemute.Marker)
				b, _ := os.ReadFile(path)
				var e wakemute.Enrollment
				json.Unmarshal(b, &e)
				if kind == "foreign-host" {
					e.Host = "different-host"
				}
				if kind == "foreign-owner" {
					e.Owner++
				}
				if kind == "version" {
					e.Version++
				}
				b, _ = json.Marshal(e)
				os.WriteFile(path, b, 0600)
			case "connection":
				err := wakemute.Admission(fixture.Context(t), root, "connections/other", func(uint64) error { t.Fatal("uncovered namespace admitted"); return nil })
				if err == nil {
					t.Fatal("connection override succeeded")
				}
				return
			case "duplicate-json":
				b := ledger(t, root)
				b = bytes.Replace(b, []byte(`"version":1`), []byte(`"version":0,"version":1`), 1)
				os.WriteFile(file, b, 0600)
			}
			called := false
			err := wakemute.Admission(fixture.Context(t), root, ".", func(uint64) error { called = true; return nil })
			if !errors.Is(err, wakemute.ErrUnsafe) || called {
				t.Fatalf("unsafe record did not refuse: %v", err)
			}
		})
	}
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	nested := filepath.Join(root, "connections", "other")
	os.MkdirAll(nested, 0700)
	if err := admission(t, nested); err == nil {
		t.Fatal("nested config-root override hid enrollment")
	}
}

// Lock contention has a finite backoff budget and makes no ledger write.
func TestScopeMuteLockBudget(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- wakemute.Admission(fixture.Context(t), root, ".", func(uint64) error { close(entered); <-release; return nil })
	}()
	<-entered
	op := fixture.Confirm(t, scope, "budget", "mute")
	before := ledger(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := wakemute.Mute(ctx, op)
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second || !bytes.Equal(before, ledger(t, root)) {
		t.Fatal("lock budget or state preservation failed")
	}
}

// Namespace selection is explicit: a muted connection does not claim another
// connection, and changing the recorded cleanup list is refused without writes.
func TestScopeMuteConnectionSelectionAndUndoScope(t *testing.T) {
	root := t.TempDir()
	all := wakemute.Scope{{Root: root, Connections: []wakemute.Connection{{Path: "."}, {Name: "one", Path: "connections/one"}}}}
	fixture.Enroll(t, all)
	one := wakemute.Scope{{Root: root, Connections: []wakemute.Connection{{Name: "one", Path: "connections/one"}}}}
	op := fixture.Mute(t, one, "named")
	if err := admission(t, root); err != nil {
		t.Fatal("legacy connection was broadened")
	}
	if e := wakemute.Admission(fixture.Context(t), root, "connections/one", func(uint64) error { return nil }); !errors.Is(e, wakemute.ErrMuted) {
		t.Fatal("named connection not gated")
	}
	before := ledger(t, root)
	_, err := wakemute.Unmute(fixture.Context(t), all, op.Run, op.ID)
	if !errors.Is(err, wakemute.ErrAuthority) || !reflect.DeepEqual(before, ledger(t, root)) {
		t.Fatal("scope override cleanup admitted")
	}
	fixture.Unmute(t, op)
}

// The admitted launch requirement survives loss of all root control files and
// process restart. Legacy absence is explicitly outside admitted coverage; it
// cannot substitute for version-1 required participation or an unknown version.
func TestScopeMuteRequiredAdmissionLossAndOverride(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	called := false
	effect := func(uint64) error { called = true; return nil }
	if err := wakemute.AdmissionRequired(fixture.Context(t), root, ".", 1, effect); err != nil || !called {
		t.Fatal("required positive refused")
	}
	fixture.Mute(t, scope, "retained")
	os.Remove(filepath.Join(root, wakemute.Marker))
	os.Remove(filepath.Join(root, wakemute.Ledger))
	called = false
	if err := wakemute.AdmissionRequired(fixture.Context(t), root, ".", 1, effect); !errors.Is(err, wakemute.ErrUnsafe) || called {
		t.Fatal("lost admitted control silently enabled a wake")
	}
	if err := wakemute.AdmissionRequired(fixture.Context(t), t.TempDir(), ".", 1, effect); !errors.Is(err, wakemute.ErrUnsafe) || called {
		t.Fatal("config-root override bypassed required control")
	}
	if err := wakemute.AdmissionRequired(fixture.Context(t), root, ".", 2, effect); !errors.Is(err, wakemute.ErrUnsupported) || called {
		t.Fatal("unknown consumer version admitted")
	}
}

// Production pipe backpressure is bounded by the explicit output deadline;
// the helper returns the actual partial count and never reports full success.
func TestScopeMuteOutputBudget(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	n, err := wakemute.WriteOutput(ctx, w, make([]byte, 1<<20))
	if (!errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded)) || n >= 1<<20 || time.Since(start) > time.Second {
		t.Fatal("output budget failed")
	}
}

// The mute layer touches only its own two control files. Existing fixture
// credential, principal/filter, pause, lease, manual and UNKNOWN bytes survive
// both overlapping operations, including an independent re-pause during undo.
func TestScopeMutePreservesIndependentStateBytes(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	paths := []string{"agents/member.json", "humans/operator.json", "owner-dm/member.principals.json", "state/member.json", "state/member.mailbox.jsonl", "subscriptions.json"}
	data := []byte(`{"hooks_paused":true,"manual_gate":"waiting-human","lease":"retained","attempt":"UNKNOWN","subscriptions":["principal","filtered"]}`)
	for _, p := range paths {
		full := filepath.Join(root, p)
		if e := os.MkdirAll(filepath.Dir(full), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(full, data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	a := fixture.Mute(t, scope, "preserve-A")
	b := fixture.Mute(t, scope, "preserve-B")
	repaused := false
	_, err := wakemute.FaultUnmuteForTest(fixture.Context(t), scope, a.Run, a.ID, func(point string) error {
		if point == "before-rename" && !repaused {
			repaused = true
			return os.WriteFile(filepath.Join(root, "state/member.json"), append(bytes.Clone(data), '\n'), 0600)
		}
		return nil
	})
	if err != nil || !repaused {
		t.Fatal("independent re-pause interleave missing")
	}
	fixture.Unmute(t, b)
	for _, p := range paths {
		want := data
		if p == "state/member.json" {
			want = append(bytes.Clone(data), '\n')
		}
		got, e := os.ReadFile(filepath.Join(root, p))
		if e != nil || !bytes.Equal(want, got) {
			t.Fatalf("independent state changed: %s", p)
		}
	}
}

// One unavailable selected root cannot hide the durable run on its readable
// coordinator. Status shows exact covered/not-covered rows without repair;
// restoring the fixture root permits the originally authorized cleanup.
func TestScopeMuteStatusUnavailableRootDiscovery(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	os.Mkdir(a, 0700)
	os.Mkdir(b, 0700)
	scope := append(fixture.Scope(a), fixture.Scope(b)...)
	fixture.Enroll(t, scope)
	operation := fixture.Mute(t, scope, "neighbor-unavailable")
	if err := os.Rename(b, b+"-offline"); err != nil {
		t.Fatal(err)
	}
	before := ledger(t, a)
	result, err := wakemute.Status(fixture.Context(t), scope)
	if !errors.Is(err, wakemute.ErrUnsafe) || result.Status != "uncertain" || len(result.Roots) != 2 || len(result.Operations) != 1 || len(result.Operations[0].NotCovered) != 1 || result.Operations[0].NotCovered[0].Root != b || result.Operations[0].ID != operation.ID {
		t.Fatal("unavailable neighbor hid recoverable operation")
	}
	if !bytes.Equal(before, ledger(t, a)) {
		t.Fatal("status repaired readable root")
	}
	if err = os.Rename(b+"-offline", b); err != nil {
		t.Fatal(err)
	}
	fixture.Unmute(t, operation)
}

// Explicit selection refuses path traversal, duplicates, aliases, overlapping
// roots and oversized finite lists; valid neighboring selections remain usable.
func TestScopeMuteSelectionBoundaries(t *testing.T) {
	root := t.TempDir()
	base := fixture.Scope(filepath.Join(root, "a"))
	cases := []wakemute.Scope{
		nil,
		{{Root: "relative", Connections: []wakemute.Connection{{Path: "."}}}},
		append(append(wakemute.Scope{}, base...), base...),
		{{Root: filepath.Join(root, "a"), Connections: []wakemute.Connection{{Path: "."}}}, {Root: filepath.Join(root, "a-b"), Connections: []wakemute.Connection{{Path: "."}}}, {Root: filepath.Join(root, "a", "child"), Connections: []wakemute.Connection{{Path: "."}}}},
		{{Root: root}},
		{{Root: root, Connections: []wakemute.Connection{{Name: "one", Path: "connections/../escape"}}}},
		{{Root: root, Connections: []wakemute.Connection{{Name: "one", Path: "connections/id"}, {Name: "two", Path: "connections/id"}}}},
	}
	for i, scope := range cases {
		if _, err := wakemute.Normalize(scope); !errors.Is(err, wakemute.ErrAuthority) {
			t.Fatalf("invalid scope %d accepted", i)
		}
	}
	if out, err := wakemute.Normalize(base); err != nil || len(out) != 1 {
		t.Fatal("valid adjacent scope refused")
	}
}

// A syntactically valid empty replacement of either control copy must not
// erase a mute. Read-only status retains the original run and exact cleanup
// reconciles both snapshots while preserving an overlapping operation.
func TestScopeMuteReplacedLedgerRetainsRecovery(t *testing.T) {
	for _, replace := range []string{"ledger", "snapshot"} {
		t.Run(replace, func(t *testing.T) {
			root := t.TempDir()
			scope := fixture.Scope(root)
			fixture.Enroll(t, scope)
			a := fixture.Mute(t, scope, "replace-A")
			b := fixture.Mute(t, scope, "replace-B")
			empty := wakemute.Store{Version: 1, Operations: []wakemute.Operation{}}
			if replace == "ledger" {
				data, _ := json.Marshal(empty)
				if err := os.WriteFile(filepath.Join(root, wakemute.Ledger), data, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				file := filepath.Join(root, wakemute.Marker)
				data, _ := os.ReadFile(file)
				var enrolled wakemute.Enrollment
				if err := json.Unmarshal(data, &enrolled); err != nil {
					t.Fatal(err)
				}
				enrolled.Snapshot = &empty
				data, _ = json.Marshal(enrolled)
				if err := os.WriteFile(file, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err := wakemute.AdmissionRequired(fixture.Context(t), root, ".", 1, func(uint64) error { called = true; return nil })
			if !errors.Is(err, wakemute.ErrUncertain) || called {
				t.Fatal("valid-shaped replacement enabled wake")
			}
			before := ledger(t, root)
			status, err := wakemute.Status(fixture.Context(t), scope)
			if err == nil || status.Status != "uncertain" || len(status.Roots[0].Operations) != 2 || !bytes.Equal(before, ledger(t, root)) {
				t.Fatal("replacement hid recovery or status repaired it")
			}
			fixture.Unmute(t, a)
			if !errors.Is(admission(t, root), wakemute.ErrMuted) {
				t.Fatal("recovery cleared overlapping B")
			}
			fixture.Unmute(t, b)
			if err = admission(t, root); err != nil {
				t.Fatal("exact recovery did not restore admission")
			}
		})
	}
}

// A real fixture TTY with no reader applies backpressure to result output.
// Chunked polling preserves a real partial count and respects the finite budget.
func TestScopeMuteTTYOutputBudget(t *testing.T) {
	_, slave := fixture.PTY(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	n, err := wakemute.WriteOutput(ctx, slave, bytes.Repeat([]byte("x"), 1<<20))
	if (!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded)) || n <= 0 || n >= 1<<20 || time.Since(start) > time.Second {
		t.Fatalf("TTY output budget: n=%d err=%v", n, err)
	}
}
