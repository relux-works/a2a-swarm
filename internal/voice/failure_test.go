package voice

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const hostileDiagnostic = "SYNTHETIC_SPOKEN_SENTINEL /private/SYNTHETIC_MODEL_SENTINEL token=SYNTHETIC_TOKEN_SENTINEL\n\x1b[31m"

func assertSynthesis(t *testing.T, err error, category SynthesisCategory, status int, message string) {
	t.Helper()
	var failure *SynthesisError
	if !errors.As(err, &failure) || failure == nil || failure.Category() != category {
		t.Fatal("synthesis category was lost or changed")
	}
	code, available := failure.ExitStatus()
	if available != (status > 0) || available && code != status {
		t.Fatal("unsafe or incorrect exit status")
	}
	if err.Error() != message {
		t.Fatal("fixed synthesis diagnostic changed or leaked input")
	}
}

// Real synthetic children distinguish spawn, exit, signal and context observations while discarding both diagnostic streams.
func TestSynthesisLocalRunnerCategories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix synthetic process fixtures")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(fixture, []byte("#!/bin/sh\nprintf 'SYNTHETIC_STDOUT_SENTINEL'\nprintf 'SYNTHETIC_STDERR_SENTINEL' >&2\n"+body+"\n"), 0700); err != nil {
			t.Fatal("cannot create process fixture")
		}
	}
	cases := []struct {
		name, body, message string
		category            SynthesisCategory
		status              int
	}{
		{"spawn", "exit 0", "voice synthesis failed: executable resolution or spawn refused", SynthesisExecutable, -1},
		{"exit", "exit 7", "voice synthesis failed: worker exited (status 7)", SynthesisWorkerExit, 7},
		{"signal", "kill -TERM $$", "voice synthesis failed: worker exited (status unavailable)", SynthesisWorkerExit, -1},
		{"cancelled", "exec /bin/sleep 5", "voice synthesis failed: context cancelled", SynthesisCancelled, -1},
		{"deadline", "exec /bin/sleep 5", "voice synthesis failed: context deadline exceeded", SynthesisDeadline, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write(tc.body)
			ctx := context.Background()
			name := fixture
			cancel := func() {}
			if tc.name == "spawn" {
				name = filepath.Join(dir, "SYNTHETIC_MODEL_SENTINEL", "missing")
			}
			if tc.name == "cancelled" {
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if tc.name == "deadline" {
				ctx, cancel = context.WithTimeout(ctx, 40*time.Millisecond)
			}
			defer cancel()
			err := (LocalRunner{}).Run(ctx, name, []string{hostileDiagnostic}, strings.NewReader(hostileDiagnostic))
			assertSynthesis(t, err, tc.category, tc.status, tc.message)
			write("exit 0")
			if err := (LocalRunner{}).Run(context.Background(), fixture, nil, nil); err != nil {
				t.Fatal("valid process neighbor refused")
			}
		})
	}
	t.Run("permission", func(t *testing.T) {
		write("exit 0")
		if os.Chmod(fixture, 0600) != nil {
			t.Fatal("cannot prepare nonexecutable fixture")
		}
		assertSynthesis(t, (LocalRunner{}).Run(context.Background(), fixture, nil, nil), SynthesisExecutable, -1, cases[0].message)
		write("exit 0")
		if os.Chmod(fixture, 0700) != nil || (LocalRunner{}).Run(context.Background(), fixture, nil, nil) != nil {
			t.Fatal("executable neighbor refused")
		}
	})
}

type categoryRunner struct {
	*renderRunner
	validation error
	failure    error
	failAt     int
	seen       int
	invalidPCM bool
	invalidOGG bool
}

func (r *categoryRunner) ValidateModel(context.Context, string) error { return r.validation }
func (r *categoryRunner) Run(ctx context.Context, name string, args []string, input io.Reader) error {
	r.seen++
	if r.seen == r.failAt {
		return r.failure
	}
	if r.invalidPCM && strings.HasSuffix(args[len(args)-1], "source.pcm") {
		return os.WriteFile(args[len(args)-1], []byte{1}, 0600)
	}
	if r.invalidOGG && strings.HasSuffix(args[len(args)-1], "voice.ogg") {
		return os.WriteFile(args[len(args)-1], []byte("invalid fixture"), 0600)
	}
	return r.renderRunner.Run(ctx, name, args, input)
}

