package tmuxwake

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var paneRE = regexp.MustCompile(`^%[0-9]+$`)

var ErrTargetGone = errors.New("pinned target exited or replaced")

var ErrEnterFenceTimeout = errors.New("terminal recovery fence deadline expired")

type Target struct {
	Socket     string `json:"socket"`
	SocketID   string `json:"socket_id"`
	Pane       string `json:"pane"`
	PanePID    int    `json:"pane_pid"`
	PaneBirth  string `json:"pane_birth"`
	OwnerPID   int    `json:"owner_pid"`
	OwnerBirth string `json:"owner_birth"`
	Runtime    string `json:"runtime"`
}

func Birth(ctx context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid process")
	}
	b, err := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=", "-o", "uid=").Output()
	f := strings.Fields(string(b))
	if err != nil || len(f) != 6 || f[5] != strconv.Itoa(os.Getuid()) {
		return "", errors.New("own process unavailable")
	}
	return strings.Join(f, " "), nil
}

func SocketID(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("absolute tmux socket required")
	}
	st, err := os.Lstat(path)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		return "", errors.New("tmux socket unavailable")
	}
	return socketID(st)
}

// tmuxCommandTimeout bounds pinned-target commands. Production keeps this
// default; serial tests may override it for a deliberately delayed wrapper.
var tmuxCommandTimeout = 3 * time.Second

func (t Target) command(ctx context.Context, args ...string) (string, error) {
	if !paneRE.MatchString(t.Pane) {
		return "", errors.New("exact tmux pane id required")
	}
	ctx, cancel := context.WithTimeout(ctx, tmuxCommandTimeout)
	defer cancel()
	b, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", t.Socket}, args...)...).Output()
	if err != nil {
		return "", errors.New("tmux operation unavailable")
	}
	return string(b), nil
}

func (t Target) Inspect(ctx context.Context) (int, int, int, error) {
	if t.OwnerPID > 0 {
		birth, err := Birth(ctx, t.OwnerPID)
		if err == nil && birth != t.OwnerBirth {
			return 0, 0, 0, ErrTargetGone
		}
		if err != nil {
			return 0, 0, 0, errors.New("runtime process replaced")
		}
	}
	id, err := SocketID(t.Socket)
	if err == nil && t.SocketID != "" && id != t.SocketID {
		return 0, 0, 0, ErrTargetGone
	}
	if err != nil {
		if _, statErr := os.Lstat(t.Socket); errors.Is(statErr, os.ErrNotExist) {
			return 0, 0, 0, ErrTargetGone
		}
	}
	if err != nil {
		return 0, 0, 0, errors.New("tmux socket replaced")
	}
	b, err := t.command(ctx, "display-message", "-p", "-t", t.Pane, "#{pane_id} #{pane_pid} #{pane_dead} #{cursor_x} #{cursor_y} #{pane_width}")
	f := strings.Fields(b)
	if err == nil && len(f) == 6 && (f[0] != t.Pane || f[2] != "0") {
		return 0, 0, 0, ErrTargetGone
	}
	if err != nil || len(f) != 6 {
		return 0, 0, 0, errors.New("tmux target unavailable")
	}
	pid, _ := strconv.Atoi(f[1])
	x, _ := strconv.Atoi(f[3])
	y, _ := strconv.Atoi(f[4])
	width, _ := strconv.Atoi(f[5])
	if t.PanePID != 0 {
		birth, err := Birth(ctx, pid)
		if pid != t.PanePID || err == nil && birth != t.PaneBirth {
			return 0, 0, 0, ErrTargetGone
		}
		if err != nil {
			return 0, 0, 0, errors.New("tmux pane process replaced")
		}
	}
	return x, y, width, nil
}

