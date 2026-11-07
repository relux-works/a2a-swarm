// Package voiceaudio converts a bounded, already fetched Ogg/Opus object into
// canonical mono 16 kHz PCM16 WAV using an explicitly provisioned local helper.
// It has no media fetch, event mutation, installation, or speech inference API.
package voiceaudio

import (
	"bytes"
	"context"
	"encoding/binary"
	"time"
)

const (
	MaxInputBytes      = 8 << 20
	SampleRate         = 16000
	MaxSamples         = 30 * SampleRate
	MaxPCMBytes        = 2 * MaxSamples
	MaxDiagnosticBytes = 16 << 10
	WallLimit          = 15 * time.Second
	ProfileID          = "xiph-opus-1.6.1-opusfile-0.12-ogg-1.3.6-resampler-0.2-vap1"
)

type Reason string

const (
	Encrypted           Reason = "encrypted_media"
	InputLimit          Reason = "input_limit"
	Malformed           Reason = "malformed_audio"
	Unsupported         Reason = "unsupported_codec"
	Ambiguous           Reason = "ambiguous_stream"
	MissingProfile      Reason = "missing_decoder_profile"
	ProfileMismatch     Reason = "decoder_profile_mismatch"
	PlatformUnqualified Reason = "platform_unqualified"
	UnsafeTMPDIR        Reason = "unsafe_tmpdir"
	Busy                Reason = "decoder_busy"
	Cancelled           Reason = "cancelled"
	Timeout             Reason = "timeout"
	DecodeFailed        Reason = "decode_failed"
	OutputInvalid       Reason = "invalid_decoder_output"
	DiagnosticLimit     Reason = "decoder_output_limit"
	DurationLimit       Reason = "duration_limit"
	CleanupRefused      Reason = "cleanup_refused"
)

// Refusal contains only a closed nonsecret reason, never tool diagnostics.
type Refusal struct{ Reason Reason }

func (r *Refusal) Error() string { return "voiceaudio: " + string(r.Reason) }

// Profile is trusted local configuration, never derived from peer media.
// SHA256 pins a complete static native executable, including its codec closure.
type Profile struct {
	ID         string
	Executable string
	SHA256     string
	Platform   string
}

type Request struct {
	Bytes     []byte
	Encrypted bool
	TMPDIR    string
	Profile   Profile
}

type Audio struct {
	WAV      []byte
	Samples  int
	Duration time.Duration
}

var gate = make(chan struct{}, 1)
var nativeHeader = []byte{'V', 'A', 'P', '1', 0x80, 0x3e, 0, 0, 1, 0, 16, 0}

// Decode returns zero Audio on every refusal, including cleanup failure.
// The caller must retain the original event and fetch handle on every outcome.
// Callers must not mutate Bytes or Profile concurrently with Decode.
func Decode(ctx context.Context, r Request) (audio Audio, refusal *Refusal) {
	fail := func(reason Reason) (Audio, *Refusal) { return Audio{}, &Refusal{reason} }
	if r.Encrypted {
		return fail(Encrypted)
	}
	if len(r.Bytes) > MaxInputBytes {
		return fail(InputLimit)
	}
	ctx, cancel := context.WithTimeout(ctx, WallLimit)
	defer cancel()
	if reason := contextReason(ctx); reason != "" {
		return fail(reason)
	}
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		return fail(Busy)
	}
	info, reason := inspectOgg(ctx, r.Bytes)
	if reason != "" {
		return fail(reason)
	}
	scratch, reason := newScratch(r.TMPDIR)
	if reason != "" {
		return fail(reason)
	}
	defer func() {
		if !scratch.cleanup() {
			audio = Audio{}
			refusal = &Refusal{CleanupRefused}
		}
	}()
	if !scratch.writeInput(r.Bytes) {
		return fail(UnsafeTMPDIR)
	}
	if reason = snapshot(ctx, scratch, r.Profile); reason != "" {
		return fail(reason)
	}
	data, reason := runNative(ctx, scratch)
	if reason != "" {
		return fail(reason)
	}
	if len(data) < len(nativeHeader) || !bytes.Equal(data[:len(nativeHeader)], nativeHeader) {
		return fail(OutputInvalid)
	}
	pcm := data[len(nativeHeader):]
	if len(pcm) > MaxPCMBytes {
		return fail(DurationLimit)
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 || int64(len(pcm)/2) != info.samples48/3 {
		return fail(OutputInvalid)
	}
	if reason = contextReason(ctx); reason != "" {
		return fail(reason)
	}
	wav := make([]byte, 44+len(pcm))
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(36+len(pcm)))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], SampleRate)
	binary.LittleEndian.PutUint32(wav[28:], SampleRate*2)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(pcm)))
	copy(wav[44:], pcm)
	return Audio{wav, len(pcm) / 2, time.Duration(len(pcm)/2) * time.Second / SampleRate}, nil
}

func contextReason(ctx context.Context) Reason {
	switch ctx.Err() {
	case context.Canceled:
		return Cancelled
	case context.DeadlineExceeded:
		return Timeout
	default:
		return ""
	}
}
