package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type renderRunner struct {
	t       *testing.T
	fail    int
	calls   int
	source  []byte
	encoded []byte
	inputs  []string
	paths   []string
}

func (r *renderRunner) ValidateModel(context.Context, string) error { return nil }

func (r *renderRunner) Run(ctx context.Context, name string, args []string, input io.Reader) error {
	r.calls++
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 121*time.Second {
		r.t.Fatal("unbounded subprocess")
	}
	out := args[len(args)-1]
	r.paths = append(r.paths, out)
	st, err := os.Stat(out)
	if err != nil || st.Mode().Perm() != 0600 {
		r.t.Fatalf("output not private: %v %v", st, err)
	}
	if r.calls == r.fail {
		return errors.New("PRIVATE TEXT in native diagnostics")
	}
	if input != nil {
		b, _ := io.ReadAll(input)
		r.inputs = append(r.inputs, string(b))
		return os.WriteFile(out, []byte("synth audio"), 0600)
	}
	if strings.HasSuffix(out, "voice.ogg") {
		for i, arg := range args {
			if arg == "-i" {
				r.encoded, _ = os.ReadFile(args[i+1])
			}
		}
		return os.WriteFile(out, []byte("OggS fixture OpusHead"), 0600)
	}
	pcm := r.source
	if strings.HasSuffix(out, "notice.pcm") {
		pcm = bytes.Repeat([]byte{42, 0}, sampleRate)
	}
	if strings.HasSuffix(out, "verify.pcm") {
		pcm = r.encoded
	}
	return os.WriteFile(out, pcm, 0600)
}

// A controlled long source really appends the synthesized notice, stays below
// 180s after decode, uses bounded processes and deletes every private output.
// This tests the cap algorithm, not a real engine's pronunciation or encoding.
func TestRenderCapNoticeAndPrivacy(t *testing.T) {
	runner := &renderRunner{t: t, source: bytes.Repeat([]byte{1, 0}, 181*sampleRate)}
	result, err := (LocalRenderer{Runner: runner}).Render(context.Background(), t.TempDir(), Config{true, 180}, "complete text")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.DurationMS > 180000 || result.DurationMS < 179000 {
		t.Fatalf("cap=%+v", result)
	}
	if len(runner.inputs) != 2 || runner.inputs[1] != TruncationNotice {
		t.Fatalf("notice=%q", runner.inputs)
	}
	notice := bytes.Repeat([]byte{42, 0}, sampleRate)
	if !bytes.HasSuffix(runner.encoded, notice) || !bytes.HasPrefix(runner.encoded, []byte{1, 0}) {
		t.Fatal("notice or original prefix missing")
	}
	for _, path := range runner.paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("retained scratch", path)
		}
	}
}

