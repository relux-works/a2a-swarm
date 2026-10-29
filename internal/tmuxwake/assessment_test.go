package tmuxwake

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Existing recognized layouts get closed observations; adjacent drafts,
// dialogs, cursor mismatches and unknown named layouts never become empty.
// Synthetic frames establish parser behavior, not provider readiness.
func TestInputAssessmentClosedReasons(t *testing.T) {
	claude := "──────────\n❯ \n──────────\n"
	codex := "› \n\n? for shortcuts\n"
	for _, tc := range []struct {
		name, runtime, screen string
		x, y                  int
		want                  InputAssessment
	}{
		{"claude empty", "claude", claude, 2, 1, InputAssessment{InputEmpty, InputReasonNone}},
		{"codex empty", "codex", codex, 2, 0, InputAssessment{InputEmpty, InputReasonNone}},
		{"claude draft", "claude", strings.Replace(claude, "❯ ", "❯ KEEP", 1), 6, 1, InputAssessment{InputNonempty, InputReasonInputNonempty}},
		{"codex draft", "codex", strings.Replace(codex, "› ", "› KEEP", 1), 6, 0, InputAssessment{InputNonempty, InputReasonInputNonempty}},
		{"busy", "claude", "Esc to interrupt\n" + claude, 2, 2, InputAssessment{InputUnavailable, InputReasonBusyOrDialog}},
		{"dialog", "codex", "Permission required\n" + codex, 2, 1, InputAssessment{InputUnavailable, InputReasonBusyOrDialog}},
		{"wrapped", "claude", strings.Replace(claude, "❯ \n", "❯ \nKEEP\n", 1), 2, 1, InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}},
		{"named rule", "claude", strings.Replace(claude, "──────────", "──── session ─", 1), 2, 1, InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}},
		{"unknown runtime", "other", claude, 2, 1, InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}},
		{"unsupported draft", "claude", strings.Replace(claude, "❯ ", "❯ 文字", 1), 4, 1, InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}},
		{"cursor column", "claude", claude, 3, 1, InputAssessment{InputUnavailable, InputReasonCursorMismatch}},
		{"cursor row", "codex", codex, 2, 1, InputAssessment{InputUnavailable, InputReasonCursorMismatch}},
		{"dim draft suffix", "claude", strings.Replace(claude, "❯ ", "❯ KEEP\x1b[2mSECRET\x1b[0m", 1), 6, 1, InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := assessInput(tc.screen, tc.runtime, tc.x, tc.y); got != tc.want {
				t.Fatalf("assessment = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// All current sanitized Claude fixtures retain the old empty admission and
// exact cursor requirement. The owned Codex draft has no empty admission.
func TestInputAssessmentCurrentCorpus(t *testing.T) {
	files, err := filepath.Glob("testdata/claude-idle-*.txt")
	if err != nil || len(files) != 3 {
		t.Fatalf("fixture inventory: %d %v", len(files), err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		b, ok := Input(string(raw), "claude")
		if !ok || b.Text != "" {
			t.Fatal("existing positive fixture unavailable")
		}
		if got := assessInput(string(raw), "claude", b.Column, b.Row); got != (InputAssessment{InputEmpty, InputReasonNone}) {
			t.Fatalf("%s: %+v", file, got)
		}
		if got := assessInput(string(raw), "claude", b.Column+1, b.Row); got != (InputAssessment{InputUnavailable, InputReasonCursorMismatch}) {
			t.Fatalf("cursor mismatch admitted: %+v", got)
		}
	}
	raw, err := os.ReadFile("testdata/codex-owned-notice-no-hint.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := assessInput(string(raw), "codex", 2, 0); got != (InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}) {
		t.Fatalf("journal lookalike acquired admission: %+v", got)
	}
}

// AssessInput performs only observation on private synthetic terminals.
// Existing Idle agrees, drafts stay intact and recycled pins stay unavailable.
func TestTargetAssessmentReadOnly(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			f := newTerminalFixture(t, runtime)
			before := f.wait(func(s fixtureState) bool { return true })
			if got := f.target.AssessInput(context.Background()); got != (InputAssessment{InputEmpty, InputReasonNone}) || !f.target.Idle(context.Background()) {
				t.Fatalf("valid private target unavailable: %+v", got)
			}
			if after := f.wait(func(s fixtureState) bool { return true }); !reflect.DeepEqual(before, after) {
				t.Fatal("empty assessment changed private input")
			}
			bad := f.target
			bad.SocketID = "replaced"
			if got := bad.AssessInput(context.Background()); got != (InputAssessment{InputUnavailable, InputReasonTargetUnavailable}) {
				t.Fatalf("replaced pin admitted: %+v", got)
			}
			f.typeText("KEEP")
			before = f.wait(func(s fixtureState) bool { return s.Text == "KEEP" })
			got := f.target.AssessInput(context.Background())
			if got.State == InputEmpty || f.target.Idle(context.Background()) {
				t.Fatalf("draft admitted: %+v", got)
			}
			if runtime == "claude" && got != (InputAssessment{InputNonempty, InputReasonInputNonempty}) {
				t.Fatalf("recognized draft reason: %+v", got)
			}
			if after := f.wait(func(s fixtureState) bool { return true }); !reflect.DeepEqual(before, after) {
				t.Fatal("assessment changed draft or submitted input")
			}
		})
	}
}

// A capture failure is distinct from a bad target, with no raw error leakage.
// The adjacent real-target check proves the wrapper does not reject all reads.
func TestTargetAssessmentCaptureFailure(t *testing.T) {
	f := newTerminalFixture(t, "claude")
	if got := f.target.AssessInput(context.Background()); got.State != InputEmpty {
		t.Fatalf("positive control: %+v", got)
	}
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \" $* \" in *' capture-pane '*) echo PRIVATE_CAPTURE_ERROR >&2; exit 1;; esac\nexec '" + strings.ReplaceAll(real, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Every tmux call now runs through a shell wrapper; under host load the
	// target checks in Inspect could reach the production bound and report
	// target_unavailable before the planted capture failure (#117). The wider
	// budget applies only to this wrapper path and is restored afterwards.
	saved := tmuxCommandTimeout
	tmuxCommandTimeout = 30 * time.Second
	t.Cleanup(func() { tmuxCommandTimeout = saved })
	got := f.target.AssessInput(context.Background())
	if got != (InputAssessment{InputUnavailable, InputReasonCaptureUnavailable}) {
		t.Fatalf("capture failure: %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil || string(raw) != `{"state":"unavailable","reason":"capture_unavailable"}` {
		t.Fatalf("unsafe projection: %s %v", raw, err)
	}
}

// Proves the production bound on tmux operations stays 3 seconds; only the
// capture-failure test raises it, for its wrapper path (#117).
func TestTmuxCommandTimeoutDefault(t *testing.T) {
	if tmuxCommandTimeout != 3*time.Second {
		t.Fatalf("tmux command timeout = %v, want 3s", tmuxCommandTimeout)
	}
}
