package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/voice"
)

const voiceHostileError = "SYNTHETIC_DIAGNOSTIC_TEXT /private/SYNTHETIC_MODEL_PATH token=SYNTHETIC_AUTH_TOKEN\n\x1b[31m"

type postCategoryRunner struct {
	validation error
	failure    error
	failAt     int
	calls      int
	invalid    string
	paths      []string
}

func (r *postCategoryRunner) ValidateModel(context.Context, string) error { return r.validation }
func (r *postCategoryRunner) Run(_ context.Context, _ string, args []string, input io.Reader) error {
	r.calls++
	out := args[len(args)-1]
	r.paths = append(r.paths, out)
	if r.calls == r.failAt {
		return r.failure
	}
	data := []byte("synthetic wav fixture")
	if input == nil {
		data = bytes.Repeat([]byte{1, 0}, 24000)
	}
	if strings.HasSuffix(out, "voice.ogg") {
		data = []byte("OggS synthetic fixture OpusHead")
		if r.invalid == "ogg" {
			data = []byte("invalid fixture")
		}
	}
	if strings.HasSuffix(out, "source.pcm") && r.invalid == "pcm" {
		data = []byte{1}
	}
	return os.WriteFile(out, data, 0600)
}

func assertVoiceNoDiagnostics(t *testing.T, e *env, stdout, stderr string) {
	t.Helper()
	fragments := []string{"SYNTHETIC_DIAGNOSTIC_TEXT", "SYNTHETIC_MODEL_PATH", "SYNTHETIC_AUTH_TOKEN", "\x1b"}
	check := func(data []byte) {
		for _, fragment := range fragments {
			if bytes.Contains(data, []byte(fragment)) {
				t.Fatal("hostile diagnostic sentinel escaped")
			}
		}
	}
	check([]byte(stdout + stderr))
	if err := filepath.WalkDir(e.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			check(data)
		}
		return nil
	}); err != nil {
		t.Fatal("cannot inspect own synthetic artifacts")
	}
}