func Pin(ctx context.Context, socket, pane, runtime string) (Target, error) {
	t := Target{Socket: socket, Pane: pane, Runtime: runtime}
	if runtime != "claude" && runtime != "codex" {
		return t, errors.New("runtime must be claude or codex")
	}
	id, err := SocketID(socket)
	if err != nil {
		return t, err
	}
	t.SocketID = id
	b, err := t.command(ctx, "display-message", "-p", "-t", pane, "#{pane_id} #{pane_pid} #{pane_dead}")
	f := strings.Fields(b)
	if err != nil || len(f) != 3 || f[0] != pane || f[2] != "0" {
		return t, errors.New("tmux target unavailable")
	}
	t.PanePID, _ = strconv.Atoi(f[1])
	t.PaneBirth, err = Birth(ctx, t.PanePID)
	return t, err
}

func (t Target) box(ctx context.Context) (Box, bool) {
	return t.boxWithNotice(ctx, "")
}

func (t Target) boxWithNotice(ctx context.Context, notice string) (Box, bool) {
	x, y, _, err := t.Inspect(ctx)
	if err != nil {
		return Box{}, false
	}
	screen, err := t.command(ctx, "capture-pane", "-e", "-p", "-t", t.Pane)
	if err != nil {
		return Box{}, false
	}
	b, ok := inputWithNotice(screen, t.Runtime, notice)
	// Placeholder stripping is only idle evidence. Nonempty ownership must
	// account for the complete prompt, including dim/grey prefix or suffix.
	if ok && b.Text != "" && !completeInput(screen, b) {
		return Box{}, false
	}
	return b, ok && (notice != "" && b.Text == notice || x == b.Column && y == b.Row)
}

func completeInput(screen string, box Box) bool {
	lines := strings.Split(strings.TrimSuffix(screen, "\n"), "\n")
	// Captures pad with ASCII spaces only. Trimming Unicode whitespace would
	// turn a dim NBSP suffix into apparently exact journal-owned input.
	line := strings.TrimLeft(strings.TrimRight(plain(lines[box.Row]), " "), " ")
	return line == "› "+box.Text || line == "❯ "+box.Text || line == "❯\u00a0"+box.Text
}

func Notice(count int) string { return fmt.Sprintf("a2a: %d new peer events; run a2a inbox", count) }

func (t Target) Idle(ctx context.Context) bool { b, ok := t.box(ctx); return ok && b.Text == "" }

// PendingInput retains the old empty/blocked interface. Renderer-only
// nonempty input, including a journal lookalike, never claims ownership.
func (t Target) PendingInput(ctx context.Context, notice string) string {
	b, ok := t.boxWithNotice(ctx, notice)
	if !ok {
		return "blocked"
	}
	if b.Text == "" {
		return "empty"
	}
	if notice != "" && b.Text == notice {
		return "blocked"
	}
	return "blocked"
}

// Legacy Resume and ClearNotice refuse renderer-only recovery. The bound
// consumer uses ResumeNotice with an independently fenced typing origin.
func (t Target) Resume(ctx context.Context, count int, notice string) (string, error) {
	return "suppressed: manual recovery required", nil
}

func (t Target) ClearNotice(ctx context.Context, count int, notice string, beforeErase func() error) (bool, error) {
	return false, nil
}

// ObserveInput reports rendered emptiness/nonemptiness, never exact ownership.
// Unknown layouts, busy screens and failed target/capture reads remain unknown.
func (t Target) ObserveInput(ctx context.Context, notice string) string {
	x, y, _, err := t.Inspect(ctx)
	if err != nil {
		return "unknown"
	}
	screen, err := t.command(ctx, "capture-pane", "-e", "-p", "-t", t.Pane)
	if err != nil {
		return "unknown"
	}
	b, ok := inputWithNotice(screen, t.Runtime, notice)
	if !ok {
		return "unknown"
	}
	if b.Text != "" {
		return "nonempty"
	}
	if x != b.Column || y != b.Row {
		return "unknown"
	}
	return "empty"
}

// Send is two-phase, with no Enter on mismatch. The caller reserves its event
// IDs durably before calling and owns bounded recovery of ambiguous attempts. Keystrokes
// between verification and fresh-delivery Enter remain an explicit bound.
func (t Target) Send(ctx context.Context, count int, beforeType, afterType func()) (string, error) {
	return t.send(ctx, count, "", beforeType, afterType)
}

