// Package voicetranscribe runs a pinned, explicitly provisioned local ASR CLI.
// It consumes fetched bytes; it has no media, inbox, network or installer path.
package voicetranscribe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// These are candidate bounds for source/Meta adoption, not installed policy.
const (
	CodecPCM16WAV      = "wav-pcm-s16le-16000-mono"
	MaxInputBytes      = 8 << 20
	SampleRate         = 16000
	Channels           = 1
	BitsPerSample      = 16
	MaxDuration        = 30 * time.Second
	MaxPCMBytes        = 960000
	MaxStdoutBytes     = 64 << 10
	MaxTranscriptBytes = 16 << 10
	ValidationDeadline = 15 * time.Second
	InferenceDeadline  = 60 * time.Second
	TotalDeadline      = 90 * time.Second
	EngineID           = "sherpa-onnx v1.13.8 CPU"
	ModelID            = "csukuangfj/sherpa-onnx-whisper-tiny@65176e2deb88badc814a94058666cadccc29b61c:int8"
)

// EventRef is opaque caller data, copied exactly, including on every refusal.
// It is neither verified sender identity nor authority to act on speech.
type EventRef struct {
	EventID     string `json:"event_id"`
	FetchHandle string `json:"fetch_handle"`
}

type Request struct {
	Original  EventRef
	Audio     []byte // caller-owned, immutable for the duration of Transcribe
	Codec     string
	Encrypted bool // caller must propagate the ingress encryption refusal
}

type Transcript struct {
	Text             string   `json:"text"`
	Language         string   `json:"language"`
	Model            string   `json:"model"`
	Engine           string   `json:"engine"`
	Confidence       *float64 `json:"confidence"` // always null: no calibration established
	ConfidenceReason string   `json:"confidence_reason"`
	Untrusted        bool     `json:"untrusted"`
	DurationMS       int      `json:"duration_ms"`
}

type Outcome struct {
	Original   EventRef    `json:"original"`
	Transcript *Transcript `json:"transcript,omitempty"`
	Refusal    *Refusal    `json:"refusal,omitempty"`
}

type Code string

const (
	EncryptedMedia     Code = "encrypted_media"
	Oversize           Code = "oversize"
	CorruptAudio       Code = "corrupt_audio"
	UnsupportedCodec   Code = "unsupported_codec"
	MissingModel       Code = "missing_model"
	ModelIntegrity     Code = "model_integrity"
	BackendUnavailable Code = "backend_unavailable"
	BackendIntegrity   Code = "backend_integrity"
	Busy               Code = "busy"
	Timeout            Code = "timeout"
	Cancelled          Code = "cancelled"
	EngineFailed       Code = "engine_failed"
	InvalidOutput      Code = "invalid_output"
	OutputLimit        Code = "output_limit"
	TemporaryRoot      Code = "temporary_root"
	CleanupFailed      Code = "cleanup_failed"
)

type Stage string

const (
	InputStage     Stage = "input"
	GateStage      Stage = "gate"
	ScratchStage   Stage = "scratch"
	ModelStage     Stage = "model"
	BackendStage   Stage = "backend"
	InferenceStage Stage = "inference"
	OutputStage    Stage = "output"
	CleanupStage   Stage = "cleanup"
)

// Refusal retains only parent-observed closed categories, never an engine error,
// transcript, input, filesystem path, argv, exit diagnostic or underlying error.
type Refusal struct {
	Code  Code  `json:"code"`
	Stage Stage `json:"stage"`
}

func (*Refusal) Error() string { return "local transcription refused" }

func refused(ref EventRef, code Code, stage Stage) Outcome {
	return Outcome{Original: ref, Refusal: &Refusal{Code: code, Stage: stage}}
}

// processGate is shared by all Adapter instances: one operation, no queue. It
// stays held through validation, child join and scratch cleanup.
var processGate = make(chan struct{}, 1)

type Adapter struct {
	profile Profile
	tempDir string
	native  []Artifact
	model   []Artifact
}

