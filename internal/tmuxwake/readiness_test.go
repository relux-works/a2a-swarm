package tmuxwake

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

type boundFixture struct {
	f      *terminalFixture
	b      BoundTerminal
	m      map[string]any
	record string
}

// Trusted association needs the exact live owner/birth/pane/session record,
// but does not mistake busy or an unknown version for lost root provenance.
// The detached assessment consumes only the explicitly carried root.
func TestRegistryRootAssociation(t *testing.T) {
	for _, mode := range []string{"valid", "busy", "unknown-version", "session", "birth", "pane", "relative", "codex", "canceled", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newBoundFixture(t, "claude-named", "2.1.310", "idle")
			root := f.b.ClaudeRegistryRoot
			target := f.b.Target
			session := f.b.SessionID
			ctx := context.Background()
			switch mode {
			case "busy":
				f.m["status"] = "busy"
			case "unknown-version":
				f.m["version"] = "2.1.311"
			case "session":
				session = "replacement"
			case "birth":
				f.m["procStart"] = "Sun Oct 25 12:00:00 2026"
			case "pane":
				f.m["tmux"] = "synthetic:@0.%99999"
			case "relative":
				root = "relative"
			case "codex":
				target.Runtime = "codex"
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "symlink":
				link := filepath.Join(t.TempDir(), "root")
				if err := os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				root = link
			}
			f.write(t)
			got := AssociateRegistryRoot(ctx, target, session, root)
			want := mode == "valid" || mode == "busy" || mode == "unknown-version" || mode == "symlink"
			canonical, err := filepath.EvalSymlinks(f.b.ClaudeRegistryRoot)
			if err != nil {
				t.Fatal(err)
			}
			if (got != "") != want || (want && got != canonical) {
				t.Fatalf("association=%q mode=%s", got, mode)
			}
			f.empty(t)
		})
	}
}

// Runtime-ready alone does not admit a draft or an unknown parser state.
// The same valid key with actual empty input is the positive neighbor.
func TestReadinessAdmissionState(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		for _, state := range []InputState{InputEmpty, InputNonempty, InputUnavailable} {
			t.Run(runtime+"/"+string(state), func(t *testing.T) {
				k := ReadinessKey{Route: LegacyCodex, Target: Target{Runtime: runtime, OwnerPID: 123, OwnerBirth: "birth", Pane: "%0", SocketID: "socket"}, Generation: "g", SessionID: "s", ComposerRow: 0, ComposerColumn: 2, PaneWidth: 120}
				if runtime == "claude" {
					k.Route = RegistryNamed
					k.ClaudeRegistryRoot = "/synthetic"
					k.RegistrySessionID = "s"
					k.RegistryVersion = "2.1.310"
					k.RegistryStatus = "idle"
				}
				r := ReadinessAssessment{Runtime: runtimeResult(RuntimeNone, ""), Input: InputAssessment{state, InputReasonNone}, Key: k}
				if r.Eligible() != (state == InputEmpty) {
					t.Fatal("runtime alone admitted input state", state)
				}
			})
		}
	}
}

// The saved notice restores nonempty diagnostics for a filled no-hint Codex
// composer. Neither its returned key nor a prior empty key authorizes Send;
// the existing empty layout remains the positive neighbor.
func TestReadinessNoticeDiagnosticOnly(t *testing.T) {
	f := newBoundFixture(t, "codex", "", "")
	empty := f.b.AssessReadiness(context.Background())
	if !empty.Eligible() {
		t.Fatal("valid empty setup refused")
	}
	f.f.typeText(Notice(1))
	f.f.wait(func(s fixtureState) bool { return s.Text == Notice(1) })
	r := f.b.ObserveReadiness(context.Background(), Notice(1))
	if r.Input.State != InputNonempty || r.Eligible() || r.Key != (ReadinessKey{}) {
		t.Fatal("diagnostic notice acquired authority", r)
	}
	for _, key := range []ReadinessKey{r.Key, empty.Key} {
		result, err := f.b.Send(context.Background(), 1, key, nil, nil)
		if err != nil || result.Disposition != SendRefused {
			t.Fatal("journal diagnostic admitted Send", result, err)
		}
	}
	s := f.f.wait(func(s fixtureState) bool { return s.PID > 1 })
	if s.Text != Notice(1) || len(s.Submitted) != 0 {
		t.Fatal("diagnostic changed draft", s)
	}
}

