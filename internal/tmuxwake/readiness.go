package tmuxwake

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

type ReadinessRoute string

const (
	LegacyFallback ReadinessRoute = "legacy_fallback"
	LegacyCodex    ReadinessRoute = "legacy_codex"
	RegistryLegacy ReadinessRoute = "registry_legacy"
	RegistryNamed  ReadinessRoute = "registry_named"
)

// ReadinessKey is comparable but deliberately has no JSON projection. It is
// an additional refusal constraint, never stored permission to send input.
type ReadinessKey struct {
	Route              ReadinessRoute `json:"-"`
	Target             Target         `json:"-"`
	ClaudeRegistryRoot string         `json:"-"`
	Generation         string         `json:"-"`
	SessionID          string         `json:"-"`
	RegistrySessionID  string         `json:"-"`
	RegistryVersion    string         `json:"-"`
	RegistryStatus     string         `json:"-"`
	RegistryStatusAt   int64          `json:"-"`
	ComposerRow        int            `json:"-"`
	ComposerColumn     int            `json:"-"`
	PaneWidth          int            `json:"-"`
}

type ReadinessAssessment struct {
	// InputObserved distinguishes a parser result from an early default reason.
	// It is diagnostic only and never participates in eligibility or a key.
	InputObserved bool              `json:"-"`
	Runtime       RuntimeAssessment `json:"runtime"`
	Input         InputAssessment   `json:"input"`
	Key           ReadinessKey      `json:"-"`

	submitBusy    bool
	emptyComposer bool
	submitKey     ReadinessKey
	foreign       bool
}
type BoundTerminal struct {
	Target             Target
	ClaudeRegistryRoot string
	Generation         string
	SessionID          string
	NoticePosition     *ComposerPosition
	// Muse is the session-message seam of the muse carrier; nil selects
	// the Muse CLI route. It is never read for claude or codex targets.
	Muse MuseSender
}

func (k ReadinessKey) valid() bool {
	if k.Route == MuseSessionRoute {
		// The muse carrier pins owner process, generation and the exact
		// session id; it has no pane, registry or composer geometry.
		return k.Target.Runtime == "muse" && k.Target.OwnerPID > 1 && k.Target.OwnerBirth != "" && k.Target.Pane == "" && k.Target.SocketID == "" && k.Generation != "" && ValidMuseSessionID(k.SessionID) && k.ClaudeRegistryRoot == "" && k.RegistrySessionID == "" && k.RegistryVersion == "" && k.RegistryStatus == "" && k.RegistryStatusAt == 0 && k.ComposerRow == 0 && k.ComposerColumn == 0 && k.PaneWidth == 0
	}
	if k.Target.OwnerPID <= 1 || k.Target.OwnerBirth == "" || k.Target.Pane == "" || k.Target.SocketID == "" || k.Generation == "" || k.SessionID == "" || k.ComposerRow < 0 || k.ComposerColumn < 0 || k.PaneWidth <= 0 || k.ComposerColumn >= k.PaneWidth {
		return false
	}
	noRegistry := k.RegistrySessionID == "" && k.RegistryVersion == "" && k.RegistryStatus == "" && k.RegistryStatusAt == 0
	switch k.Route {
	case LegacyCodex:
		return k.Target.Runtime == "codex" && k.ClaudeRegistryRoot == "" && noRegistry
	case LegacyFallback:
		return k.Target.Runtime == "claude" && noRegistry
	case RegistryLegacy, RegistryNamed:
		return k.Target.Runtime == "claude" && k.ClaudeRegistryRoot != "" && k.RegistrySessionID == k.SessionID && SafeRegistryVersion(k.RegistryVersion) != "" && (k.RegistryStatus == "idle" || k.RegistryStatus == "shell") && k.RegistryStatusAt >= 0
	default:
		return false
	}
}
func (r ReadinessAssessment) Eligible() bool {
	return r.Input.State == InputEmpty && r.Input.Reason == InputReasonNone && r.runtimeEligible()
}

// Both empty admission and the current invocation's exact typed notice must
// retain the same runtime route. Owning notice bytes never bypasses refusal.
func (r ReadinessAssessment) runtimeEligible() bool {
	if !r.Key.valid() || !ValidRuntimeReason(r.Runtime.Reason) || r.Runtime.State != RuntimeStateFor(r.Runtime.Reason) {
		return false
	}
	switch r.Key.Route {
	case LegacyCodex, RegistryLegacy, RegistryNamed, MuseSessionRoute:
		return r.Runtime.State == RuntimeReady && r.Runtime.Reason == RuntimeNone
	case LegacyFallback:
		switch r.Runtime.Reason {
		case RuntimeRootUnavailable, RuntimeRegistryUnavailable, RuntimeVersionUnqualified, RuntimeSchemaUnqualified:
			return true
		}
	}
	return false
}

