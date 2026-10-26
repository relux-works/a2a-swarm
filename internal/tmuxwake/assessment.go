package tmuxwake

import "context"

// InputState is a rendered-input observation, never permission to send keys.
type InputState string

const (
	InputEmpty       InputState = "empty"
	InputNonempty    InputState = "nonempty"
	InputUnavailable InputState = "unavailable"
)

// InputReason contains fixed diagnostic codes; it never contains screen text.
type InputReason string

const (
	InputReasonNone               InputReason = "none"
	InputReasonInputNonempty      InputReason = "input_nonempty"
	InputReasonBusyOrDialog       InputReason = "busy_or_dialog"
	InputReasonLayoutUnrecognized InputReason = "layout_unrecognized"
	InputReasonCursorMismatch     InputReason = "cursor_mismatch"
	InputReasonTargetUnavailable  InputReason = "target_unavailable"
	InputReasonCaptureUnavailable InputReason = "capture_unavailable"
)

// InputAssessment describes one fresh read of an existing qualified layout.
// It carries no text or ownership, and is not two-sample idle qualification.
type InputAssessment struct {
	State  InputState  `json:"state"`
	Reason InputReason `json:"reason"`
}

// AssessInput preserves Idle's target, parser, complete-input and cursor
// requirements. It performs no input or recovery operation. A caller must keep
// its existing authority, qualification and fresh delivery reinspection gates.
func (t Target) AssessInput(ctx context.Context) InputAssessment {
	x, y, _, err := t.Inspect(ctx)
	if err != nil {
		return InputAssessment{InputUnavailable, InputReasonTargetUnavailable}
	}
	screen, err := t.command(ctx, "capture-pane", "-e", "-p", "-t", t.Pane)
	if err != nil {
		return InputAssessment{InputUnavailable, InputReasonCaptureUnavailable}
	}
	return assessInput(screen, t.Runtime, x, y)
}

func assessInput(screen, runtime string, x, y int) InputAssessment {
	b, reason := inputWithNoticeReason(screen, runtime, "")
	if reason != InputReasonNone {
		return InputAssessment{InputUnavailable, reason}
	}
	if b.Text != "" && !completeInput(screen, b) {
		return InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}
	}
	if x != b.Column || y != b.Row {
		return InputAssessment{InputUnavailable, InputReasonCursorMismatch}
	}
	if b.Text != "" {
		return InputAssessment{InputNonempty, InputReasonInputNonempty}
	}
	return InputAssessment{InputEmpty, InputReasonNone}
}