// The exact-PID reader refuses nonregular, symlink, oversized and missing
// records without following adjacent files. Synthetic FIFO nodes must refuse
// promptly rather than hanging; no private provider files are read.
func TestRegistryFileBoundary(t *testing.T) {
	for _, mode := range []string{"regular", "missing", "symlink", "directory", "fifo", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "sessions")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "1234.json")
			var err error
			switch mode {
			case "regular":
				err = os.WriteFile(path, []byte("synthetic"), 0600)
			case "symlink":
				other := filepath.Join(root, "unrelated")
				if e := os.WriteFile(other, []byte("private"), 0600); e != nil {
					t.Fatal(e)
				}
				err = os.Symlink(other, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "oversized":
				err = os.WriteFile(path, []byte(strings.Repeat(" ", maxRegistryBytes+1)), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := registryBytes(root, 1234)
			if mode == "regular" {
				if err != nil || string(got) != "synthetic" {
					t.Fatal("valid record refused", err)
				}
			} else if err == nil || len(got) != 0 {
				t.Fatal("unsafe record read", mode)
			}
		})
	}
}

func newBoundFixture(t *testing.T, runtime, version, status string) *boundFixture {
	t.Helper()
	f := newTerminalFixture(t, runtime)
	pid := f.wait(func(s fixtureState) bool { return s.PID > 1 }).PID
	f.target.OwnerPID = pid
	f.target.OwnerBirth, _ = Birth(context.Background(), pid)
	b := BoundTerminal{Target: f.target, Generation: "synthetic-generation", SessionID: "11111111-1111-4111-8111-111111111111"}
	out := &boundFixture{f: f, b: b}
	if runtime != "codex" {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
			t.Fatal(err)
		}
		out.b.ClaudeRegistryRoot = root
		id, err := liveRegistryIdentity(context.Background(), f.target, b.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		out.m = registryFixture(version, status)
		out.m["pid"] = pid
		out.m["procStart"] = id.Birth.Format("Mon Jan _2 15:04:05 2006")
		out.m["tmux"] = "synthetic:@0." + f.target.Pane
		out.record = filepath.Join(root, "sessions", strconv.Itoa(pid)+".json")
		out.write(t)
	}
	return out
}
func (f *boundFixture) write(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.record+".next", data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(f.record+".next", f.record); err != nil {
		t.Fatal(err)
	}
}
func (f *boundFixture) empty(t *testing.T) {
	t.Helper()
	s := f.f.wait(func(s fixtureState) bool { return s.PID > 1 })
	if s.Text != "" || len(s.Submitted) != 0 {
		t.Fatalf("unexpected input effects: %#v", s)
	}
}

// Real isolated tmux and synthetic provider records exercise both directions
// of the keeper's legacy fallback rule. No live provider/session is involved.
func TestBoundReadinessRoutes(t *testing.T) {
	cases := []struct {
		name, runtime, version, status string
		rootless                       bool
		extra                          bool
		want                           bool
		route                          ReadinessRoute
	}{
		{"codex", "codex", "", "", false, false, true, LegacyCodex},
		{"legacy-rootless", "claude", "2.1.310", "idle", true, false, true, LegacyFallback},
		{"named-rootless", "claude-named", "2.1.310", "idle", true, false, false, ""},
		{"legacy-new-version", "claude", "2.1.311", "idle", false, false, true, RegistryLegacy},
		{"named-new-version", "claude-named", "2.1.311", "idle", false, false, true, RegistryNamed},
		{"legacy-schema-drift", "claude", "2.1.310", "idle", false, true, true, LegacyFallback},
		{"named-schema-drift", "claude-named", "3.0.0", "idle", false, true, false, ""},
		{"legacy-busy", "claude", "2.1.310", "busy", false, false, false, ""},
		{"named-busy", "claude-named", "2.1.310", "busy", false, false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newBoundFixture(t, c.runtime, c.version, c.status)
			if c.rootless {
				f.b.ClaudeRegistryRoot = ""
			}
			if c.extra {
				delete(f.m, "cwd")
				f.write(t)
			}
			got := f.b.AssessReadiness(context.Background())
			if got.Eligible() != c.want || got.Key.Route != c.route {
				t.Fatalf("got %#v", got)
			}
			f.empty(t)
			data, _ := json.Marshal(got)
			if strings.Contains(string(data), f.b.ClaudeRegistryRoot) && f.b.ClaudeRegistryRoot != "" || strings.Contains(string(data), "synthetic-session") {
				t.Fatal("private observation leaked")
			}
		})
	}
	for _, version := range []string{"2.1.302", "2.1.309", "2.1.310", "2.1.312", "2.1.317", "2.1.318"} {
		for _, status := range []string{"idle", "shell", "busy"} {
			t.Run(version+"/"+status, func(t *testing.T) {
				f := newBoundFixture(t, "claude-named", version, status)
				got := f.b.AssessReadiness(context.Background())
				if got.Eligible() != (status != "busy") {
					t.Fatalf("got %#v", got)
				}
				f.empty(t)
			})
		}
	}
}