// submitWitnessFor ties a post-Enter empty composer to the same pinned runtime
// identity. Composer coordinates may move after submission, so only those two
// fields are normalized against the invocation's expected key.
func (r ReadinessAssessment) submitWitnessFor(expected ReadinessKey) bool {
	if !r.emptyComposer || !r.submitKey.valid() {
		return false
	}
	r.Key = r.submitKey
	return r.runtimeEligible() && sameKeyAfterOwnedTyping(r.submitKey, expected)
}

func (t BoundTerminal) registry(ctx context.Context) registryAssessment {
	if t.ClaudeRegistryRoot == "" {
		return registryAssessment{RuntimeAssessment: runtimeResult(RuntimeRootUnavailable, "")}
	}
	id, err := liveRegistryIdentity(ctx, t.Target, t.SessionID)
	if err != nil {
		return registryAssessment{RuntimeAssessment: runtimeResult(RuntimeTargetUnavailable, "")}
	}
	data, err := registryBytes(t.ClaudeRegistryRoot, t.Target.OwnerPID)
	if err != nil {
		return registryAssessment{RuntimeAssessment: runtimeResult(RuntimeRegistryUnavailable, "")}
	}
	after, err := liveRegistryIdentity(ctx, t.Target, t.SessionID)
	if err != nil {
		return registryAssessment{RuntimeAssessment: runtimeResult(RuntimeTargetUnavailable, "")}
	}
	if after != id {
		return registryAssessment{RuntimeAssessment: runtimeResult(RuntimeIdentityMismatch, "")}
	}
	return decodeRegistry(data, id)
}

// A named top rule is input geometry only. This helper is reachable solely
// behind a qualified registry. Busy/dialog checks still use the complete tail.
func boundInput(screen, runtime, owned string, named bool) (Box, InputReason, bool) {
	return boundInputUsing(screen, runtime, owned, named, inputWithNoticeReason)
}

func boundInputUsing(screen, runtime, owned string, named bool, parse func(string, string, string) (Box, InputReason)) (Box, InputReason, bool) {
	b, reason := parse(screen, runtime, owned)
	if reason != InputReasonLayoutUnrecognized || runtime != "claude" || !named {
		return b, reason, false
	}
	lines := screenLines(screen)
	for i := len(lines) - 1; i >= max(2, len(lines)-18); i-- {
		if !rule.MatchString(strings.TrimSpace(plain(lines[i]))) {
			continue
		}
		top := strings.TrimSpace(plain(lines[i-2]))
		prefix := 0
		for _, r := range top {
			if r != '─' {
				break
			}
			prefix++
		}
		if prefix < 5 {
			continue
		}
		label := strings.TrimSpace(strings.TrimLeft(top, "─"))
		if label == "" || utf8.RuneCountInString(label) > 128 {
			continue
		}
		valid := true
		for _, r := range label {
			if unicode.IsControl(r) || r == '❯' || r == '›' {
				valid = false
			}
		}
		if !valid {
			continue
		}
		copyLines := append([]string(nil), lines...)
		copyLines[i-2] = "────────"
		b, reason = parse(strings.Join(copyLines, "\n"), runtime, owned)
		return b, reason, true
	}
	return Box{}, InputReasonLayoutUnrecognized, false
}

// emptyComposerWitness recognizes a post-submit empty prompt without requiring
// the cursor to remain at its old coordinates. The qualified footer and known
// runtime layout remain mandatory; this is evidence only after Enter.
func emptyComposerWitness(screen, runtime string, named bool) (Box, bool) {
	if _, reason := inputWithNoticeReason(screen, runtime, ""); reason == InputReasonBusyOrDialog {
		return Box{}, false
	}
	if runtime == "claude" {
		box, reason, _ := boundInput(screen, runtime, "", named)
		return box, reason == InputReasonNone && box.Text == ""
	}
	if runtime != "codex" {
		return Box{}, false
	}
	lines := strings.Split(strings.TrimSuffix(screen, "\n"), "\n")
	start := max(0, len(lines)-18)
	for i := len(lines) - 1; i >= start; i-- {
		line := visible(lines[i])
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "›") {
			continue
		}
		if strings.TrimRight(trimmed, " ") != "›" || i+1 >= len(lines) || strings.TrimSpace(plain(lines[i+1])) != "" {
			return Box{}, false
		}
		footer := plain(strings.Join(lines[i+1:], "\n"))
		if !strings.Contains(footer, "? for shortcuts") {
			return Box{}, false
		}
		indent := len(line) - len(trimmed)
		return Box{Row: i, Column: indent + 2}, true
	}
	return Box{}, false
}

