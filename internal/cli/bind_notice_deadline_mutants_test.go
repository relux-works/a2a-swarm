package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Independent opt-in B1 controls preserve all 40 accepted CR5 plants verbatim.
// A missing network budget is exempted only for otherwise live callers; earlier
// caller deadlines and pre-HTTP state-lock acquisition stay bounded; HTTP must
// leave the state lock available to other writers. Only an intended
// assertion plus a passing actual publication is credited, never a timeout,
// panic, build error, skip, missing test, or cleanup failure.
func TestNoticePublicationDeadlineNarrowingPlants(t *testing.T) {
	isolateBindTest(t)
	if os.Getenv("A2A_RUN_NOTICE_DEADLINE_MUTANTS") != "1" {
		t.Skip("opt-in synthetic B1 controls")
	}
	plants := []struct{ name, old, next, killer, assertion string }{
		{"background-put-no-deadline", "publicationErr := s.putOwnStatus(writeCtx, &snapshot, record)", "if _, hasDeadline := ctx.Deadline(); !hasDeadline { writeCtx = ctx }; publicationErr := s.putOwnStatus(writeCtx, &snapshot, record)", "TestNoticePublicationNetworkBound/put-default", "publication exceeded network bound"},
		{"background-resolve-no-deadline", "s.resolveRoom(ctx, StatusRoom)", "s.resolveRoom(func() context.Context { if d, ok := ctx.Deadline(); ok && time.Until(d)>9*time.Second { return context.WithoutCancel(ctx) }; return ctx }(), StatusRoom)", "TestNoticePublicationNetworkBound/resolve-default", "publication exceeded network bound"},
		{"earlier-lock-budget-exemption", "snapshot, err := config.UpdateStateBy(s.paths, s.name, deadline,", "snapshot, err := config.UpdateStateBy(s.paths, s.name, time.Now().Add(nativeRPCTimeout),", "TestNoticePublicationEarlierLockDeadline", "lock wait ignored earlier caller deadline"},
		{"background-cancel-release-exemption", "defer cancel()", "defer func() { if _, hasDeadline := ctx.Deadline(); hasDeadline { cancel() } }()", "TestNoticePublicationSuccessCancelsBudget", "network budget missing or cancellation resources not released"},
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plants {
		t.Run(p.name, func(t *testing.T) {
			dst := t.TempDir()
			for _, name := range []string{"go.mod", "go.sum", "internal", "cmd"} {
				err := filepath.WalkDir(filepath.Join(root, name), func(path string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					rel, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					target := filepath.Join(dst, rel)
					if d.IsDir() {
						return os.MkdirAll(target, 0700)
					}
					b, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					return os.WriteFile(target, b, 0600)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dst, "internal/cli/own_status.go")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(b), p.old) != 1 {
				t.Fatal("setup: nonunique plant anchor")
			}
			mutated := strings.Replace(string(b), p.old, p.next, 1)
			if err = os.WriteFile(path, []byte(mutated), 0600); err != nil {
				t.Fatal(err)
			}
			positive := "TestTerminalNoticePublicStatusAndWho"
			for _, run := range []struct {
				name string
				red  bool
			}{{p.killer, true}, {positive, false}} {
				parts := strings.Split(run.name, "/")
				for i := range parts {
					parts[i] = "^" + regexp.QuoteMeta(parts[i]) + "$"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
				cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "./internal/cli", "-run", strings.Join(parts, "/"))
				cmd.Dir = dst
				cmd.Env = append(os.Environ(), "A2A_RUN_NOTICE_DEADLINE_MUTANTS=", "A2A_RUN_TERMINAL_ORPHAN_MUTANTS=", "A2A_ALITE_SENTINEL_RUN=", "A2A_PHASE1_SENTINEL_RUN=", "A2A_MATRIX_IT=", "A2A_RUN_KEYCHAIN_MUTANTS=")
				out, runErr := cmd.CombinedOutput()
				timedOut := ctx.Err() != nil
				cancel()
				exit := 0
				if ee, ok := runErr.(*exec.ExitError); ok {
					exit = ee.ExitCode()
				} else if runErr != nil {
					exit = -1
				}
				reached, asserted, badFailure := false, false, false
				decoder := json.NewDecoder(bytes.NewReader(out))
				for decoder.More() {
					var event struct{ Action, Test, Output string }
					if err := decoder.Decode(&event); err != nil {
						t.Fatal(err)
					}
					if event.Test == run.name && (run.red && event.Action == "fail" || !run.red && event.Action == "pass") {
						reached = true
					}
					if event.Test == run.name && strings.Contains(event.Output, p.assertion) {
						asserted = true
					}
					if strings.Contains(event.Output, "panic:") || strings.Contains(event.Output, "setup:") || strings.Contains(event.Output, "cleanup:") {
						badFailure = true
					}
				}
				t.Logf("plant=%s test=%s real_exit=%d\n%s", p.name, run.name, exit, out)
				if timedOut || badFailure || !reached || run.red && (exit != 1 || !asserted) || !run.red && exit != 0 {
					t.Fatalf("SURVIVOR/unproved plant=%s test=%s exit=%d reached=%t intended=%t timeout=%t setup_or_crash=%t", p.name, run.name, exit, reached, asserted, timedOut, badFailure)
				}
			}
			t.Logf("KILLED %s by %s with paired %s", p.name, p.killer, positive)
		})
	}
}