// Each runner category and downstream audio stage preserves identical text, one exact warning, no audio send/retry, and a valid post neighbor.
func TestPostVoiceTypedCategories(t *testing.T) {
	cases := []struct {
		name, message string
		failure       error
		at            int
	}{
		{"validation", "voice synthesis failed: model/config validation refused", errors.New(voiceHostileError), 0},
		{"executable", "voice synthesis failed: executable resolution or spawn refused", voice.NewSynthesisError(voice.SynthesisExecutable, -1), 1},
		{"cancelled", "voice synthesis failed: context cancelled", context.Canceled, 1},
		{"deadline", "voice synthesis failed: context deadline exceeded", context.DeadlineExceeded, 1},
		{"worker", "voice synthesis failed: worker exited (status 7)", voice.NewSynthesisError(voice.SynthesisWorkerExit, 7), 1},
		{"worker-signal", "voice synthesis failed: worker exited (status unavailable)", voice.NewSynthesisError(voice.SynthesisWorkerExit, -1), 1},
		{"unknown", "voice synthesis failed: unclassified failure", errors.New(voiceHostileError), 1},
		{"decode", "voice decode failed (ffmpeg with Opus support required)", errors.New(voiceHostileError), 2},
		{"encode", "voice encode failed (ffmpeg with libopus required)", errors.New(voiceHostileError), 3},
		{"verify-decode", "voice decode failed (ffmpeg with Opus support required)", errors.New(voiceHostileError), 4},
		{"invalid-pcm", "invalid decoded voice audio", nil, 0},
		{"invalid-ogg", "invalid Ogg/Opus voice output", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := voiceWorld(t)
			runner := &postCategoryRunner{failAt: tc.at}
			if tc.failure != nil {
				runner.failure = fmt.Errorf("%s: %w", voiceHostileError, tc.failure)
			}
			if tc.name == "validation" {
				runner.validation = runner.failure
			}
			if tc.name == "invalid-pcm" {
				runner.invalid = "pcm"
			}
			if tc.name == "invalid-ogg" {
				runner.invalid = "ogg"
			}
			e.configure = func(a *App) { a.voiceRenderer = voice.LocalRenderer{Runner: runner} }
			text := "Complete synthetic text\nretained exactly"
			plain := e.as("alice", "post", "proj", "--topic", "alpha", text).ok(t)
			plainEvent, _ := e.hs.eventByID(strings.TrimSpace(plain.stdout))
			e.hs.mu.Lock()
			before := len(e.hs.events)
			e.hs.mu.Unlock()
			uploads, downloads := e.hs.CallCount(uploadRoute), e.hs.CallCount(downloadRoute)
			refused := e.as("alice", "post", "proj", "--topic", "alpha", "--voice", text).ok(t)
			assertVoiceNoDiagnostics(t, e, refused.stdout, refused.stderr)
			want := "a2a: warning [VOICE_TEXT_ONLY]: " + tc.message + "; text retained\n"
			if refused.stderr != want || strings.Count(refused.stderr, "[VOICE_TEXT_ONLY]") != 1 {
				t.Fatal("typed warning category/count changed")
			}
			retained, ok := e.hs.eventByID(strings.TrimSpace(refused.stdout))
			if !ok || !bytes.Equal(retained.Content, plainEvent.Content) || strings.Count(refused.stdout, "\n") != 1 {
				t.Fatal("successful text receipt/content changed")
			}
			e.hs.mu.Lock()
			after := len(e.hs.events)
			e.hs.mu.Unlock()
			if after != before+1 || e.hs.CallCount(uploadRoute) != uploads || e.hs.CallCount(downloadRoute) != downloads {
				t.Fatal("refused voice published audio or duplicated text")
			}
			wantCalls := tc.at
			if tc.name == "invalid-pcm" {
				wantCalls = 2
			}
			if tc.name == "invalid-ogg" {
				wantCalls = 3
			}
			if runner.calls != wantCalls {
				t.Fatal("refused stage retried or took the wrong path")
			}
			runner.validation, runner.failure, runner.failAt, runner.calls, runner.invalid = nil, nil, 0, 0, ""
			valid := e.as("alice", "post", "proj", "--topic", "alpha", "--voice", text).ok(t)
			if valid.stderr != "" || runner.calls != 4 || e.hs.CallCount(uploadRoute) != uploads+1 || e.hs.CallCount(downloadRoute) != downloads+1 {
				t.Fatal("valid voice post neighbor refused or duplicated")
			}
			validText, ok := e.hs.eventByID(strings.TrimSpace(valid.stdout))
			if !ok || !bytes.Equal(validText.Content, plainEvent.Content) {
				t.Fatal("valid voice changed text")
			}
			e.hs.mu.Lock()
			finalCount := len(e.hs.events)
			last := e.hs.events[finalCount-1]
			e.hs.mu.Unlock()
			if finalCount != after+2 || last.Message().MsgType != "m.audio" {
				t.Fatal("valid neighbor did not publish exactly one audio event")
			}
			assertVoiceNoDiagnostics(t, e, valid.stdout, valid.stderr)
			for _, path := range runner.paths {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("voice scratch survived return")
				}
			}
		})
	}
}

// Arbitrary renderer errors and hostile wrappers are refused at the output boundary without printing or persisting sentinel bytes.
func TestPostVoiceHostileRendererError(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprint(wrapped), func(t *testing.T) {
			e, _, renderer := voiceWorld(t)
			renderer.err = errors.New(voiceHostileError)
			message := "voice synthesis failed: unclassified failure"
			if wrapped {
				renderer.err = fmt.Errorf("%s: %w", voiceHostileError, voice.NewSynthesisError(voice.SynthesisWorkerExit, 7))
				message = "voice synthesis failed: worker exited (status 7)"
			}
			result := e.as("alice", "post", "proj", "--topic", "alpha", "--voice", "Synthetic retained text").ok(t)
			assertVoiceNoDiagnostics(t, e, result.stdout, result.stderr)
			if result.stderr != "a2a: warning [VOICE_TEXT_ONLY]: "+message+"; text retained\n" || e.hs.CallCount(uploadRoute) != 0 || renderer.calls != 1 {
				t.Fatal("hostile renderer changed warning or published audio")
			}
			renderer.err = nil
			valid := e.as("alice", "post", "proj", "--topic", "alpha", "--voice", "Synthetic retained text").ok(t)
			if valid.stderr != "" || e.hs.CallCount(uploadRoute) != 1 || renderer.calls != 2 {
				t.Fatal("valid renderer neighbor refused")
			}
		})
	}
	var output bytes.Buffer
	app := &App{Stderr: &output}
	app.voiceWarning(errors.New(voiceHostileError))
	if output.String() != "a2a: warning [VOICE_TEXT_ONLY]: voice synthesis failed: unclassified failure; text retained\n" {
		t.Fatal("warning boundary admitted uncontrolled diagnostics")
	}
}
