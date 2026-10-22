package matrix

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in is local source mutation in a private copy, never a live operation.
// Every child runs a named behavioral test and records its actual process exit.
func TestStrictNarrowingMutants(t *testing.T) {
	if os.Getenv("A2A_RUN_STRICT_WIRE_MUTANTS") != "1" {
		t.Skip("explicit local narrowing campaign")
	}
	type plant struct{ name, file, old, next, killer, positive string }
	plants := []plant{
		{"duplicate-x", "strict_json.go", `if _, exists := m[k]; exists {`, `if _, exists := m[k]; exists && k != "x" {`, "TestStrictExpectedCorpus/duplicate", "TestStrictExpectedCorpus/case-distinct"},
		{"utf8-ff", "strict_json.go", `if !utf8.Valid(raw) {`, `if !utf8.Valid(raw) && !strings.Contains(string(raw), "\xff") {`, "TestStrictExpectedCorpus/invalid-utf8", "TestStrictExpectedCorpus/replacement"},
		{"scalar-low-dfff", "strict_json.go", `} else if r >= 0xdc00 && r <= 0xdfff {`, `} else if r >= 0xdc00 && r <= 0xdfff && r != 0xdfff {`, "TestStrictExpectedCorpus/low-surrogate", "TestStrictExpectedCorpus/paired"},
		{"framing-empty-object", "strict_json.go", `if p.i != len(raw) {`, `if p.i != len(raw) && string(raw[p.i:]) != "{}" {`, "TestStrictExpectedCorpus/trailing", "TestStrictExpectedCorpus/object"},
		{"overreject-replacement", "strict_json.go", `if !utf8.Valid(raw) {`, `if !utf8.Valid(raw) || string(raw) == "\"�\"" {`, "TestStrictExpectedCorpus/replacement", "TestStrictExpectedCorpus/string"},
		{"numeric-value-lexeme", "strict_json.go", `return json.Number(string(p.b[start:p.i])), nil`, `if string(p.b[start:p.i]) == "-0" { return json.Number("0"), nil }; return json.Number(string(p.b[start:p.i])), nil`, "TestStrictExpectedCorpus/negative-zero", "TestStrictExpectedCorpus/zero"},
		{"required-event-only", "control.go", `if !exists {`, `if !exists && key == "event_id" { continue }; if !exists {`, "TestGenericACKContract/put/missing", "TestGenericACKContract/put/valid"},
		{"exact-key-substitution", "control.go", `if v, exists := obj[key]; exists {`, `if v, exists := obj[key]; exists { if key == "event_id" { if folded, ok := obj["EVENT_ID"]; ok { v = folded } }`, "TestGenericACKContract/put/substitution", "TestGenericACKContract/put/valid"},
		{"length-plus-one", "control.go", `if resp.ContentLength >= 0 && int64(len(raw)) != resp.ContentLength {`, `if resp.ContentLength >= 0 && int64(len(raw)) != resp.ContentLength && int64(len(raw))+1 != resp.ContentLength {`, "TestACKCompletionAndReceipt/txn/long", "TestACKCompletionAndReceipt/txn/known"},
		{"read-error-exception", "control.go", `if err != nil {
		return v, errors.New("matrix: control response read failed")`, `if err != nil && !(err == io.ErrUnexpectedEOF && strings.Contains(string(raw), "$receipt")) {
		return v, errors.New("matrix: control response read failed")`, "TestACKCompletionAndReceipt/txn/read-error", "TestACKCompletionAndReceipt/txn/known"},
		{"schema-sigil-only", "control.go", `if key == "event_id" && (len(s) < 2 || s[0] != '$') {`, `if key == "event_id" && (len(s) < 2 || s[0] != '$') && s != "ack" {`, "TestGenericACKContract/put/id-grammar", "TestGenericACKContract/put/valid"},
		{"alias-status-absence", "client.go", `if IsCode(err, "M_NOT_FOUND") {`, `if IsCode(err, "M_NOT_FOUND") || IsStatus(err, http.StatusNotFound) {`, "TestStrictAliasAbsence/missing", "TestStrictAliasAbsence/valid"},
		{"privacy-one-message", "client.go", `switch e.ErrCode {`, `if e.Message == "server secret" { return e.Message }; switch e.ErrCode {`, "TestErrorCompletionAndUIA/valid", "TestGenericErrorContract/valid"},
		{"error-case-only", "control.go", `if v, exists := obj[key]; exists {`, `if v, exists := obj[key]; exists {`, "TestGenericErrorContract/case", "TestGenericErrorContract/valid"},
	}
	// The case plant keeps the exact lookup guard and admits only ERRCODE as a fallback.
	plants = append(plants, plant{"bulk-sync-overstrict", "client.go", `if !bulk {`, `if bulk && path == v3+"/sync" { raw, err := io.ReadAll(resp.Body); if err != nil { return nil,err }; resp.Body = io.NopCloser(bytes.NewReader(raw)); if _,err := strictJSON(raw); err != nil { return nil,err } }; if !bulk {`, "TestBulkPeerContentCompatibility/true/sync", "TestBulkPeerContentCompatibility/false/sync"})
	plants[len(plants)-2].old = `if v, exists := obj[key]; exists {`
	plants[len(plants)-2].next = `v, exists := obj[key]; if !exists && key == "errcode" { v, exists = obj["ERRCODE"] }; if exists {`
	plants = append(plants,
		plant{"close-late-forbidden-caller", "client.go", `v, decodeErr := readControl(ctx, resp, maxErrorBody)`, `if resp.StatusCode == 403 && path == v3+"/rooms/"+seg("!r:s")+"/state/s/k" { owned := resp.Body; defer owned.Close(); resp.Body = io.NopCloser(owned) }; v, decodeErr := readControl(ctx, resp, maxErrorBody)`, "TestReviewerCloseCancellation/error", "TestGenericErrorContract/valid"},
		plant{"close-late-alias-caller", "client.go", `if !bulk {`, `if !bulk && path == v3+"/directory/room/"+seg("#r:s") { owned := resp.Body; defer owned.Close(); resp.Body = io.NopCloser(owned) }; if !bulk {`, "TestReviewerCloseCancellation/resolve", "TestGenericACKContract/resolve/valid"},
		plant{"close-error-one-ack", "control.go", `if closeErr != nil {`, `if closeErr != nil && string(raw) != ` + "`" + `{"event_id":"$real","room_id":"!real:s","user_id":"@u:s","content_uri":"mxc://s/media"}` + "`" + ` {`, "TestControlCloseBoundary/put/failure", "TestControlCloseBoundary/put/clean"},
		plant{"identity-no-separator", "control.go", `if s[0] != '@' || !separated || local == "" || server == "" {`, `if (s[0] != '@' || !separated || local == "" || server == "") && s != "@u" {`, "TestReviewerIdentitySchema/whoami/@u", "TestReviewerIdentitySchema/whoami/@u:s"},
		plant{"uia-missing-flows", "control.go", `flows, ok := e.control.value["flows"].([]any)`, `flows, ok := e.control.value["flows"].([]any); if !ok && len(e.control.value) == 1 && e.control.value["session"] == "s" { flows, ok = []any{}, true }`, "TestReviewerUIARequiredFlows/missing-flows", "TestReviewerUIARequiredFlows/genuine"},
		plant{"uia-missing-stages", "control.go", `if _, ok := object["stages"].([]any); !ok {`, `if _, ok := object["stages"].([]any); !ok && len(object) != 0 {`, "TestReviewerUIARequiredFlows/missing-stages", "TestReviewerUIARequiredFlows/genuine"},
		plant{"identifier-plus-one", "control.go", `(key == "event_id" || key == "room_id") && len(s) > 255`, `(key == "event_id" || key == "room_id") && len(s) > 256`, "TestControlIdentifierLength/put/ascii/256", "TestControlIdentifierLength/put/ascii/255"},
		plant{"depth-plus-one", "strict_json.go", `if depth >= maxJSONDepth {`, `if depth > maxJSONDepth {`, "TestStrictResourceNeighbors/10001", "TestStrictResourceNeighbors/10000"},
		plant{"error-cap-equality", "control.go", `if limit > 0 && int64(len(raw)) >= limit {`, `if limit > 0 && int64(len(raw)) >= limit && int64(len(raw)) != maxErrorBody {`, "TestErrorCompletionAndUIA/at-cap", "TestErrorCompletionAndUIA/below-cap"},
		plant{"cancel-one-txn", "txn.go", `func (c *Client) SendMessageTxnReceipt(ctx context.Context, roomID, txnID string, content any) (ACKReceipt, error) {`, `func (c *Client) SendMessageTxnReceipt(ctx context.Context, roomID, txnID string, content any) (ACKReceipt, error) { if txnID == "txn" { ctx = context.WithoutCancel(ctx) }`, "TestACKCompletionAndReceipt/txn/cancel-eof", "TestACKCompletionAndReceipt/txn/known"},
	)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	evidence := os.Getenv("A2A_STRICT_WIRE_EVIDENCE_DIR")
	if evidence == "" {
		evidence = t.TempDir()
	}
	if err := os.MkdirAll(evidence, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go.mod", "go.sum", "internal/matrix"} {
		err := filepath.WalkDir(filepath.Join(root, name), func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return e
			}
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			raw, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			return os.WriteFile(target, raw, 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Shared fixture setup once; each plant is restored before the next one.
	for _, p := range plants {
		t.Run(p.name, func(t *testing.T) {
			path := filepath.Join(dst, "internal/matrix", p.file)
			raw, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Count(string(raw), p.old) != 1 {
				t.Fatal("plant not applied: anchor not unique")
			}
			mutated := []byte(strings.Replace(string(raw), p.old, p.next, 1))
			if e = os.WriteFile(path, mutated, 0600); e != nil {
				t.Fatal(e)
			}
			defer os.WriteFile(path, raw, 0600)
			for name, body := range map[string][]byte{p.name + ".original.go": raw, p.name + ".mutant.go": mutated} {
				if err := os.WriteFile(filepath.Join(evidence, name), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("plant=%s original=%x mutant=%x", p.name, sha256.Sum256(raw), sha256.Sum256(mutated))
			// Run both siblings even after a red; the negative is EXPECTED to fail.
			killed := false
			for _, lane := range []struct {
				name string
				want int
			}{{p.killer, 1}, {p.positive, 0}} {
				argv := []string{"test", "-count=1", "./internal/matrix", "-run", "^" + strings.ReplaceAll(lane.name, "/", "$/^") + "$", "-v"}
				cmd := exec.Command("go", argv...)
				cmd.Dir = dst
				var stdout, stderr strings.Builder
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				start := time.Now().UTC()
				e := cmd.Start()
				if e != nil {
					t.Errorf("NOT STARTED: %v", e)
					continue
				}
				pid := cmd.Process.Pid
				e = cmd.Wait()
				code := cmd.ProcessState.ExitCode()
				laneID := p.name + ".negative"
				if lane.want == 0 {
					laneID = p.name + ".positive"
				}
				metadata := fmt.Sprintf("argv=go %q pid=%d start=%s end=%s exit=%d\n", argv, pid, start.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), code)
				if err := os.WriteFile(filepath.Join(evidence, laneID+".meta"), []byte(metadata), 0600); err != nil {
					t.Fatal(err)
				}
				t.Logf("argv=go %q pid=%d start=%s end=%s exit=%d", argv, pid, start.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), code)
				for _, stream := range []struct{ name, body string }{{"stdout", stdout.String()}, {"stderr", stderr.String()}} {
					if err := os.WriteFile(filepath.Join(evidence, laneID+"."+stream.name), []byte(stream.body), 0600); err != nil {
						t.Fatal(err)
					}
					t.Logf("STREAM %s bytes=%d sha256=%x\n%s", stream.name, len(stream.body), sha256.Sum256([]byte(stream.body)), stream.body)
				}
				if lane.want == 1 {
					if code == 1 && strings.Contains(stdout.String(), "--- FAIL: "+strings.Split(p.killer, "/")[0]) && strings.Contains(stdout.String(), "=== RUN   "+p.killer) && !strings.Contains(stderr.String(), "build failed") {
						killed = true
					} else {
						t.Errorf("SURVIVOR or invalid failure: %v", e)
					}
				} else if code != 0 {
					t.Errorf("valid sibling failed: %v", e)
				}
			}
			if !killed {
				t.Errorf("SURVIVOR: %s; bound is not established", p.name)
			}
		})
	}
	fmt.Fprintln(os.Stderr, "strict narrowing campaign: all selected lanes executed")
}