// New accepts an explicit local profile and caller TMPDIR. It does not inspect
// HOME, PATH, environment model caches or the network, and does not provision.
// Invalid/unavailable profiles produce typed outcomes from Transcribe.
func New(profile Profile, tempDir string) *Adapter {
	return &Adapter{profile: profile, tempDir: tempDir,
		native: profileArtifacts(profile), model: modelArtifacts()}
}

func contextCode(ctx context.Context) Code {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Timeout
	}
	return Cancelled
}

func (a *Adapter) Transcribe(ctx context.Context, req Request) (out Outcome) {
	if req.Encrypted {
		return refused(req.Original, EncryptedMedia, InputStage)
	}
	if len(req.Audio) > MaxInputBytes {
		return refused(req.Original, Oversize, InputStage)
	}
	if req.Codec != CodecPCM16WAV {
		return refused(req.Original, UnsupportedCodec, InputStage)
	}
	pcmBytes, code := validateWAV(req.Audio)
	if code != "" {
		return refused(req.Original, code, InputStage)
	}
	if ctx == nil {
		return refused(req.Original, Cancelled, InputStage)
	}
	ctx, cancel := context.WithTimeout(ctx, TotalDeadline)
	defer cancel()
	if ctx.Err() != nil {
		return refused(req.Original, contextCode(ctx), InputStage)
	}
	select {
	case processGate <- struct{}{}:
		defer func() { <-processGate }()
	default:
		return refused(req.Original, Busy, GateStage)
	}
	if a == nil || len(a.native) == 0 {
		return refused(req.Original, BackendUnavailable, BackendStage)
	}
	root, err := openDirectory(a.tempDir, true)
	if err != nil {
		return refused(req.Original, TemporaryRoot, ScratchStage)
	}
	defer root.Close()
	op, name, err := makeOperation(root)
	if err != nil {
		return refused(req.Original, TemporaryRoot, ScratchStage)
	}
	defer func() {
		closeErr := op.Close()
		removeErr := root.RemoveAll(name)
		if closeErr != nil || removeErr != nil {
			out = refused(req.Original, CleanupFailed, CleanupStage)
		}
	}()
	for _, dir := range []string{"bin", "lib", "model", "home", "tmp", "cache"} {
		if op.Mkdir(dir, 0700) != nil {
			return refused(req.Original, TemporaryRoot, ScratchStage)
		}
	}
	if err := writeInput(op, req.Audio); err != nil {
		return refused(req.Original, TemporaryRoot, ScratchStage)
	}
	validation, stopValidation := context.WithTimeout(ctx, ValidationDeadline)
	defer stopValidation()
	source, err := openDirectory(a.profile.Directory, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return refused(req.Original, MissingModel, ModelStage)
		}
		return refused(req.Original, BackendIntegrity, BackendStage)
	}
	defer source.Close()
	// Verify/copy models first so missing models never start a native process.
	for _, group := range []struct {
		files            []Artifact
		stage            Stage
		missing, damaged Code
	}{
		{a.model, ModelStage, MissingModel, ModelIntegrity},
		{a.native, BackendStage, BackendUnavailable, BackendIntegrity},
	} {
		for _, file := range group.files {
			err := snapshotFile(validation, source, op, file)
			if validation.Err() != nil {
				return refused(req.Original, contextCode(validation), group.stage)
			}
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return refused(req.Original, group.missing, group.stage)
				}
				return refused(req.Original, group.damaged, group.stage)
			}
		}
	}
	stopValidation()
	if ctx.Err() != nil {
		return refused(req.Original, contextCode(ctx), InferenceStage)
	}
	dir := filepath.Join(a.tempDir, name)
	data, failure := runEngine(ctx, dir)
	if failure != nil {
		return refused(req.Original, failure.Code, failure.Stage)
	}
	text, language, code := parseOutput(data)
	if code != "" {
		return refused(req.Original, code, OutputStage)
	}
	if ctx.Err() != nil {
		return refused(req.Original, contextCode(ctx), OutputStage)
	}
	return Outcome{Original: req.Original, Transcript: &Transcript{
		Text: text, Language: language, Model: ModelID, Engine: EngineID,
		Confidence: nil, ConfidenceReason: "no calibrated confidence established",
		Untrusted: true, DurationMS: pcmBytes * 1000 / (SampleRate * 2),
	}}
}