// Every real pipeline stage has an injected process failure paired with a
// successful short control; diagnostics containing private input never escape.
func TestRenderFailuresAndShortControl(t *testing.T) {
	for fail := 0; fail <= 4; fail++ {
		t.Run(string(rune('0'+fail)), func(t *testing.T) {
			runner := &renderRunner{t: t, fail: fail, source: bytes.Repeat([]byte{0, 1}, sampleRate)}
			a, err := (LocalRenderer{Runner: runner}).Render(context.Background(), t.TempDir(), Config{true, 180}, "PRIVATE TEXT")
			if fail == 0 {
				if err != nil || a.Truncated || a.DurationMS != 1000 {
					t.Fatalf("short control: %+v %v", a, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "PRIVATE TEXT") {
				t.Fatalf("failure=%v", err)
			}
			if fail > 0 {
				want := map[int]string{1: "voice synthesis failed", 2: "voice decode failed", 3: "voice encode failed", 4: "voice decode failed"}[fail]
				if runner.calls != fail || !strings.Contains(err.Error(), want) {
					t.Fatalf("wrong stage: calls=%d error=%v", runner.calls, err)
				}
			}
			for _, path := range runner.paths {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("retained scratch", path)
				}
			}
		})
	}
}

// Input and PCM boundaries trigger notice independently; an oversized notice
// is refused, while short audio is unchanged. Waveform covers signed full scale.
func TestCapPCMAndWaveform(t *testing.T) {
	short := []byte{0, 0, 255, 127, 0, 128}
	b, cut, err := capPCM(short, nil, false)
	if err != nil || cut || !bytes.Equal(b, short) {
		t.Fatal("changed short control")
	}
	b, cut, err = capPCM(short, []byte{7, 0}, true)
	if err != nil || !cut || !bytes.HasSuffix(b, []byte{7, 0}) {
		t.Fatal("pre-cut input lost notice")
	}
	if _, _, err = capPCM(short, make([]byte, maxPCMBytes), true); err == nil {
		t.Fatal("accepted oversized notice")
	}
	if got := waveform(short); len(got) != 3 || got[0] != 0 || got[2] != 1024 {
		t.Fatalf("waveform=%v", got)
	}
}

// Gated actual macOS synthesis and Opus encode/decode witness. Uses only the
// caller's explicit isolated model/config directory and separately built binary;
// it writes no audio or text logs and never touches Matrix or the shared binary.
func TestVoiceReal(t *testing.T) {
	if os.Getenv("A2A_VOICE_IT") != "1" {
		t.Skip("explicit local voice witness only")
	}
	if runtime.GOOS != "darwin" {
		t.Fatal("real local witness requires macOS")
	}
	base, exe := os.Getenv("A2A_VOICE_IT_BASE"), os.Getenv("A2A_VOICE_IT_EXECUTABLE")
	if base == "" || !filepath.IsAbs(exe) {
		t.Fatal("explicit isolated base and executable required")
	}
	c := Config{true, 180}
	t.Run("sherpa", func(t *testing.T) {
		audio, err := (LocalRenderer{Executable: exe}).Render(context.Background(), base, c, "Проверка голосового ответа. Полный текст сохраняется в сообщении.")
		if err != nil {
			t.Fatal(err)
		}
		if audio.DurationMS < 1000 || audio.DurationMS > 180000 || len(audio.Waveform) != 100 || audio.Truncated {
			t.Fatalf("invalid rendered witness duration=%d waveform=%d", audio.DurationMS, len(audio.Waveform))
		}
		nonzero := false
		for _, v := range audio.Waveform {
			nonzero = nonzero || v > 0
		}
		if !nonzero {
			t.Fatal("silent rendered audio")
		}
		// Independently inspect basic codec signature; pipeline also fully decodes it.
		if !bytes.HasPrefix(audio.OGG, []byte("OggS")) || !bytes.Contains(audio.OGG, []byte("OpusHead")) {
			t.Fatal("not Ogg Opus")
		}
		t.Logf("engine=%s original_data_dir_bytes=%d duration_ms=%d ogg_bytes=%d waveform_bins=%d", "sherpa", len(filepath.Join(ModelDir(base), "espeak-ng-data")), audio.DurationMS, len(audio.OGG), len(audio.Waveform))
		if os.Getenv("A2A_VOICE_IT_LONG") == "1" {
			long, err := (LocalRenderer{Executable: exe}).Render(context.Background(), base, c, strings.Repeat("Это проверка ограничения длительности голосового ответа. ", 160))
			if err != nil {
				t.Fatal(err)
			}
			if !long.Truncated || long.DurationMS > 180000 || long.DurationMS < 179000 {
				t.Fatalf("actual capped output: truncated=%v duration_ms=%d", long.Truncated, long.DurationMS)
			}
			t.Logf("engine=%s actual_capped_duration_ms=%d ogg_bytes=%d truncated=%v", "sherpa", long.DurationMS, len(long.OGG), long.Truncated)
		}
	})
}

// Little-endian signed waveform decoding is a format assertion, including the
// negative32768 boundary that must not overflow before taking its magnitude.
func TestWaveformSignedBoundary(t *testing.T) {
	b := make([]byte, 200)
	for i := 0; i < 100; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], 0x8000)
	}
	for _, v := range waveform(b) {
		if v != 1024 {
			t.Fatalf("negative full scale=%d", v)
		}
	}
}

// Oversized text is capped before the engine sees it, and even a short fake
// waveform receives the spoken notice when the input itself was shortened.
func TestRenderInputBound(t *testing.T) {
	runner := &renderRunner{t: t, source: bytes.Repeat([]byte{1, 0}, sampleRate)}
	a, err := (LocalRenderer{Runner: runner}).Render(context.Background(), t.TempDir(), Config{true, 180}, strings.Repeat("Я", 6001))
	if err != nil || !a.Truncated || len([]rune(runner.inputs[0])) != 6000 || runner.inputs[1] != TruncationNotice {
		t.Fatalf("input bound failed: truncated=%v err=%v", a.Truncated, err)
	}
}

// The production process adapter reports success/failure without disclosing
// diagnostics, and a cancelled context refuses work. No TTS dependency is used.
func TestLocalRunnerVerdicts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix fixture executable")
	}
	p := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nprintf 'PRIVATE TEXT' >&2\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := (LocalRunner{}).Run(context.Background(), p, nil, nil); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte("#!/bin/sh\nprintf 'PRIVATE TEXT' >&2\nexit 7\n"), 0700)
	if err := (LocalRunner{}).Run(context.Background(), p, nil, nil); err == nil || strings.Contains(err.Error(), "PRIVATE TEXT") {
		t.Fatalf("failed adapter=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (LocalRunner{}).Run(ctx, p, nil, nil); err == nil {
		t.Fatal("cancelled command succeeded")
	}
}
