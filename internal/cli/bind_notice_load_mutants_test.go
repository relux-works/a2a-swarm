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
	isolateBindTest(t)
	if os.Getenv("A2A_RUN_NOTICE_LOAD_MUTANTS") != "1" {
		t.Skip("opt-in synthetic publication plants")
	}
	plants := []struct{ name, old, next, killer, assertion string }{
		{"predicate-refusal-length", `if c.ownStatusEvidence && resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {`, `if c.ownStatusEvidence && resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength && int64(len(data))+1 != resp.ContentLength {`, "TestOwnStatusRefusalDeclaredLength/short-true/prior-false", "refusal length evidence accepted"},
		{"direct-put-get-ack-exemption", `if c.directOnly {`, `if c.directOnly && !(req.Method == http.MethodPut && next.Method == http.MethodGet) {`, "TestReviewCR2RedirectACKTraffic/direct-false", "redirect ACK cooldown traffic"},
		{"direct-resolve-get-exemption", `if c.directOnly {`, `if c.directOnly && req.Method != http.MethodGet {`, "TestDirectOnlyRedirects/302/resolve", "scoped redirect refusal"},
		{"direct-success-other-url-exemption", `resp.Request.URL.String() != originalURL`, `(resp.Request.URL.String() != originalURL && resp.Request.URL.Path != "/other")`, "TestDirectOnlyResponseOrigin/different-url", "nonoriginal ACK accepted"},
		{"direct-success-101-exemption", `resp.StatusCode < 200 || redirected`, `(resp.StatusCode < 200 && resp.StatusCode != 101) || redirected`, "TestDirectOnlyResponseOrigin/informational", "nonoriginal ACK accepted"},

		{"response-cap-hit-exemption", `len(data) < maxErrorBody`, `len(data) <= maxErrorBody`, "TestReviewN1ErrorBodyCap/valid-cap/prior-false/notice", "complete-body knowledge"},
		{"response-redirect-get-exemption", `OriginalRequest: !redirected && resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() == originalURL`, `OriginalRequest: (!redirected && resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() == originalURL) || (redirected && resp.Request != nil && resp.Request.Method == http.MethodGet)`, "TestPutStateGenericRedirectRefusal/redirect302", "redirected GET refusal provenance"},
		{"response-returned-url-exemption", `OriginalRequest: !redirected &&`, `OriginalRequest: (!redirected || (resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() == originalURL)) &&`, "TestPutStateRedirectProvenance/return-to-original", "redirect chain provenance"},
		{"response-url-other-exemption", `resp.Request.URL.String() == originalURL`, `(resp.Request.URL.String() == originalURL || resp.Request.URL.Path == "/other")`, "TestPutStateResponseEvidence/different-url", "original response URL"},
		{"response-cancelled-eof-exemption", `ctx.Err() == nil && json.Unmarshal`, `(ctx.Err() == nil || method == http.MethodPut) && json.Unmarshal`, "TestPutStateResponseEvidence/cancel-at-eof", "complete bounded response"},
		{"retry-known-cooldown-exemption", `if now.Before(st.NoticeRetry.Next) {`, `if now.Before(st.NoticeRetry.Next) && st.NoticeWriteUncertain {`, "TestNoticeRetryPersistentFailures/forbidden", "cooldown traffic"},
		{"retry-known-no-doubling", `delay = min(b.RetryMax, max(b.Interval, st.NoticeRetry.Delay*2))`, `if !st.NoticeWriteUncertain { delay = min(b.RetryMax, b.Interval) } else { delay = min(b.RetryMax, max(b.Interval, st.NoticeRetry.Delay*2)) }`, "TestNoticeRetryPersistentFailures/forbidden", "cooldown traffic"},
		{"retry-forbidden-prior-clear", `st.NoticeWriteUncertain = priorUncertain`, `st.NoticeWriteUncertain = priorUncertain && st.Terminal == nil`, "TestNoticeRetryEarlierUncertaintyRebind", "later forbidden erased earlier uncertainty"},
		{"retry-status-500-exemption", `refusal.Status == http.StatusForbidden`, `(refusal.Status == http.StatusForbidden || refusal.Status == 500)`, "TestOwnStatusDefinitiveRefusalKnowledge/notice/prior-false/server-code", "per-attempt knowledge"},
		{"retry-hook-ack-reset-exemption", `st.NoticeRetry = nil
		} else if`, `if record.State != "idle" { st.NoticeRetry = nil }
		} else if`, "TestNoticeRetryOwnWriterReset/hook/ack-true", "actual ACK did not clear retry state"},
		{"retry-partial-error-decode", `if readErr == nil && len(data) < maxErrorBody && ctx.Err() == nil && json.Unmarshal(data, &decoded) == nil {`, `if readErr == nil && len(data) < maxErrorBody && ctx.Err() == nil && (json.Unmarshal(data, &decoded) == nil || decoded.ErrCode == "M_FORBIDDEN") {`, "TestOwnStatusDefinitiveRefusalKnowledge/notice/prior-false/partial-decode", "per-attempt knowledge"},
		{"retry-put-body-error-exemption", `if readErr == nil && len(data) < maxErrorBody && ctx.Err() == nil && json.Unmarshal(data, &decoded) == nil {`, `if (readErr == nil || method == http.MethodPut) && len(data) < maxErrorBody && ctx.Err() == nil && json.Unmarshal(data, &decoded) == nil {`, "TestOwnStatusDefinitiveRefusalKnowledge/notice/prior-false/body-read-error", "per-attempt knowledge"},
		{"retry-zero-policy-exemption", `b.Interval < time.Second`, `b.Interval < time.Second && b.Interval != 0`, "TestNoticeRetryPolicy/zero", "invalid retry policy made HTTP or mutated knowledge"},
		{"retry-zero-delay-exemption", `retry.Delay <= 0`, `retry.Delay < 0`, "TestNoticeRetryMalformedState/zero", "invalid retry did not report pending"},
		{"shared-writer-helper-bypass", `publicationErr := s.putOwnStatus(writeCtx, &snapshot, record)`, `room, _, publicationErr := s.resolveRoom(writeCtx, StatusRoom); if publicationErr == nil { _, publicationErr = s.client.PutState(writeCtx, room, StatusEventType, s.creds.UserID, record) }; if publicationErr == nil { snapshot.Liveness = &record }`, "TestNoticeSharedWriterUncertainty/hook-cancelled", "same-identity unacknowledged writer bypassed required reconciliation and fabricated certainty"},
		{"uncertain-known-equality-exemption", `if !st.NoticeWriteUncertain {`, `if (!st.NoticeWriteUncertain || b.OwnNotice.PossiblyOrphaned != nil) {`, "TestNoticeReturnToAcknowledgedAfterCancellation/applied-cancelled", "uncertain equality did not publish exactly once"},
		{"resolve-before-local-checks", `var publicationErr error`, `if _, _, eagerErr := s.resolveRoom(ctx, StatusRoom); eagerErr != nil { return eagerErr }; var publicationErr error`, "TestNoticeDispatchLocalNoop/unchanged", "local no-op made network requests"},
		{"unchanged-resolve", `if ack != nil && reflect.DeepEqual(ack.OwnNotice, b.OwnNotice) {`, `if ack != nil && reflect.DeepEqual(ack.OwnNotice, b.OwnNotice) { if _,_,err:=s.resolveRoom(ctx,StatusRoom);err!=nil{return config.AgentStatus{},false,err};`, "TestNoticeDispatchLocalNoop/unchanged", "local no-op made network requests"},
		{"missing-liveness-network", `if st.Liveness == nil {`, `if st.Liveness == nil { _,_,_ = s.resolveRoom(ctx,StatusRoom);`, "TestNoticeDispatchLocalNoop/missing", "local no-op made network requests"},
		{"stopped-generation-exemption", `|| st.Terminal.Stopped {`, `|| st.Terminal.Stopped && generation!="deadline" {`, "TestNoticePublicationLocalOwnerGates/stopped", "local owner gate made network requests"},
		{"stale-deadline-exemption", `st.Terminal.Generation != generation`, `st.Terminal.Generation != generation && generation!="deadline"`, "TestNoticePublicationLocalOwnerGates/stale", "local owner gate made network requests"},
		{"foreign-bob-exemption", `if !ok || name != s.name {`, `if !ok || name != s.name && name!="bob" {`, "TestNoticePublicationLocalOwnerGates/foreign", "local owner gate made network requests"},
		{"forbidden-pre-send-uncertainty", "if err != nil {\n\t\tif st.Terminal != nil {", "if err != nil { if matrix.IsCode(err, \"M_FORBIDDEN\") { st.NoticeWriteUncertain = true };\n\t\tif st.Terminal != nil {", "TestNoticeCurrentRoomPreSendKnowledge/wait/false/M_FORBIDDEN", "pre-send resolution changed ACK/certainty or bypassed resolver"},
		{"not-found-pre-send-uncertainty", "if err != nil {\n\t\tif st.Terminal != nil {", "if err != nil { if errors.Is(err, matrix.ErrUnknownAlias) { st.NoticeWriteUncertain = true };\n\t\tif st.Terminal != nil {", "TestNoticeCurrentRoomPreSendKnowledge/wait/false/M_NOT_FOUND", "pre-send resolution changed ACK/certainty or bypassed resolver"},
		{"cache-without-invalidation", `func (s *session) putOwnStatus`, `var mutantStatusRooms sync.Map
func (s *session) putOwnStatus`, "TestNoticeCurrentRoomRotation/hook/silent", "current-room publication used stale alias"},
		{"recovered-equality-exemption", `if ack != nil && reflect.DeepEqual(ack.OwnNotice, b.OwnNotice) {`, `if ack != nil && reflect.DeepEqual(ack.OwnNotice, b.OwnNotice) { if b.NoticePublication=="unavailable: no liveness record (a2a status set)" {return config.AgentStatus{},false,nil};`, "TestNoticeDispatchLocalNoop/recovered", "acknowledged notice retains missing cause"},
		{"missing-observation-exemption", `!reflect.DeepEqual(current.OwnNotice, n) && current.NoticePublication == "published"`, `!reflect.DeepEqual(current.OwnNotice, n) && (current.NoticePublication == "published" || input=="empty")`, "TestNoticeObservationDoesNotMarkPending", "missing liveness cause changed during observation"},
	}

	// Shared predicate legs are narrowed separately for the two response shapes.
	// These run the real own writer, including pre-send resolve knowledge.
	for _, shape := range []string{"put", "resolve"} {
		field := "event_id"
		method := "http.MethodPut"
		if shape == "resolve" {
			field = "room_id"
			method = "http.MethodGet"
		}
		for _, leg := range []struct{ name, old, next, kind string }{
			{"origin", `resp.Request.URL.String() != originalURL`, `(resp.Request.URL.String() != originalURL && !(req.Method == ` + method + ` && resp.Request.URL.Path == "/other"))`, "different-url"},
			{"method", `resp.Request.Method != method`, `(resp.Request.Method != method && !(req.Method == ` + method + ` && resp.Request.Method == http.MethodPost))`, "different-method"},
			{"status", `resp.StatusCode < 200 || redirected`, `(resp.StatusCode < 200 && !(req.Method == ` + method + ` && resp.StatusCode == 101)) || redirected`, "informational"},
			{"length", `resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength`, `resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength && field != "` + field + `"`, "short"},
			{"read", `if err != nil {`, `if err != nil && !(field == "` + field + `" && errors.Is(err, io.ErrUnexpectedEOF)) {`, "read-error"},
			{"cancel", `if err := ctx.Err(); err != nil {`, `if err := ctx.Err(); err != nil && field != "` + field + `" {`, "cancel-eof"},
			{"json", `if err := json.Unmarshal(data, &object);`, `if err := json.NewDecoder(strings.NewReader(string(data))).Decode(&object);`, "trailing-value"},
			{"shape", `err != nil || id == ""`, `err != nil || (id == "" && field != "` + field + `")`, "empty"},
		} {
			plants = append(plants, struct{ name, old, next, killer, assertion string }{
				"predicate-" + shape + "-" + leg.name, leg.old, leg.next,
				"TestOwnStatusEvidencePredicate/" + shape + "/" + leg.kind + "/prior-false", "predicate evidence accepted",
			})
			if leg.name == "cancel" {
				// The CLI also checks cancellation before PUT. Drive the public
				// adapter itself so that independent guard cannot mask this plant.
				last := &plants[len(plants)-1]
				last.killer = "TestOwnStatusResponseCancellation/" + shape + "/cancel-eof"
				last.assertion = "cancelled shape evidence accepted"
			}
		}
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
				path = filepath.Join(dst, "internal/cli/own_status.go")
			}
			if p.name == "forbidden-pre-send-uncertainty" || p.name == "not-found-pre-send-uncertainty" || p.name == "cache-without-invalidation" {
				path = filepath.Join(dst, "internal/cli/own_status.go")
			}
			if p.name == "retry-forbidden-prior-clear" || p.name == "retry-status-500-exemption" || p.name == "retry-hook-ack-reset-exemption" {
				path = filepath.Join(dst, "internal/cli/own_status.go")
			}
			if strings.HasPrefix(p.name, "direct-") || strings.HasPrefix(p.name, "response-") || p.name == "retry-partial-error-decode" || p.name == "retry-put-body-error-exemption" {
				path = filepath.Join(dst, "internal/matrix/client.go")
			}
			if strings.HasPrefix(p.name, "predicate-") {
				path = filepath.Join(dst, "internal/matrix/own_status.go")
				if strings.HasSuffix(p.name, "-origin") || strings.HasSuffix(p.name, "-status") || strings.HasSuffix(p.name, "-method") || p.name == "predicate-refusal-length" {
					path = filepath.Join(dst, "internal/matrix/client.go")
				}
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(raw), p.old) != 1 {
				t.Fatal("setup: plant anchor not unique")
			}
			mutated := strings.Replace(string(raw), p.old, p.next, 1)
			if strings.HasPrefix(p.name, "predicate-") && strings.HasSuffix(p.name, "-json") {
				mutated = strings.Replace(mutated, `"net/http"`, `"net/http"; "strings"`, 1)
				mutated = strings.Replace(mutated, `return json.Unmarshal(data, out)`, `return json.NewDecoder(strings.NewReader(string(data))).Decode(out)`, 1)
			}
			if p.name == "direct-put-get-ack-exemption" {
				old := `if c.directOnly && resp.StatusCode < 300 &&`
				next := `if c.directOnly && !(req.Method == http.MethodPut && resp.Request != nil && resp.Request.Method == http.MethodGet && resp.StatusCode == 200) && resp.StatusCode < 300 &&`
				if strings.Count(mutated, old) != 1 {
					t.Fatal("setup: scoped ACK anchor not unique")
				}
				mutated = strings.Replace(mutated, old, next, 1)
			}

			if p.name == "cache-without-invalidation" {
				mutated = strings.Replace(mutated, `"reflect"`, `"reflect"; "sync"`, 1)
				old := `room, _, err := writer.resolveRoom(ctx, StatusRoom)`
				if strings.Count(mutated, old) != 1 {
					t.Fatal("setup: cache resolver anchor not unique")
				}
				mutated = strings.Replace(mutated, old, `var room string; var err error
key := s.creds.UserID+" "+s.client.BaseURL
if cached,ok:=mutantStatusRooms.Load(key);ok {room=cached.(string)} else {room,_,err=writer.resolveRoom(ctx,StatusRoom);if err==nil {mutantStatusRooms.Store(key,room)}}`, 1)
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
			if strings.HasPrefix(p.name, "predicate-") {
				shape := strings.Split(p.name, "-")[1]
				positive = "TestOwnStatusEvidencePredicate/" + shape + "/direct/prior-false"
				if p.name == "predicate-refusal-length" {
					positive = "TestOwnStatusRefusalDeclaredLength/short-false/prior-false"
				}
				if strings.HasSuffix(p.name, "-cancel") {
					positive = "TestOwnStatusResponseCancellation/" + shape + "/direct"
				}
			}
			switch p.name {
			case "direct-put-get-ack-exemption":
				positive = "TestReviewCR2RedirectACKTraffic/direct-true"
			case "direct-resolve-get-exemption":
				positive = "TestDirectOnlyRedirects/0/resolve"
			case "direct-success-other-url-exemption", "direct-success-101-exemption":
				positive = "TestDirectOnlyResponseOrigin/direct"
			case "response-cap-hit-exemption":
				positive = "TestReviewN1ErrorBodyCap/valid-below-cap/prior-false/notice"
			case "response-redirect-get-exemption":
				positive = "TestPutStateGenericRedirectRefusal/direct"
			case "response-returned-url-exemption":
				positive = "TestPutStateRedirectProvenance/direct"
			case "response-url-other-exemption", "response-cancelled-eof-exemption":
				positive = "TestPutStateResponseEvidence/direct"
			case "retry-known-cooldown-exemption", "retry-known-no-doubling":
				positive = "TestNoticeRetryPersistentFailures/server"
			case "retry-forbidden-prior-clear", "retry-status-500-exemption", "retry-partial-error-decode":
				positive = "TestOwnStatusDefinitiveRefusalKnowledge/notice/prior-false/forbidden"
			case "retry-hook-ack-reset-exemption":
				positive = "TestNoticeRetryOwnWriterReset/manual/ack-true"
			case "retry-put-body-error-exemption":
				positive = "TestOwnStatusDefinitiveRefusalKnowledge/notice/prior-false/forbidden"
			case "retry-zero-policy-exemption":
				positive = "TestNoticeRetryPolicy/valid"
			case "retry-zero-delay-exemption":
				positive = "TestNoticeRetryMalformedState/absent"
			}
			runs := []struct {
				name string
				red  bool
			}{{p.killer, true}, {positive, false}}
			if strings.HasPrefix(p.name, "predicate-") && p.name != "predicate-refusal-length" {
				shape := strings.Split(p.name, "-")[1]
				runs = append(runs, struct {
					name string
					red  bool
				}{"TestOwnStatusEvidencePredicate/" + shape + "/server/prior-false", false})
				if strings.HasSuffix(p.name, "-cancel") {
					runs[len(runs)-1].name = "TestOwnStatusResponseCancellation/" + shape + "/server"
					// Retain the end-to-end knowledge assertion as an additional
					// valid control for the redundant CLI pre-send guard.
					if shape == "resolve" {
						runs = append(runs, struct {
							name string
							red  bool
						}{"TestOwnStatusEvidencePredicate/resolve/cancel-eof/prior-false", false})
					}
				}
			}
			for _, run := range runs {
				parts := strings.Split(run.name, "/")
				for i := range parts {
					parts[i] = "^" + regexp.QuoteMeta(parts[i]) + "$"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				pkg := "./internal/cli"
				if strings.HasPrefix(run.name, "TestPutState") || strings.HasPrefix(run.name, "TestDirectOnly") || strings.HasPrefix(run.name, "TestOwnStatusResponseCancellation") {
					pkg = "./internal/matrix"
				}
				cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", pkg, "-run", strings.Join(parts, "/"))
				cmd.Dir = dst
				cmd.Env = append(os.Environ(), "A2A_RUN_NOTICE_LOAD_MUTANTS=", "A2A_RUN_NOTICE_DEADLINE_MUTANTS=", "A2A_RUN_TERMINAL_ORPHAN_MUTANTS=", "A2A_MATRIX_IT=", "A2A_ALITE_SENTINEL_RUN=", "A2A_PHASE1_SENTINEL_RUN=", "A2A_RUN_KEYCHAIN_MUTANTS=")
				start := time.Now().UTC()
				out, runErr := cmd.CombinedOutput()
				end := time.Now().UTC()
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
				pid := 0
				if cmd.Process != nil {
					pid = cmd.Process.Pid
				}
				t.Logf("plant=%s test=%s argv=%q pid=%d start=%s end=%s exit=%d\n%s", p.name, run.name, cmd.Args, pid, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), code, out)
				if timedOut || bad || !reached || run.red && (code != 1 || !asserted) || !run.red && code != 0 {
					t.Errorf("SURVIVOR/unproved plant=%s test=%s exit=%d named=%t intended=%t timeout=%t setup_or_crash=%t", p.name, run.name, code, reached, asserted, timedOut, bad)
				}
			}
		})
	}
}