// An idle↔shell transition changes the key even at identical timestamps;
// a prior coherent sample cannot constrain a later different transition.
func TestBoundReadinessStatusValueKey(t *testing.T) {
	f := newBoundFixture(t, "claude-named", "2.1.310", "idle")
	a := f.b.AssessReadiness(context.Background())
	f.m["status"] = "shell"
	f.write(t)
	b := f.b.AssessReadiness(context.Background())
	if !a.Eligible() || !b.Eligible() || a.Key == b.Key || a.Key.RegistryStatusAt != b.Key.RegistryStatusAt {
		t.Fatalf("status not compared: %#v %#v", a, b)
	}
	result, err := f.b.Send(context.Background(), 1, a.Key, nil, nil)
	if err != nil || result.Disposition != SendRefused {
		t.Fatal(result, err)
	}
	f.empty(t)
}

// Every pre-type mismatch has zero input; every post-type mismatch leaves
// the exact synthetic notice with zero submissions. Valid neighbors submit.
func TestBoundSendContinuity(t *testing.T) {
	for _, runtime := range []string{"codex", "claude-named"} {
		for _, stage := range []string{"valid", "pre-key", "pre-geometry", "post-key", "post-geometry", "post-draft", "forged-runtime"} {
			if runtime == "codex" && (stage == "pre-key" || stage == "post-key") {
				continue
			}
			t.Run(runtime+"/"+stage, func(t *testing.T) {
				f := newBoundFixture(t, runtime, "2.1.310", "idle")
				sample := f.b.AssessReadiness(context.Background())
				if !sample.Eligible() {
					t.Fatalf("positive setup: %#v", sample)
				}
				var before, after func()
				changeKey := func() { f.m["statusUpdatedAt"] = int64(1793016002001); f.write(t) }
				geometry := func() {
					if _, err := f.f.target.command(context.Background(), "resize-window", "-t", f.f.target.Pane, "-x", "121", "-y", "24"); err != nil {
						t.Fatal(err)
					}
				}
				switch stage {
				case "pre-key":
					before = changeKey
				case "pre-geometry":
					before = geometry
				case "post-key":
					after = func() { f.f.wait(func(s fixtureState) bool { return s.Text == Notice(1) }); changeKey() }
				case "post-geometry":
					after = func() { f.f.wait(func(s fixtureState) bool { return s.Text == Notice(1) }); geometry() }
				case "post-draft":
					after = func() {
						f.f.typeText("suffix")
						f.f.wait(func(s fixtureState) bool { return strings.HasSuffix(s.Text, "suffix") })
					}
				case "forged-runtime":
					sample.Key.Target.Runtime = "unrecognized"
				}
				got, err := f.b.Send(context.Background(), 1, sample.Key, before, after)
				if err != nil {
					t.Fatal(err)
				}
				state := f.f.wait(func(s fixtureState) bool { return s.PID > 1 })
				switch stage {
				case "valid":
					if got.Disposition != SendSubmitted {
						t.Fatal(got)
					}
					state = f.f.wait(func(s fixtureState) bool { return len(s.Submitted) == 1 })
					if state.Submitted[0] != Notice(1) {
						t.Fatal("wrong submit")
					}
				case "pre-key", "pre-geometry", "forged-runtime":
					if got.Disposition != SendRefused || state.Text != "" || len(state.Submitted) != 0 {
						t.Fatal("continuity gate effects", got, state)
					}
				default:
					if got.Disposition != SendDraft || state.Text == "" || len(state.Submitted) != 0 {
						t.Fatal("continuity gate effects", got, state)
					}
				}
			})
		}
	}
}