func (t Target) send(ctx context.Context, count int, owned string, beforeType, afterType func()) (string, error) {
	if count <= 0 {
		return "", errors.New("positive count required")
	}
	result, err := t.sendWith(ctx, count, owned, beforeType, afterType, t.boxWithNotice, nil)
	return result.Text(), err
}

// The bound adapter shares the established two-phase effect implementation;
// only its observation and additional pre-type constraint differ.
func (t Target) sendWith(ctx context.Context, count int, owned string, beforeType, afterType func(), inspect func(context.Context, string) (Box, bool), preType func() bool, beforeEnter ...func() error) (SendResult, error) {
	if count <= 0 {
		return SendResult{Disposition: SendRefused, Outcome: SendSuppressedInput}, errors.New("positive count required")
	}
	box, ok := inspect(ctx, owned)
	if !ok || box.Text != "" && box.Text != owned {
		return SendResult{Disposition: SendRefused, Outcome: SendSuppressedInput}, nil
	}
	// Resume is continuation only. If the owned text vanished after the
	// caller's observation, a fresh type needs separate qualified admission.
	if owned != "" && box.Text == "" {
		return SendResult{Disposition: SendRefused, Outcome: SendSuppressedInput}, nil
	}
	notice := Notice(count)
	_, _, width, err := t.Inspect(ctx)
	if err != nil || len(notice)+3 >= width {
		return SendResult{Disposition: SendRefused, Outcome: SendSuppressedWidth}, nil
	}
	if box.Text == "" {
		if beforeType != nil {
			beforeType()
		}
		if preType != nil && !preType() {
			return SendResult{Disposition: SendRefused, Outcome: SendSuppressedInput}, nil
		}
		if _, err = t.command(ctx, "send-keys", "-t", t.Pane, "-l", notice); err != nil {
			return SendResult{Disposition: SendUncertain, Outcome: SendTypeUncertain}, err
		}
		if afterType != nil {
			afterType()
		}
	}
	// The redraw may lag key delivery, and a background-priority or busy
	// terminal can take far longer than one fixed delay (#89). Polling does
	// not submit anything: Enter still requires one capture showing exactly
	// the notice. An empty box or a strict prefix of the notice is a lagging
	// redraw and keeps waiting; any other recognized text is foreign input and
	// stops. A layout that cannot be read (a filled Codex composer holding
	// anything but the exact notice, a dialog, a failed capture) also keeps
	// waiting, and ends as unavailable at the deadline.
	deadline := time.NewTimer(redrawWait)
	defer deadline.Stop()
	tick := time.NewTicker(redrawPoll)
	defer tick.Stop()
	outcome := SendDraftUnavailable
	for {
		select {
		case <-ctx.Done():
			return SendResult{Disposition: SendUncertain, Outcome: SendInterrupted}, ctx.Err()
		case <-deadline.C:
			return SendResult{Disposition: SendDraft, Outcome: outcome}, nil
		case <-tick.C:
		}
		box, ok = inspect(ctx, notice)
		if !ok {
			outcome = SendDraftUnavailable
			continue
		}
		if box.Text == notice {
			for _, fence := range beforeEnter {
				if err := fence(); err != nil {
					reason := ReasonRecoveryHeld
					if errors.Is(err, ErrEnterFenceTimeout) {
						reason = ReasonFenceTimeout
					}
					return SendResult{Disposition: SendRefused, Outcome: SendSuppressedInput, Reason: reason, ReasonAt: time.Now().UTC()}, err
				}
			}
			if _, err = t.command(ctx, "send-keys", "-t", t.Pane, "Enter"); err != nil {
				return SendResult{Disposition: SendUncertain, Outcome: SendEnterUncertain}, err
			}
			return SendResult{Disposition: SendSubmitted, Outcome: SendWakeSubmitted}, nil
		}
		outcome = SendDraftChanged
		if !strings.HasPrefix(notice, box.Text) {
			return SendResult{Disposition: SendDraft, Outcome: SendDraftChanged}, nil
		}
	}
}

// redrawPoll and redrawWait bound the post-type verification. Variables only
// so tests can shorten the window; production never changes them.
var (
	redrawPoll = 100 * time.Millisecond
	redrawWait = 2 * time.Second
)