// Every synthesis category survives the renderer with no audio, scratch or hostile diagnostics; each refusal has a valid render neighbor.
func TestSynthesisRenderCategories(t *testing.T) {
	cases := []struct {
		name, message string
		category      SynthesisCategory
		status        int
		err           error
	}{
		{"validation", "voice synthesis failed: model/config validation refused", SynthesisValidation, -1, errors.New(hostileDiagnostic)},
		{"executable", "voice synthesis failed: executable resolution or spawn refused", SynthesisExecutable, -1, NewSynthesisError(SynthesisExecutable, -1)},
		{"cancelled", "voice synthesis failed: context cancelled", SynthesisCancelled, -1, context.Canceled},
		{"deadline", "voice synthesis failed: context deadline exceeded", SynthesisDeadline, -1, context.DeadlineExceeded},
		{"worker", "voice synthesis failed: worker exited (status 7)", SynthesisWorkerExit, 7, NewSynthesisError(SynthesisWorkerExit, 7)},
		{"unknown", "voice synthesis failed: unclassified failure", SynthesisUnknown, -1, errors.New(hostileDiagnostic)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			runner := &categoryRunner{renderRunner: &renderRunner{t: t, source: bytes.Repeat([]byte{1, 0}, sampleRate)}, failAt: 1, failure: fmt.Errorf("%s: %w", hostileDiagnostic, tc.err)}
			if tc.name == "validation" {
				runner.validation, runner.failAt = tc.err, 0
			}
			audio, err := (LocalRenderer{Runner: runner}).Render(context.Background(), base, Config{true, MaxSeconds}, hostileDiagnostic)
			assertSynthesis(t, err, tc.category, tc.status, tc.message)
			if len(audio.OGG) != 0 || runner.seen > 1 {
				t.Fatal("refusal retained audio or retried")
			}
			runner.validation, runner.failure, runner.failAt, runner.seen = nil, nil, 0, 0
			audio, err = (LocalRenderer{Runner: runner}).Render(context.Background(), base, Config{true, MaxSeconds}, "valid synthetic input")
			if err != nil || audio.DurationMS != 1000 || len(audio.OGG) == 0 || runner.seen != 4 {
				t.Fatal("valid render neighbor refused or duplicated")
			}
			for _, path := range runner.paths {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("private scratch survived render")
				}
			}
		})
	}
	t.Run("config", func(t *testing.T) {
		r := &renderRunner{t: t}
		_, err := (LocalRenderer{Runner: r}).Render(context.Background(), t.TempDir(), Config{}, hostileDiagnostic)
		assertSynthesis(t, err, SynthesisValidation, -1, cases[0].message)
		if r.calls != 0 {
			t.Fatal("invalid config launched a process")
		}
	})
}

// Decode, encode and invalid PCM/container outcomes retain their own fixed stages rather than masquerading as synthesis failures.
func TestSynthesisOtherRenderStages(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   int
		want RenderFailure
	}{
		{"decode", 2, DecodeFailure}, {"encode", 3, EncodeFailure}, {"verify-decode", 4, DecodeFailure},
		{"invalid-pcm", 0, InvalidPCMFailure}, {"invalid-ogg", 0, InvalidOGGFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &categoryRunner{renderRunner: &renderRunner{t: t, source: bytes.Repeat([]byte{1, 0}, sampleRate)}, failAt: tc.at, failure: errors.New(hostileDiagnostic), invalidPCM: tc.name == "invalid-pcm", invalidOGG: tc.name == "invalid-ogg"}
			audio, err := (LocalRenderer{Runner: r}).Render(context.Background(), t.TempDir(), Config{true, MaxSeconds}, "synthetic input")
			if err != tc.want || SafeRenderError(err) != tc.want || len(audio.OGG) != 0 {
				t.Fatal("render failure stage changed or audio escaped")
			}
			r.failAt, r.seen, r.invalidPCM, r.invalidOGG = 0, 0, false, false
			r.renderRunner.calls = 0
			if _, err := (LocalRenderer{Runner: r}).Render(context.Background(), t.TempDir(), Config{true, MaxSeconds}, "valid synthetic input"); err != nil {
				t.Fatal("valid stage neighbor refused")
			}
		})
	}
}

