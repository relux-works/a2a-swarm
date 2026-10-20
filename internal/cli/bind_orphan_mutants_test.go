package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Bounded, opt-in plants execute the real dispatch/terminal behavioral suite
// in disposable source copies. Every plant keeps its gate and admits only a
// narrower forbidden class. Compile/setup/timeout failures are never kills;
// the specified failing assertion and nearby valid positive must both run.
func TestTerminalOrphanNarrowingPlants(t *testing.T) {
	if os.Getenv("A2A_RUN_TERMINAL_ORPHAN_MUTANTS") != "1" {
		t.Skip("opt-in bounded source controls")
	}
	type plant struct{ name, file, old, next, killer, positive string }
	const admission = "TestTerminalDispatchIdleQualification"
	plants := []plant{
		{"timestamp-short-hour-exemption", "cli/bind_notice.go", `if !noticeTimestampGrammar.MatchString(value) {`, `if len(value)==19 && value[10]=='T' && noticeTimestampGrammar.MatchString(value[:11]+"0"+value[11:]) {value=value[:11]+"0"+value[11:]}; if !noticeTimestampGrammar.MatchString(value) {`, "TestReviewCR4TimestampGrammar/since/one-digit-hour", "TestReviewCR4TimestampGrammar/since/valid-z"},
		{"timestamp-short-hour-generated", "cli/bind_notice.go", `if !noticeTimestampGrammar.MatchString(value) {`, `if len(value)==19 && value[10]=='T' && noticeTimestampGrammar.MatchString(value[:11]+"0"+value[11:]) {value=value[:11]+"0"+value[11:]}; if !noticeTimestampGrammar.MatchString(value) {`, "TestNoticeTimestampGeneratedGrammar/observed/short-component-0539", "TestNoticeTimestampGeneratedGrammar/observed/valid-0000"},
		{"timestamp-comma-fraction-exemption", "cli/bind_notice.go", `if !noticeTimestampGrammar.MatchString(value) {`, `if !noticeTimestampGrammar.MatchString(value) && !noticeTimestampGrammar.MatchString(strings.ReplaceAll(value,",",".")) {`, "TestReviewCR4LocalTimestampGrammar/observed/comma-fraction", "TestReviewCR4LocalTimestampGrammar/observed/valid-fraction-offset"},
		{"timestamp-subnano-order-exemption", "cli/bind_notice.go", `if left != right {
			return left < right
		}`, `if left != right {return i<9 && left<right}`, "TestNoticeTimestampFractionOrder/reversed", "TestNoticeTimestampFractionOrder/forward"},

		{"consumer-unknown-publication-exemption", "cli/bind_notice.go", `a.publishTerminalNotice(ctx, s, generation)
		if consumer {`, `if !consumer {a.publishTerminalNotice(ctx, s, generation)}
  if consumer {`, "TestReviewCR3ConsumerUnknownPublication", "TestTerminalNoticePublicStatusAndWho"},
		{"dispatch-unknown-publication-exemption", "cli/bind_notice.go", `a.publishTerminalNotice(ctx, s, generation)
		if consumer {`, `if consumer {a.publishTerminalNotice(ctx, s, generation)}
  if consumer {`, "TestNoticeErrorPublication/dispatch", "TestNoticeErrorPublication/consumer"},
		{"notice-invalid-reason-exemption", "cli/bind_notice.go", `func noticeValid(n config.OwnNoticeObservation) error {`, `func noticeValid(n config.OwnNoticeObservation) error { if n.Reason=="UNBOUNDED_UNVERIFIED_REASON" {return nil}`, "TestReviewCR3WhoNoticeSchema/invalid-reason", "TestReviewCR3WhoNoticeSchema/valid"},
		{"peer-observed-newline-exemption", "cli/bind_notice.go", `if !noticeTimestampGrammar.MatchString(value) {`, `if len(value)>20 && value[20:]=="\nmallory\tworking\tfresh" {value=value[:20]}; if !noticeTimestampGrammar.MatchString(value) {`, "TestNoticePeerReadSchema/newline-observed", "TestNoticePeerReadSchema/true"},
		{"local-observed-newline-exemption", "cli/bind_notice.go", `if !noticeTimestampGrammar.MatchString(value) {`, `if len(value)>20 && value[20:]=="\nmallory\tworking\tfresh" {value=value[:20]}; if !noticeTimestampGrammar.MatchString(value) {`, "TestNoticeLocalSchemaBoundaries/newline-observed", "TestNoticeLocalSchemaBoundaries/true"},

		{"partial-untyped-reconcile-exemption", "cli/bind.go", `observation == "empty" && b.Typed && len(b.AttemptIDs) < b.NoticeCount`, `observation == "empty" && (b.Typed || b.NoticeCount==2) && len(b.AttemptIDs) < b.NoticeCount`, "TestTerminalUntypedPartialReceipt", admission + "/none"},
		{"first-glimpse-owned-session", "cli/bind.go", `if !qualified {`, `if !qualified && !(previous == nil && b.SessionID == "owned-thread" && len(b.Reservations) == 0) {`, admission + "/none", "TestTerminalNoticeDetection/partial"},
		{"same-time-admission", "cli/bind.go", `!now.Before(previous.At.Add(b.Interval))`, `!now.Before(previous.At)`, admission + "/none", "TestTerminalNoticeDetection/partial"},
		{"busy-reset-omission", "cli/bind.go", `b.Idle = nil; b.Outcome = "suppressed: busy, draft or unknown input"`, `if b.SessionID != "owned-thread" { b.Idle = nil }; b.Outcome = "suppressed: busy, draft or unknown input"`, admission + "/busy", "TestTerminalNoticeDetection/partial"},
		{"daemon-birth-continuity", "cli/bind.go", `DaemonBirth: b.DaemonBirth, Target:`, `DaemonBirth: func() string { if b.SessionID == "owned-thread" { return "" }; return b.DaemonBirth }(), Target:`, admission + "/daemon", "TestTerminalNoticeDetection/partial"},
		{"pause-pulse-reset-omission", "cli/hook.go", `if cur.Terminal != nil {`, `if cur.Terminal != nil && cur.Terminal.SessionID != "owned-thread" {`, "TestTerminalDispatchQualificationInterruption/pause-pulse", admission + "/none"},
		{"manual-pulse-reset-exemption", "cli/liveness.go", `if st.Terminal != nil && manualWakeGate(*st) != "" {`, `if st.Terminal != nil && manualWakeGate(*st) != "" && st.Terminal.SessionID != "owned-thread" {`, "TestTerminalDispatchQualificationInterruption/manual-pulse", admission + "/none"},
		{"unwritable-idle-reset-exemption", "cli/bind.go", `qualificationInterrupted = true`, `qualificationInterrupted = policy.SessionID != "owned-thread"`, "TestTerminalRunAuthorityReadFailure", admission + "/none"},
		{"recovery-first-glimpse-count-one", "cli/bind.go", `if qualified, err := a.qualifyTerminalIdle(s, generation, b); err != nil || !qualified {`, `if qualified, err := a.qualifyTerminalIdle(s, generation, b); err != nil || !qualified && b.NoticeCount != 1 {`, "TestReviewerFreshRecoveryQualification", admission + "/none"},
		{"detect-partial-as-orphan", "cli/bind_notice.go", `len(b.AttemptIDs) == 0`, `len(b.AttemptIDs) <= 1`, "TestTerminalNoticeDetection/partial", admission + "/none"},
		{"detect-unowned-draft", "cli/bind_notice.go", `b.Typed && b.NoticeCount > 0`, `(b.Typed && b.NoticeCount > 0 || !b.Typed && b.NoticeCount == 0)`, "TestTerminalNoticeDetection/no-journal", admission + "/none"},
		{"unknown-silently-clear", "cli/bind_notice.go", `n.PossiblyOrphaned = nil`, `n.PossiblyOrphaned = nil; if n.Since!="" { v:=false; n.PossiblyOrphaned=&v }`, "TestTerminalNoticeDetection/suspected", admission + "/none"},
		{"since-refreshed-count-one", "cli/bind_notice.go", `if n.Since == "" {`, `if n.Since == "" || b.NoticeCount==1 {`, "TestTerminalNoticeDetection/suspected", admission + "/none"},
		{"empty-reconcile-count-one-omission", "cli/bind.go", `observation == "empty" && b.Typed && len(b.AttemptIDs) < b.NoticeCount`, `observation == "empty" && b.Typed && len(b.AttemptIDs) < b.NoticeCount && b.NoticeCount!=1`, "TestTerminalNoticeDetection/suspected", admission + "/none"},
		{"foreign-bob-key-exemption", "cli/bind_notice.go", `if !ok || name != s.name {`, `if !ok || name != s.name && name!="bob" {`, "TestTerminalNoticePublicationRefusals/foreign", admission + "/none"},
		{"publication-error-suspected-exemption", "cli/bind_notice.go", `return fmt.Errorf("notice publication unavailable: %w", err)`, `if record.OwnNotice.PossiblyOrphaned==nil || !*record.OwnNotice.PossiblyOrphaned {return fmt.Errorf("notice publication unavailable: %w", err)}`, "TestTerminalNoticePublicationRefusals/failed", admission + "/none"},
		{"publication-refreshes-heartbeat", "cli/bind_notice.go", `record.OwnNotice = st.Terminal.OwnNotice`, `record.OwnNotice = st.Terminal.OwnNotice; if record.OwnNotice.PossiblyOrphaned!=nil && *record.OwnNotice.PossiblyOrphaned {record.HeartbeatAt=s.app.Now()}`, "TestTerminalNoticePublicStatusAndWho", admission + "/none"},
		{"bind-status-hides-suspected", "cli/bind_notice.go", `n = st.Terminal.OwnNotice`, `n = st.Terminal.OwnNotice; if n.PossiblyOrphaned!=nil && *n.PossiblyOrphaned {v:=false;n.PossiblyOrphaned=&v}`, "TestTerminalNoticePublicStatusAndWho", admission + "/none"},
		{"resume-count-one-exemption", "tmuxwake/terminal.go", `return "suppressed: manual recovery required", nil`, `if count==1 {return t.send(ctx,count,notice,nil,nil)}; return "suppressed: manual recovery required", nil`, "TestTerminalResumeNeverTypesEmpty", "TestTerminalTwoPhaseRaceControls/codex/none"},
		{"erase-count-one-exemption", "tmuxwake/terminal.go", `return false, nil
}

// ObserveInput`, `if count==1 { _,err:=t.command(ctx,"send-keys","-t",t.Pane,"-N",strconv.Itoa(len(notice)),"BSpace"); return true,err }; return false,nil
}

// ObserveInput`, "TestTerminalClearFreshFence/valid", "TestTerminalTwoPhaseRaceControls/codex/none"},
		{"live-space-lookalike-recovery", "cli/bind.go", `if input != "empty" {`, `if input=="nonempty" && b.Typed && b.NoticeCount==1 { if err:=exec.CommandContext(ctx,"tmux","-S",b.Target.Socket,"send-keys","-t",b.Target.Pane,"Enter").Run();err!=nil{return err} }; if input != "empty" {`, "TestRev3PaddedDraftEffects/codex/live/space-left", admission + "/none"},
		{"orphan-space-lookalike-erase", "cli/bind.go", `observed = true`, `if observation=="nonempty" && b.Typed && len(b.AttemptIDs)==0 && b.NoticeCount==1 { if err:=exec.CommandContext(ctx,"tmux","-S",b.Target.Socket,"send-keys","-t",b.Target.Pane,"-N",strconv.Itoa(len(b.Notice)),"BSpace").Run();err!=nil{return err} }; observed = true`, "TestRev3PaddedDraftEffects/claude/orphan/space-left", admission + "/none"},
		{"pause-owned-session-exemption", "cli/bind.go", `if st.HooksPaused || manualWakeGate(st) != "" {`, `if st.HooksPaused && b.SessionID!="owned-thread" || manualWakeGate(st) != "" {`, admission + "/paused", "TestTerminalNoticeDetection/partial"},
		{"manual-human-exemption", "cli/bind.go", `if st.HooksPaused || manualWakeGate(st) != "" {`, `if st.HooksPaused || manualWakeGate(st) != "" && st.Liveness.State!="waiting-human" {`, admission + "/manual", "TestTerminalNoticeDetection/partial"},
		{"native-owned-session-exemption", "cli/bind.go", `if st.Runtime != nil && !st.Runtime.Retired {`, `if st.Runtime != nil && !st.Runtime.Retired && b.SessionID!="owned-thread" {`, admission + "/native", "TestTerminalNoticeDetection/partial"},
		{"lease-foreign-holder-exemption", "cli/bind.go", `l.Holder != b.SessionID`, `l.Holder != b.SessionID && l.Holder!="another-session"`, "TestTerminalFreshAuthorityGates/foreign-lease", admission + "/none"},
		{"binding-owner-one-exemption", "cli/bind.go", `if !ownTerminalTarget(b, b.Target) {`, `if !ownTerminalTarget(b, b.Target) && b.OwnerPID!=b.Target.OwnerPID+1 {`, "TestTerminalFreshAuthorityGates/binding-owner", admission + "/none"},
		{"rate-one-exemption", "cli/bind.go", `if len(reservations) >= b.MaxWakes {`, `if len(reservations) >= b.MaxWakes && b.MaxWakes!=1 {`, "TestTerminalFreshAuthorityGates/rate", admission + "/none"},
		{"typed-fence-detection-exemption", "cli/bind_notice.go", `b.Typed && b.NoticeCount > 0`, `(b.Typed || b.NoticeCount==1) && b.NoticeCount > 0`, "TestTerminalNoticeDetection/untyped", admission + "/none"},
		{"journal-count-two-exemption", "cli/bind_notice.go", `b.Notice != tmuxwake.Notice(b.NoticeCount)`, `b.Notice != tmuxwake.Notice(b.NoticeCount) && b.NoticeCount!=2`, "TestTerminalNoticeDetection/count", admission + "/none"},
		{"fully-pruned-one-omitted", "cli/bind_notice.go", `len(b.AttemptIDs) == 0`, `len(b.AttemptIDs) == 0 && b.NoticeCount!=1`, "TestTerminalNoticeDetection/suspected", admission + "/none"},
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plants {
		t.Run(p.name, func(t *testing.T) {
			copyRoot := t.TempDir()
			for _, name := range []string{"go.mod", "go.sum", "internal", "cmd"} {
				err := filepath.WalkDir(filepath.Join(root, name), func(path string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					rel, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					dst := filepath.Join(copyRoot, rel)
					if d.IsDir() {
						return os.MkdirAll(dst, 0700)
					}
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					return os.WriteFile(dst, data, 0600)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(copyRoot, "internal", p.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			start := 0
			if p.name == "pause-owned-session-exemption" || p.name == "manual-human-exemption" || p.name == "native-owned-session-exemption" {
				start = strings.Index(string(data), "func (a *App) dispatchTerminal(")
			}
			if start < 0 {
				t.Fatal("dispatch anchor missing")
			}
			prefix := string(data[:start])
			data = data[start:]
			if !strings.Contains(string(data), p.old) {
				t.Fatal("mutation anchor missing")
			}
			if err := os.WriteFile(path, []byte(prefix+strings.Replace(string(data), p.old, p.next, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			if p.name == "busy-reset-omission" {
				extra := filepath.Join(copyRoot, "internal", "cli", "bind_notice.go")
				raw, err := os.ReadFile(extra)
				if err != nil {
					t.Fatal(err)
				}
				old := "current.Idle = nil"
				if !strings.Contains(string(raw), old) {
					t.Fatal("observation reset anchor missing")
				}
				if err := os.WriteFile(extra, []byte(strings.Replace(string(raw), old, `if b.SessionID!="owned-thread" {current.Idle=nil}`, 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			mask := func(name string) string {
				parts := strings.Split(name, "/")
				for i := range parts {
					parts[i] = "^" + parts[i] + "$"
				}
				return strings.Join(parts, "/")
			}
			pkg := "./internal/cli"
			if strings.HasPrefix(p.file, "tmuxwake/") {
				pkg = "./internal/tmuxwake"
			}
			cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", pkg, "-run", mask(p.killer))
			cmd.Dir = copyRoot
			cmd.Env = append(os.Environ(), "A2A_RUN_TERMINAL_ORPHAN_MUTANTS=", "A2A_ALITE_SENTINEL_RUN=", "A2A_PHASE1_SENTINEL_RUN=", "A2A_MATRIX_IT=", "A2A_RUN_KEYCHAIN_MUTANTS=")
			out, runErr := cmd.CombinedOutput()
			pos := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", pkg, "-run", mask(p.positive))
			pos.Dir = copyRoot
			pos.Env = cmd.Env
			posOut, posErr := pos.CombinedOutput()
			out = append(out, posOut...)
			if posErr != nil {
				t.Fatalf("positive failed: %v\n%s", posErr, posOut)
			}
			exit := -1
			if ee, ok := runErr.(*exec.ExitError); ok {
				exit = ee.ExitCode()
			} else if runErr == nil {
				exit = 0
			}
			killed, positive, assertion := false, false, false
			intended := ""
			switch strings.Split(p.killer, "/")[0] {
			case "TestNoticeTimestampGeneratedGrammar":
				intended = "timestamp grammar class=short-component"
			case "TestReviewCR4TimestampGrammar":
				intended = "non-RFC3339 timestamp admitted as fresh factual suspicion"
			case "TestReviewCR4LocalTimestampGrammar":
				intended = "non-RFC3339 local timestamp rendered as factual suspicion"
			case "TestNoticeTimestampFractionOrder":
				intended = "fraction ordering admitted reversed evidence"

			case "TestReviewCR3ConsumerUnknownPublication":
				intended = "consumer failure leaves who advertising suspected while bind reports unknown"
			case "TestNoticeErrorPublication":
				intended = "error unknown not published"
			case "TestReviewCR3WhoNoticeSchema":
				intended = "invalid notice facts reported as fresh suspected state"
			case "TestNoticePeerReadSchema":
				intended = "peer notice payload escaped attribution"
			case "TestNoticeLocalSchemaBoundaries":
				intended = "malformed local notice published"
			case "TestTerminalUntypedPartialReceipt":
				intended = "partial uncertain attempt lost retry fence or reused stale count"
			case "TestTerminalDispatchIdleQualification":
				intended = "unqualified idle typed or reserved"
			case "TestTerminalDispatchQualificationInterruption":
				intended = "interruption retained idle authority"
			case "TestTerminalRunAuthorityReadFailure":
				intended = "restored authority reused stale idle observation"
			case "TestReviewerFreshRecoveryQualification":
				intended = "first recovery idle glimpse typed/reserved"
			case "TestTerminalFreshAuthorityGates":
				intended = "fresh authority gate admitted"
			case "TestTerminalNoticeDetection":
				intended = "notice detection mismatch"
				if p.name == "since-refreshed-count-one" {
					intended = "since changed without fresh clear"
				}
				if p.name == "empty-reconcile-count-one-omission" {
					intended = "empty clear spent first glimpse"
				}
			case "TestTerminalNoticePublicationRefusals":
				intended = "failed publication claimed visibility"
				if p.name == "foreign-bob-key-exemption" {
					intended = "foreign publication attempted write"
				}
			case "TestTerminalNoticePublicStatusAndWho":
				intended = "publication changed heartbeat/fields"
				if p.name == "bind-status-hides-suspected" {
					intended = "bind status missing"
				}
			case "TestTerminalResumeNeverTypesEmpty":
				intended = "owned Resume not refused"
			case "TestTerminalClearFreshFence":
				intended = "renderer authorized erase"
			case "TestRev3PaddedDraftEffects":
				intended = "ambiguous space suffix touched"
			}
			decoder := json.NewDecoder(bytes.NewReader(out))
			for decoder.More() {
				var event struct{ Action, Test, Output string }
				if decoder.Decode(&event) != nil {
					break
				}
				if event.Test == p.killer && event.Action == "fail" {
					killed = true
				}
				if event.Test == p.killer && event.Action == "output" && strings.Contains(event.Output, intended) {
					assertion = true
				}
				if event.Test == p.positive && event.Action == "pass" {
					positive = true
				}
			}
			if ctx.Err() != nil || exit != 1 || !killed || !positive || !assertion {
				t.Fatalf("SURVIVOR/unproved plant=%s exit=%d killed=%t positive=%t intended_assertion=%t error=%v\n%s", p.name, exit, killed, positive, assertion, runErr, out)
			}
			t.Logf("KILLED plant=%s gate_exit=%d killer=%s positive=%s\n%s", p.name, exit, p.killer, p.positive, out)
		})
	}
}
