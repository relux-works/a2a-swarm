package voice

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const sampleRate = 24000
const maxInputRunes = 6000
const maxPCMBytes = (MaxSeconds + 1) * sampleRate * 2
const MaxAudioBytes = 4 << 20

// Runner keeps process I/O replaceable without hiding real synthesis failures.
// Implementations must not retain input or subprocess diagnostics.
type Runner interface {
	Run(context.Context, string, []string, io.Reader) error
	ValidateModel(context.Context, string) error
}
type LocalRunner struct{}

func (LocalRunner) ValidateModel(ctx context.Context, base string) error {
	return ValidateModel(ctx, base)
}

func (LocalRunner) Run(ctx context.Context, name string, args []string, input io.Reader) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		if failure := contextFailure(ctx, err); failure != nil {
			return failure
		}
		return NewSynthesisError(SynthesisExecutable, -1)
	}
	// Wait reaps the exact process, including cancellation; no shell or pipeline.
	if err := cmd.Wait(); err != nil {
		if failure := contextFailure(ctx, err); failure != nil {
			return failure
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return NewSynthesisError(SynthesisWorkerExit, exit.ExitCode())
		}
		return NewSynthesisError(SynthesisUnknown, -1)
	}
	return nil
}

type Audio struct {
	OGG        []byte
	DurationMS int
	Waveform   []int
	Truncated  bool
}
type Renderer interface {
	Render(context.Context, string, Config, string) (Audio, error)
}
type LocalRenderer struct {
	Runner     Runner
	Executable string
}

func (r LocalRenderer) run(ctx context.Context, timeout time.Duration, name string, args []string, in io.Reader) error {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runner := r.Runner
	if runner == nil {
		runner = LocalRunner{}
	}
	return runner.Run(c, name, args, in)
}

func (r LocalRenderer) synth(ctx context.Context, base, text, out string) error {
	runner := r.Runner
	if runner == nil {
		runner = LocalRunner{}
	}
	if err := runner.ValidateModel(ctx, base); err != nil {
		if failure := contextFailure(ctx, err); failure != nil {
			return failure
		}
		return NewSynthesisError(SynthesisValidation, -1)
	}
	exe := r.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return NewSynthesisError(SynthesisExecutable, -1)
		}
	}
	if err := r.run(ctx, 120*time.Second, exe, []string{"--voice-worker", ModelDir(base), out}, strings.NewReader(text)); err != nil {
		return synthesisFailure(ctx, err)
	}
	return nil
}

func readBounded(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(b) > limit {
		return nil, errors.New("voice output exceeds limit")
	}
	return b, nil
}

func privateOutput(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return OutputFailure
	}
	if err := f.Close(); err != nil {
		return OutputFailure
	}
	return nil
}

func (r LocalRenderer) pcm(ctx context.Context, source, out string) ([]byte, error) {
	if err := privateOutput(out); err != nil {
		return nil, err
	}
	err := r.run(ctx, 30*time.Second, "ffmpeg", []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", source, "-t", "181", "-f", "s16le", "-ac", "1", "-ar", "24000", out}, nil)
	if err != nil {
		return nil, DecodeFailure
	}
	b, err := readBounded(out, maxPCMBytes)
	if err != nil || len(b) == 0 || len(b)%2 != 0 {
		return nil, InvalidPCMFailure
	}
	return b, nil
}

// capPCM reserves space for the spoken notice and a short pause. The extra50ms
// keeps the Ogg/Opus container duration (including codec padding) below180s.
func capPCM(pcm, notice []byte, cut bool) ([]byte, bool, error) {
	limit := MaxSeconds * sampleRate * 2
	if !cut && len(pcm) <= limit-sampleRate/10 {
		return pcm, false, nil
	}
	silence := make([]byte, sampleRate/2) //250ms at16bits
	target := limit - sampleRate/10
	keep := target - len(notice) - len(silence)
	keep -= keep % 2
	if keep <= 0 || len(notice) == 0 {
		return nil, false, InvalidNoticeFailure
	}
	if keep > len(pcm) {
		keep = len(pcm)
	}
	out := append([]byte(nil), pcm[:keep]...)
	out = append(out, silence...)
	out = append(out, notice...)
	return out, true, nil
}

func (r LocalRenderer) Render(ctx context.Context, base string, c Config, text string) (Audio, error) {
	var result Audio
	if err := c.Validate(); err != nil {
		return result, NewSynthesisError(SynthesisValidation, -1)
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	// Private0700 temporary directory, removed on every return; no retained text,
	// command output, or audio logs. stdin keeps text out of process arguments.
	dir, err := os.MkdirTemp("", "a2a-voice-*")
	if err != nil {
		return result, ScratchFailure
	}
	defer os.RemoveAll(dir)
	input := []rune(text)
	cut := len(input) > maxInputRunes
	if cut {
		input = input[:maxInputRunes]
	}
	raw := filepath.Join(dir, "source.wav")
	if err := privateOutput(raw); err != nil {
		return result, err
	}
	if err := r.synth(ctx, base, string(input), raw); err != nil {
		return result, err
	}
	pcm, err := r.pcm(ctx, raw, filepath.Join(dir, "source.pcm"))
	if err != nil {
		return result, err
	}
	if cut || len(pcm) > MaxSeconds*sampleRate*2-sampleRate/10 {
		noticeFile := filepath.Join(dir, "notice.wav")
		if err := privateOutput(noticeFile); err != nil {
			return result, err
		}
		if err := r.synth(ctx, base, TruncationNotice, noticeFile); err != nil {
			return result, err
		}
		notice, err := r.pcm(ctx, noticeFile, filepath.Join(dir, "notice.pcm"))
		if err != nil {
			return result, err
		}
		pcm, result.Truncated, err = capPCM(pcm, notice, cut)
		if err != nil {
			return result, err
		}
	}
	source := filepath.Join(dir, "voice.pcm")
	if err := os.WriteFile(source, pcm, 0600); err != nil {
		return result, PrepareEncodingFailure
	}
	ogg := filepath.Join(dir, "voice.ogg")
	if err := privateOutput(ogg); err != nil {
		return result, err
	}
	if err := r.run(ctx, 30*time.Second, "ffmpeg", []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-f", "s16le", "-ar", "24000", "-ac", "1", "-i", source, "-c:a", "libopus", "-application", "voip", "-b:a", "24k", "-ar", "48000", ogg}, nil); err != nil {
		return result, EncodeFailure
	}
	result.OGG, err = readBounded(ogg, MaxAudioBytes)
	if err != nil || !bytes.HasPrefix(result.OGG, []byte("OggS")) || !bytes.Contains(result.OGG[:min(len(result.OGG), 256)], []byte("OpusHead")) {
		return Audio{}, InvalidOGGFailure
	}
	decoded, err := r.pcm(ctx, ogg, filepath.Join(dir, "verify.pcm"))
	if err != nil {
		return Audio{}, err
	}
	if len(decoded) > MaxSeconds*sampleRate*2 {
		return Audio{}, AudioLimitFailure
	}
	result.DurationMS = len(decoded) * 1000 / (sampleRate * 2)
	result.Waveform = waveform(decoded)
	return result, nil
}

func waveform(pcm []byte) []int {
	n := len(pcm) / 2
	bins := min(100, n)
	out := make([]int, bins)
	for i := range out {
		peak := 0
		for j := i * n / bins; j < (i+1)*n/bins; j++ {
			v := int(int16(binary.LittleEndian.Uint16(pcm[j*2:])))
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		out[i] = peak * 1024 / 32768
	}
	return out
}