// Invalid enum/status values and hostile wrappers can contribute neither new categories nor diagnostic bytes.
func TestSynthesisClosedContract(t *testing.T) {
	for _, status := range []int{-100, -1, 0, 256, 1 << 30} {
		err := NewSynthesisError(SynthesisWorkerExit, status)
		assertSynthesis(t, err, SynthesisWorkerExit, -1, "voice synthesis failed: worker exited (status unavailable)")
	}
	for _, status := range []int{1, 7, 255} {
		err := NewSynthesisError(SynthesisWorkerExit, status)
		assertSynthesis(t, err, SynthesisWorkerExit, status, fmt.Sprintf("voice synthesis failed: worker exited (status %d)", status))
	}
	for _, err := range []error{errors.New(hostileDiagnostic), NewSynthesisError(255, 7), RenderFailure(255), (*SynthesisError)(nil)} {
		assertSynthesis(t, SafeRenderError(err), SynthesisUnknown, -1, "voice synthesis failed: unclassified failure")
	}
	err := fmt.Errorf("%s: %w", hostileDiagnostic, NewSynthesisError(SynthesisWorkerExit, 7))
	assertSynthesis(t, SafeRenderError(err), SynthesisWorkerExit, 7, "voice synthesis failed: worker exited (status 7)")
}

type deadlineRunner struct {
	t       *testing.T
	parent  time.Time
	render  time.Time
	process time.Time
}

func (r *deadlineRunner) ValidateModel(ctx context.Context, _ string) error {
	r.render, _ = ctx.Deadline()
	if remaining := time.Until(r.render); remaining <= 0 || remaining > 4*time.Minute {
		r.t.Fatal("render deadline changed or unbounded")
	}
	return nil
}
func (r *deadlineRunner) Run(ctx context.Context, _ string, _ []string, _ io.Reader) error {
	r.process, _ = ctx.Deadline()
	if remaining := time.Until(r.process); remaining <= 0 || remaining > 120*time.Second {
		r.t.Fatal("synthesis deadline changed or unbounded")
	}
	if !r.parent.IsZero() && (r.process.After(r.parent) || r.render.After(r.parent)) {
		r.t.Fatal("child deadline extended the parent")
	}
	return context.DeadlineExceeded
}

// Fixed synthesis/render bounds never extend an earlier caller deadline; audio duration remains an independent fixed config bound.
func TestSynthesisDeadlineContract(t *testing.T) {
	for _, parentLimit := range []time.Duration{0, time.Minute} {
		t.Run(parentLimit.String(), func(t *testing.T) {
			ctx := context.Background()
			cancel := func() {}
			r := &deadlineRunner{t: t}
			if parentLimit != 0 {
				ctx, cancel = context.WithTimeout(ctx, parentLimit)
				r.parent, _ = ctx.Deadline()
			}
			defer cancel()
			_, err := (LocalRenderer{Runner: r}).Render(ctx, t.TempDir(), Config{true, MaxSeconds}, "synthetic fixture")
			assertSynthesis(t, err, SynthesisDeadline, -1, "voice synthesis failed: context deadline exceeded")
		})
	}
}

// A failure while synthesizing the truncation notice retains the same bounded category and cleans earlier audio.
func TestSynthesisNoticeCategory(t *testing.T) {
	r := &categoryRunner{renderRunner: &renderRunner{t: t, source: bytes.Repeat([]byte{1, 0}, 181*sampleRate)}, failAt: 3, failure: NewSynthesisError(SynthesisWorkerExit, 7)}
	audio, err := (LocalRenderer{Runner: r}).Render(context.Background(), t.TempDir(), Config{true, MaxSeconds}, "synthetic fixture")
	assertSynthesis(t, err, SynthesisWorkerExit, 7, "voice synthesis failed: worker exited (status 7)")
	if len(audio.OGG) != 0 || r.seen != 3 {
		t.Fatal("notice failure published audio or retried")
	}
	for _, path := range r.paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("notice failure retained scratch")
		}
	}
	r.failAt, r.seen, r.renderRunner.calls = 0, 0, 0
	if audio, err := (LocalRenderer{Runner: r}).Render(context.Background(), t.TempDir(), Config{true, MaxSeconds}, "synthetic fixture"); err != nil || !audio.Truncated {
		t.Fatal("valid notice neighbor refused")
	}
}