func (t BoundTerminal) AssessReadiness(ctx context.Context) ReadinessAssessment {
	r, _ := t.assess(ctx, "")
	return r
}

// ObserveReadiness preserves journal-assisted nonempty diagnostics when the
// filled Codex composer hides its hint. The journal is never current ownership:
// nonempty input receives no key and cannot become eligible for Send.
func (t BoundTerminal) ObserveReadiness(ctx context.Context, diagnosticNotice string) ReadinessAssessment {
	r, _ := t.assessWithNotice(ctx, "", diagnosticNotice)
	return r
}

func (t BoundTerminal) assess(ctx context.Context, owned string) (ReadinessAssessment, Box) {
	return t.assessWithNotice(ctx, owned, "")
}

// One logical assessment comprises several independent reads, not an atomic
// snapshot. The same observation supplies telemetry, parser result and key.
func (t BoundTerminal) assessWithNotice(ctx context.Context, owned, diagnosticNotice string) (ReadinessAssessment, Box) {
	if t.Target.Runtime == "muse" {
		return t.assessMuse(ctx), Box{}
	}
	r := ReadinessAssessment{Runtime: runtimeResult(RuntimeTargetUnavailable, ""), Input: InputAssessment{InputUnavailable, InputReasonTargetUnavailable}}
	x, y, width, err := t.Target.Inspect(ctx)
	if err != nil || ctx.Err() != nil {
		return r, Box{}
	}
	runtime := t.Target.Runtime
	if runtime != "claude" && runtime != "codex" {
		r.Input.Reason = InputReasonLayoutUnrecognized
		return r, Box{}
	}
	before := registryAssessment{RuntimeAssessment: runtimeResult(RuntimeNone, "")}
	if runtime == "claude" {
		before = t.registry(ctx)
	}
	r.Runtime = before.RuntimeAssessment
	r.Input.Reason = InputReasonLayoutUnrecognized
	if before.State == RuntimeRefused {
		r.submitBusy = before.Reason == RuntimeBusy
		return r, Box{}
	}
	screen, err := t.Target.command(ctx, "capture-pane", "-e", "-p", "-t", t.Target.Pane)
	if err != nil {
		r.Input.Reason = InputReasonCaptureUnavailable
		return r, Box{}
	}
	after := before
	if runtime == "claude" {
		after = t.registry(ctx)
	}
	r.Runtime = after.RuntimeAssessment
	r.submitBusy = after.Reason == RuntimeBusy || runtimeBusyMarker(screen, runtime)
	if after.State == RuntimeRefused {
		return r, Box{}
	}
	x2, y2, width2, err := t.Target.Inspect(ctx)
	if err != nil || ctx.Err() != nil {
		r.Runtime = runtimeResult(RuntimeTargetUnavailable, "")
		r.Input.Reason = InputReasonTargetUnavailable
		return r, Box{}
	}
	// A changed registry observation cannot be downgraded into fallback.
	// Reported key names are diagnostic only and never change admission.
	beforeAuthority, afterAuthority := before, after
	beforeAuthority.UnknownKeys, afterAuthority.UnknownKeys = "", ""
	if beforeAuthority != afterAuthority {
		r.Runtime = runtimeResult(RuntimeRegistryUnavailable, after.Version)
		return r, Box{}
	}
	parserNotice := owned
	if parserNotice == "" && fixedNotice.MatchString(diagnosticNotice) {
		parserNotice = diagnosticNotice
	}
	r.foreign = foreignComposerText(screen, runtime, parserNotice)
	b, reason, named := boundInput(screen, runtime, parserNotice, after.State == RuntimeReady)
	r.InputObserved = true
	// A recognized empty composer can witness submission after Enter even when
	// the cursor moved. Keep its runtime identity separately from position keys.
	if emptyBox, ok := emptyComposerWitness(screen, runtime, after.State == RuntimeReady); ok && width2 == width && emptyBox.Row >= 0 && emptyBox.Row < len(strings.Split(strings.TrimSuffix(screen, "\n"), "\n")) && emptyBox.Column >= 0 && emptyBox.Column < width {
		r.emptyComposer = true
		r.submitKey = readinessKey(t, after, emptyBox, width, named)
	}
	r.Input = InputAssessment{InputUnavailable, reason}
	if reason != InputReasonNone {
		return r, Box{}
	}
	lines := strings.Split(strings.TrimSuffix(screen, "\n"), "\n")
	if b.Row < 0 || b.Row >= len(lines) || width <= 0 || b.Column < 0 || b.Column >= width || width2 != width || (parserNotice == "" || b.Text != parserNotice) && (x != b.Column || y != b.Row || x2 != x || y2 != y) {
		r.Input.Reason = InputReasonCursorMismatch
		return r, Box{}
	}
	if b.Text != "" && !completeInput(screen, b) {
		r.Input.Reason = InputReasonLayoutUnrecognized
		return r, Box{}
	}
	if b.Text != "" {
		r.Input = InputAssessment{InputNonempty, InputReasonInputNonempty}
	} else {
		r.Input = InputAssessment{InputEmpty, InputReasonNone}
	}
	// Only an explicitly supplied exact owned notice can get a nonempty key.
	// Consumer recovery must independently fence its generation and journal.
	if b.Text != "" && (owned == "" || b.Text != owned || !fixedNotice.MatchString(owned)) {
		return r, b
	}
	k := readinessKey(t, after, b, width, named)
	if k.valid() {
		r.Key = k
	}
	return r, b
}