// Row0 is a legitimate parser coordinate. Nearby negative coordinates and
// zero width refuse; this pure check is not a first-row end-to-end Send claim.
func TestReadinessKeyCoordinatesAndCrossRuntime(t *testing.T) {
	b, reason, _ := boundInput("› \n\n? for shortcuts", "codex", "", false)
	if reason != InputReasonNone || b.Row != 0 {
		t.Fatal(b, reason)
	}
	k := ReadinessKey{Route: LegacyCodex, Target: Target{Runtime: "codex", OwnerPID: 123, OwnerBirth: "synthetic", Pane: "%0", SocketID: "synthetic"}, Generation: "g", SessionID: "s", ComposerRow: 0, ComposerColumn: 2, PaneWidth: 80}
	if !k.valid() {
		t.Fatal("row0 refused")
	}
	for _, change := range []func(*ReadinessKey){func(k *ReadinessKey) { k.ComposerRow = -1 }, func(k *ReadinessKey) { k.PaneWidth = 0 }, func(k *ReadinessKey) { k.ComposerColumn = 80 }, func(k *ReadinessKey) { k.Target.Runtime = "unknown" }, func(k *ReadinessKey) { k.Target.Runtime = "claude" }, func(k *ReadinessKey) { k.RegistryStatus = "idle" }, func(k *ReadinessKey) { k.Route = "" }} {
		bad := k
		change(&bad)
		if bad.valid() {
			t.Fatal("invalid key accepted")
		}
	}
	data, _ := json.Marshal(k)
	if string(data) != "{}" {
		t.Fatal("key projected")
	}
}

// An unknown version with the existing named composer admits an empty lane and
// BoundTerminal.Send submits exactly its typed notice once. Registry busy,
// rendered busy/dialog, and foreign drafts refuse without an Enter; racing
// foreign suffixes preserve the complete draft after our notice was typed.
func TestBoundNewVersionSend(t *testing.T) {
	for _, mode := range []string{"valid", "registry-busy", "rendered-busy", "dialog", "foreign", "foreign-after-type", "layout"} {
		t.Run(mode, func(t *testing.T) {
			runtime, status := "claude-named", "idle"
			switch mode {
			case "registry-busy":
				status = "busy"
			case "rendered-busy":
				runtime = "claude-named-busy"
			case "dialog":
				runtime = "claude-named-dialog"
			case "layout":
				runtime = "claude-named-unknown"
			}
			f := newBoundFixture(t, runtime, "3.0.0", status)
			if mode == "foreign" {
				f.f.typeText("KEEP")
				f.f.wait(func(s fixtureState) bool { return s.Text == "KEEP" })
			}
			sample := f.b.AssessReadiness(context.Background())
			wantEligible := mode == "valid" || mode == "foreign-after-type"
			if sample.Eligible() != wantEligible {
				t.Fatalf("unexpected readiness: %#v", sample)
			}
			if wantEligible {
				if sample.Key.Route != RegistryNamed || sample.Key.RegistryVersion != "3.0.0" {
					t.Fatalf("missing qualified named route: %#v", sample)
				}
			} else if sample.Key != (ReadinessKey{}) {
				t.Fatal("refused lane acquired a key")
			}
			switch mode {
			case "registry-busy":
				if sample.Runtime.Reason != RuntimeBusy {
					t.Fatal(sample)
				}
			case "rendered-busy", "dialog":
				if sample.Input.Reason != InputReasonBusyOrDialog {
					t.Fatal(sample)
				}
			case "layout":
				if sample.Input.Reason != InputReasonLayoutUnrecognized {
					t.Fatal(sample)
				}
			case "foreign":
				if sample.Input.State != InputNonempty {
					t.Fatal(sample)
				}
			}
			var after func()
			if mode == "foreign-after-type" {
				after = func() {
					f.f.typeText("KEEP")
					f.f.wait(func(s fixtureState) bool { return s.Text == Notice(1)+"KEEP" })
				}
			}
			result, err := f.b.Send(context.Background(), 1, sample.Key, nil, after)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "valid" {
				if result.Disposition != SendSubmitted {
					t.Fatal(result)
				}
				state := f.f.wait(func(s fixtureState) bool { return len(s.Submitted) > 0 })
				if len(state.Submitted) != 1 || state.Submitted[0] != Notice(1) || state.Text != "" {
					t.Fatal("wrong bound submission", state)
				}
				return
			}
			wantText, wantDisposition := "", SendRefused
			if mode == "foreign" {
				wantText = "KEEP"
			}
			if mode == "foreign-after-type" {
				wantText, wantDisposition = Notice(1)+"KEEP", SendDraft
			}
			state := f.f.wait(func(s fixtureState) bool { return s.PID > 1 })
			if result.Disposition != wantDisposition || state.Text != wantText || len(state.Submitted) != 0 {
				t.Fatal("refusal changed or submitted input", result, state)
			}
		})
	}
}

