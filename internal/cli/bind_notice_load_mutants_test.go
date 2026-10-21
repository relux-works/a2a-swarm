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

// Each plant keeps its gate, admitting a narrower forbidden class. Behavioral
// assertions, never source tokens, timeouts, setup failures or skips, earn kills.
func TestNoticePublicationLoadNarrowingPlants(t *testing.T) {
	if os.Getenv("A2A_RUN_NOTICE_LOAD_MUTANTS") != "1" {
		t.Skip("opt-in synthetic publication plants")
	}
	plants := []struct{ name, old, next, killer, assertion string }{
		{"shared-writer-helper-bypass", `err := s.putOwnStatus(writeCtx, st, record)`, `room, _, err := s.resolveRoom(writeCtx, StatusRoom); if err == nil { _, err = s.client.PutState(writeCtx, room, StatusEventType, s.creds.UserID, record) }; if err == nil { st.Liveness = &record }`, "TestNoticeSharedWriterUncertainty/hook-cancelled", "same-identity unacknowledged writer bypassed required reconciliation and fabricated certainty"},
		{"uncertain-known-equality-exemption", `if !st.NoticeWriteUncertain {`, `if (!st.NoticeWriteUncertain || b.OwnNotice.PossiblyOrphaned != nil) {`, "TestNoticeReturnToAcknowledgedAfterCancellation/applied-cancelled", "uncertain equality did not publish exactly once"},
		{"resolve-before-local-checks", `var publicationErr error`, `if current, loadErr := config.LoadState(s.paths, s.name); loadErr == nil && current.Liveness == nil { if _, _, eagerErr := s.resolveRoom(writeCtx, StatusRoom); eagerErr != nil { return eagerErr } }; var publicationErr error`, "TestNoticeDispatchLocalNoop/missing", "local no-op made network requests"},
		{"unchanged-resolve", `if reflect.DeepEqual(st.Liveness.OwnNotice, b.OwnNotice) {`, `if reflect.DeepEqual(st.Liveness.OwnNotice, b.OwnNotice) { if _,_,err:=s.resolveRoom(writeCtx,StatusRoom);err!=nil{return err};`, "TestNoticeDispatchLocalNoop/unchanged", "local no-op made network requests"},
		{"missing-pending", `b.NoticePublication = "unavailable: no liveness record (a2a status set)"`, `b.NoticePublication = "pending: own liveness publication"`, "TestNoticeDispatchLocalNoop/missing", "missing liveness cause lost"},
		{"stopped-generation-exemption", `|| st.Terminal.Stopped {`, `|| st.Terminal.Stopped && generation!="deadline" {`, "TestNoticePublicationLocalOwnerGates/stopped", "local owner gate made network requests"},
		{"stale-deadline-exemption", `st.Terminal.Generation != generation`, `st.Terminal.Generation != generation && generation!="deadline"`, "TestNoticePublicationLocalOwnerGates/stale", "local owner gate made network requests"},
		{"foreign-bob-exemption", `if !ok || name != s.name {`, `if !ok || name != s.name && name!="bob" {`, "TestNoticePublicationLocalOwnerGates/foreign", "local owner gate made network requests"},
		{"forbidden-pre-send-uncertainty", "if err != nil {\n\t\tif st.Terminal != nil {", "if err != nil { if matrix.IsCode(err, \"M_FORBIDDEN\") { st.NoticeWriteUncertain = true };\n\t\tif st.Terminal != nil {", "TestNoticeCurrentRoomPreSendKnowledge/wait/false/M_FORBIDDEN", "pre-send resolution changed ACK/certainty or bypassed resolver"},
		{"not-found-pre-send-uncertainty", "if err != nil {\n\t\tif st.Terminal != nil {", "if err != nil { if errors.Is(err, matrix.ErrUnknownAlias) { st.NoticeWriteUncertain = true };\n\t\tif st.Terminal != nil {", "TestNoticeCurrentRoomPreSendKnowledge/wait/false/M_NOT_FOUND", "pre-send resolution changed ACK/certainty or bypassed resolver"},
		{"cache-without-invalidation", `func (s *session) putOwnStatus`, `var mutantStatusRooms sync.Map
func (s *session) putOwnStatus`, "TestNoticeCurrentRoomRotation/hook/silent", "current-room publication used stale alias"},
		{"recovered-equality-exemption", `if reflect.DeepEqual(st.Liveness.OwnNotice, b.OwnNotice) {`, `if reflect.DeepEqual(st.Liveness.OwnNotice, b.OwnNotice) { if b.NoticePublication=="unavailable: no liveness record (a2a status set)" {return nil};`, "TestNoticeDispatchLocalNoop/recovered", "acknowledged notice retains missing cause"},
		{"missing-observation-exemption", `!reflect.DeepEqual(current.OwnNotice, n) && current.NoticePublication == "published"`, `!reflect.DeepEqual(current.OwnNotice, n) && (current.NoticePublication == "published" || input=="empty")`, "TestNoticeObservationDoesNotMarkPending", "missing liveness cause changed during observation"},
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
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					return os.WriteFile(target, raw, 0600)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dst, "internal/cli/bind_notice.go")
			if p.name == "shared-writer-helper-bypass" {
				path = filepath.Join(dst, "internal/cli/liveness.go")
			}
			if p.name == "forbidden-pre-send-uncertainty" || p.name == "not-found-pre-send-uncertainty" || p.name == "cache-without-invalidation" {
				path = filepath.Join(dst, "internal/cli/own_status.go")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(raw), p.old) != 1 {
				t.Fatal("setup: plant anchor not unique")
			}
			mutated := strings.Replace(string(raw), p.old, p.next, 1)

			if p.name == "cache-without-invalidation" {
				mutated = strings.Replace(mutated, `"reflect"`, `"reflect"; "sync"`, 1)
				old := `room, _, err := s.resolveRoom(ctx, StatusRoom)`
				if strings.Count(mutated, old) != 1 {
					t.Fatal("setup: cache resolver anchor not unique")
				}
				mutated = strings.Replace(mutated, old, `var room string; var err error
key := s.creds.UserID+" "+s.client.BaseURL
if cached,ok:=mutantStatusRooms.Load(key);ok {room=cached.(string)} else {room,_,err=s.resolveRoom(ctx,StatusRoom);if err==nil {mutantStatusRooms.Store(key,room)}}`, 1)
			}
			if err = os.WriteFile(path, []byte(mutated), 0600); err != nil {
				t.Fatal(err)
			}
			// A shared owner also refuses foreign keys before resolution. Narrow
			// the same Bob class through both retained gates, preserving others.
			if p.name == "foreign-bob-exemption" {
				extra := filepath.Join(dst, "internal/cli/own_status.go")
				raw, err := os.ReadFile(extra)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(raw), p.old) != 1 {
					t.Fatal("setup: shared owner anchor not unique")
				}
				if err = os.WriteFile(extra, []byte(strings.Replace(string(raw), p.old, p.next, 1)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Both runs execute even if one is unexpectedly red. Each plant has its
			// own bounded process and source copy; no live credential opt-ins inherit.
			positive := "TestNoticePublicationLocalOwnerGates/valid"
			if p.name == "shared-writer-helper-bypass" {
				positive = "TestNoticeSharedWriterUncertainty/hook-acknowledged"
			}
			if strings.Contains(p.name, "pre-send-uncertainty") {
				positive = "TestNoticeCurrentRoomPreSendKnowledge/wait/false/M_UNKNOWN"
			}
			if p.name == "uncertain-known-equality-exemption" {
				positive = "TestNoticeReturnToAcknowledgedAfterCancellation/acknowledged-control"
			}
			for _, run := range []struct {
				name string
				red  bool
			}{{p.killer, true}, {positive, false}} {
				parts := strings.Split(run.name, "/")
				for i := range parts {
					parts[i] = "^" + regexp.QuoteMeta(parts[i]) + "$"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "./internal/cli", "-run", strings.Join(parts, "/"))
				cmd.Dir = dst
				cmd.Env = append(os.Environ(), "A2A_RUN_NOTICE_LOAD_MUTANTS=", "A2A_RUN_NOTICE_DEADLINE_MUTANTS=", "A2A_RUN_TERMINAL_ORPHAN_MUTANTS=", "A2A_MATRIX_IT=", "A2A_ALITE_SENTINEL_RUN=", "A2A_PHASE1_SENTINEL_RUN=", "A2A_RUN_KEYCHAIN_MUTANTS=")
				out, runErr := cmd.CombinedOutput()
				timedOut := ctx.Err() != nil
				cancel()
				code := 0
				if ee, ok := runErr.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else if runErr != nil {
					code = -1
				}
				reached, asserted, bad := false, false, false
				dec := json.NewDecoder(bytes.NewReader(out))
				for dec.More() {
					var event struct{ Action, Test, Output string }
					if err := dec.Decode(&event); err != nil {
						t.Error(err)
						break
					}
					if event.Test == run.name && (run.red && event.Action == "fail" || !run.red && event.Action == "pass") {
						reached = true
					}
					if event.Test == run.name && strings.Contains(event.Output, p.assertion) {
						asserted = true
					}
					if strings.Contains(event.Output, "panic:") || strings.Contains(event.Output, "setup:") || strings.Contains(event.Output, "cleanup:") {
						bad = true
					}
				}
				t.Logf("plant=%s test=%s pid=%d exit=%d\n%s", p.name, run.name, cmd.Process.Pid, code, out)
				if timedOut || bad || !reached || run.red && (code != 1 || !asserted) || !run.red && code != 0 {
					t.Errorf("SURVIVOR/unproved plant=%s test=%s exit=%d named=%t intended=%t timeout=%t setup_or_crash=%t", p.name, run.name, code, reached, asserted, timedOut, bad)
				}
			}
		})
	}
}