func readinessKey(t BoundTerminal, after registryAssessment, box Box, width int, named bool) ReadinessKey {
	k := ReadinessKey{Target: t.Target, ClaudeRegistryRoot: t.ClaudeRegistryRoot, Generation: t.Generation, SessionID: t.SessionID, ComposerRow: box.Row, ComposerColumn: box.Column - len(box.Text), PaneWidth: width}
	if t.Target.Runtime == "codex" {
		k.Route = LegacyCodex
		k.ClaudeRegistryRoot = ""
	} else if after.State == RuntimeReady {
		k.Route = RegistryLegacy
		if named {
			k.Route = RegistryNamed
		}
		k.RegistrySessionID = after.SessionID
		k.RegistryVersion = after.Version
		k.RegistryStatus = after.Status
		k.RegistryStatusAt = after.StatusAt
	} else {
		k.Route = LegacyFallback
	}
	return k
}

func (t BoundTerminal) ObserveInput(ctx context.Context, notice string) string {
	// A saved notice helps classify nonempty text, never supplies ownership.
	r := t.ObserveReadiness(ctx, notice)
	switch r.Input.State {
	case InputNonempty:
		return "nonempty"
	case InputEmpty:
		if r.Eligible() {
			return "empty"
		}
	}
	return "unknown"
}
func (t BoundTerminal) Resume(context.Context, int, string) (string, error) {
	return "suppressed: manual recovery required", nil
}
func (t BoundTerminal) ClearNotice(context.Context, int, string, func() error) (bool, error) {
	return false, nil
}

// AssessOwnedNotice is continuation-only evidence. It never admits fresh typing;
// the caller must tie notice and expected identity to a durable typing attempt.
func (t BoundTerminal) AssessOwnedNotice(ctx context.Context, notice string) ReadinessAssessment {
	r, b := t.assessRecovery(ctx, notice)
	if notice == "" || b.Text != notice {
		r.Key = ReadinessKey{}
	}
	return r
}

// The Codex no-hint exception is safe for continuation only at a position
// actually seen by the typing invocation. Without that record the footer must
// independently anchor the bottommost composer. Diagnostics grant no keys.
func (t BoundTerminal) assessRecovery(ctx context.Context, notice string) (ReadinessAssessment, Box) {
	r, b := t.assess(ctx, notice)
	if b.Text == notice && (t.NoticePosition != nil && b.position() != *t.NoticePosition || t.Target.Runtime == "codex" && t.NoticePosition == nil && !b.FooterHint) {
		r.Key = ReadinessKey{}
		r.Input = InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}
	}
	return r, b
}

// This independent final capture is evidence for a future refusal constraint,
// never Enter admission. Busy/registry transitions can still leave a visible
// exact notice. No record is returned for foreign, wrapped or styled extra input.
func (t BoundTerminal) captureNoticePosition(ctx context.Context, notice string, named bool) *ComposerPosition {
	_, _, width, err := t.Target.Inspect(ctx)
	if err != nil || ctx.Err() != nil {
		return nil
	}
	screen, err := t.Target.command(ctx, "capture-pane", "-e", "-p", "-t", t.Target.Pane)
	if err != nil {
		return nil
	}
	b, reason, _ := boundInputUsing(screen, t.Target.Runtime, notice, named, inputGeometry)
	if reason != InputReasonNone || b.Text != notice || !fixedNotice.MatchString(notice) || b.Column >= width || !completeInput(screen, b) {
		return nil
	}
	p := b.position()
	return &p
}

// OwnedNoticeEligible requires complete exact input and current runtime readiness.
func (r ReadinessAssessment) OwnedNoticeEligible() bool {
	return r.Input.State == InputNonempty && r.runtimeEligible()
}
