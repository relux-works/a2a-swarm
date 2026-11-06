package voice

import (
	"context"
	"errors"
	"fmt"
)

// SynthesisCategory describes only observations made by the local parent.
// In particular, a worker exit does not identify an engine or model cause.
type SynthesisCategory uint8

const (
	SynthesisUnknown SynthesisCategory = iota
	SynthesisValidation
	SynthesisExecutable
	SynthesisCancelled
	SynthesisDeadline
	SynthesisWorkerExit
)

// SynthesisError retains no underlying error, input, path or diagnostics.
// Its private fields and constructor keep both categories and status bounded.
type SynthesisError struct {
	category SynthesisCategory
	status   int
}

func NewSynthesisError(category SynthesisCategory, status int) *SynthesisError {
	if category > SynthesisWorkerExit {
		category = SynthesisUnknown
	}
	if category != SynthesisWorkerExit || status < 1 || status > 255 {
		status = -1
	}
	return &SynthesisError{category: category, status: status}
}

func (e *SynthesisError) Category() SynthesisCategory { return e.category }
func (e *SynthesisError) ExitStatus() (int, bool) {
	return e.status, e.category == SynthesisWorkerExit && e.status >= 1 && e.status <= 255
}

func (e *SynthesisError) Error() string {
	switch e.category {
	case SynthesisValidation:
		return "voice synthesis failed: model/config validation refused"
	case SynthesisExecutable:
		return "voice synthesis failed: executable resolution or spawn refused"
	case SynthesisCancelled:
		return "voice synthesis failed: context cancelled"
	case SynthesisDeadline:
		return "voice synthesis failed: context deadline exceeded"
	case SynthesisWorkerExit:
		if status, ok := e.ExitStatus(); ok {
			return fmt.Sprintf("voice synthesis failed: worker exited (status %d)", status)
		}
		return "voice synthesis failed: worker exited (status unavailable)"
	default:
		return "voice synthesis failed: unclassified failure"
	}
}

func contextFailure(ctx context.Context, err error) *SynthesisError {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return NewSynthesisError(SynthesisDeadline, -1)
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return NewSynthesisError(SynthesisCancelled, -1)
	}
	return nil
}

func synthesisFailure(ctx context.Context, err error) *SynthesisError {
	if failure := contextFailure(ctx, err); failure != nil {
		return failure
	}
	var failure *SynthesisError
	if errors.As(err, &failure) && failure != nil {
		return NewSynthesisError(failure.category, failure.status)
	}
	return NewSynthesisError(SynthesisUnknown, -1)
}

// RenderFailure keeps non-synthesis stages distinct without accepting messages
// from replaceable runners/renderers as safe diagnostics.
type RenderFailure uint8

const (
	ScratchFailure RenderFailure = iota + 1
	OutputFailure
	DecodeFailure
	InvalidPCMFailure
	InvalidNoticeFailure
	PrepareEncodingFailure
	EncodeFailure
	InvalidOGGFailure
	AudioLimitFailure
)

func (f RenderFailure) Error() string {
	switch f {
	case ScratchFailure:
		return "cannot create private voice scratch"
	case OutputFailure:
		return "cannot create private voice output"
	case DecodeFailure:
		return "voice decode failed (ffmpeg with Opus support required)"
	case InvalidPCMFailure:
		return "invalid decoded voice audio"
	case InvalidNoticeFailure:
		return "invalid truncation notice audio"
	case PrepareEncodingFailure:
		return "cannot prepare voice encoding"
	case EncodeFailure:
		return "voice encode failed (ffmpeg with libopus required)"
	case InvalidOGGFailure:
		return "invalid Ogg/Opus voice output"
	case AudioLimitFailure:
		return "voice exceeds 180s cap"
	default:
		return NewSynthesisError(SynthesisUnknown, -1).Error()
	}
}

// SafeRenderError copies only the closed local contract, never err.Error().
// Unknown implementations and wrappers cannot contribute diagnostic text.
func SafeRenderError(err error) error {
	var synthesis *SynthesisError
	if errors.As(err, &synthesis) && synthesis != nil {
		return NewSynthesisError(synthesis.category, synthesis.status)
	}
	var stage RenderFailure
	if errors.As(err, &stage) {
		if stage >= ScratchFailure && stage <= AudioLimitFailure {
			return stage
		}
	}
	return NewSynthesisError(SynthesisUnknown, -1)
}