// Unknown releases admit named and unnamed matching composers and submit exactly
// one notice. TestBoundNewVersionSchemaRefusal pairs these with invalid records.
func TestBoundNeighborVersionsStructure(t *testing.T) {
	for _, version := range []string{"2.1.311", "2.1.313", "2.1.316", "2.1.319", "3.0.0"} {
		for _, runtime := range []string{"claude", "claude-named"} {
			t.Run(version+"/"+runtime, func(t *testing.T) {
				f := newBoundFixture(t, runtime, version, "idle")
				before := f.b.AssessReadiness(context.Background())
				f.m["future"] = "PRIVATE-VALUE"
				f.write(t)
				sample := f.b.AssessReadiness(context.Background())
				if !sample.Eligible() || sample.Runtime.Version != version || sample.Key.RegistryVersion != version || sample.Runtime.UnknownKeys != "future" || sample.Key != before.Key {
					t.Fatal("matching structure refused", sample)
				}
				result, err := f.b.Send(context.Background(), 1, sample.Key, nil, nil)
				if err != nil || result.Disposition != SendSubmitted {
					t.Fatal(result, err)
				}
				state := f.f.wait(func(s fixtureState) bool { return len(s.Submitted) > 0 })
				if len(state.Submitted) != 1 || state.Submitted[0] != Notice(1) || state.Text != "" {
					t.Fatal("wrong submission", state)
				}
			})
		}
	}
}

// On an unknown release, invalid required/optional types, missing keys, unknown
// status and identity/version/decoder failures cannot unlock named geometry or
// cause input effects. TestBoundNeighborVersionsStructure is the valid control.
func TestBoundNewVersionSchemaRefusal(t *testing.T) {
	for _, mode := range []string{"required", "asksForConvertedSkills", "statusUpdatedAt", "status", "identity", "version", "missing-version", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			f := newBoundFixture(t, "claude-named", "3.0.0", "idle")
			want := RuntimeSchemaUnqualified
			switch mode {
			case "required":
				delete(f.m, "cwd")
			case "asksForConvertedSkills", "statusUpdatedAt":
				f.m[mode] = "wrong-type"
			case "status":
				f.m["status"] = "future"
				want = RuntimeStatusUnqualified
			case "identity":
				f.m["sessionId"] = "foreign"
				want = RuntimeIdentityMismatch
			case "version":
				f.m["version"] = "3.0.bad"
				want = RuntimeVersionUnqualified
			case "missing-version":
				delete(f.m, "version")
				want = RuntimeVersionUnqualified
			case "duplicate":
				want = RuntimeRegistryUnavailable
			}
			f.write(t)
			if mode == "duplicate" {
				data, _ := json.Marshal(f.m)
				data = append([]byte(`{"version":"3.0.0",`), data[1:]...)
				if err := os.WriteFile(f.record, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			sample := f.b.AssessReadiness(context.Background())
			if sample.Runtime.Reason != want || sample.Eligible() || sample.Key != (ReadinessKey{}) {
				t.Fatal("invalid record admitted", sample)
			}
			result, err := f.b.Send(context.Background(), 1, sample.Key, nil, nil)
			if err != nil || result.Disposition != SendRefused {
				t.Fatal(result, err)
			}
			f.empty(t)
		})
	}
}
