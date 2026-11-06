package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Proves the envelope carriage tests detect known violations. Each plant
// disables or corrupts exactly one check in an isolated copy of one source file
// (go test -overlay; the real file is never written) and must make the named
// test fail on the intended assertion. A compile error, a missing test or a
// timeout does not qualify: the negative run must contain "--- FAIL:" and the
// intended assertion text, the run before the plant and the run after it must
// pass, and the original sources must be byte-identical at the end. It is opt-in
// because it starts nested `go test` runs:
//
//	A2A_RUN_ENVELOPE_CONTROLS=1 go test -p 1 ./internal/cli -run '^TestEnvelopeInstrumentControls$' -v -timeout 40m
func TestEnvelopeInstrumentControls(t *testing.T) {
	if os.Getenv("A2A_RUN_ENVELOPE_CONTROLS") != "1" {
		t.Skip("opt-in isolated source controls")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	refusals := `^TestEnvelopeEventRefusals$/^`
	plants := []struct{ name, pkg, file, old, next, mask, assertion string }{
		// --- strict single-event projection (internal/matrix) ---
		// With the primary trailing-data check off, the span extraction's own
		// trailing-data refusal still stops the response, but under the generic
		// code; the specific-code assertion is what detects the missing check.
		{"skip-trailing-data-check", "matrix", "strict_json.go", `if p.i != len(raw) {`, `if false && p.i != len(raw) {`,
			refusals + `trailing-document$`, `trailing-document: got code "response_invalid" want "trailing_data"`},
		{"skip-duplicate-key-check", "matrix", "strict_json.go", `if _, exists := m[k]; exists {`, `if _, exists := m[k]; exists && false {`,
			refusals + `duplicate-envelope-key$`, `want "duplicate_key"`},
		{"skip-room-check", "matrix", "envelope.go", `!ok || s != roomID`, `!ok || (false && s != roomID)`,
			refusals + `wrong-room$`, `wrong-room: got code "" want "room_id_mismatch"`},
		{"skip-sender-validation", "matrix", "envelope.go", `!ok || !ValidVerificationID(sender, '@')`, `!ok`,
			refusals + `sender-short$`, `want "sender_invalid"`},
		{"skip-case-alias-check", "matrix", "envelope.go", `if k != name && strings.EqualFold(k, name) {`, `if false && k != name && strings.EqualFold(k, name) {`,
			refusals + `alias-only-envelope-key$`, `want "key_case_alias"`},
		{"normalize-body-text", "matrix", "envelope.go", `BodyText: []byte(body)`, `BodyText: []byte(strings.ReplaceAll(body, "\r\n", "\n"))`,
			`^TestEnvelopeEventExactObservation$/^json-with-crlf-and-unicode$`, `body text changed`},
		{"shorten-sender", "matrix", "envelope.go", `Sender: sender, RoomID: roomID`, `Sender: strings.SplitN(strings.TrimPrefix(sender, "@"), ":", 2)[0], RoomID: roomID`,
			`^TestEnvelopeEventSenderSpellingPreserved$`, `sender changed`},
		{"skip-envelope-limit", "matrix", "envelope.go", `if len(value) > base64.RawURLEncoding.EncodedLen(EnvelopeMaxBytes) {`, `if false {`,
			`^TestEnvelopeCodec$/^decode-over-limit$`, `over limit:`},
		{"lenient-base64", "matrix", "envelope.go", ` || base64.RawURLEncoding.EncodeToString(out) != value`, ``,
			`^TestEnvelopeCodec$/^decode-malformed$`, `got <nil>`},
		// --- sender (internal/cli, internal/matrix) ---
		{"trim-envelope-bytes", "cli", "envelope.go", `return matrix.EncodeEnvelope(data)`, `return matrix.EncodeEnvelope([]byte(strings.TrimRight(string(data), "\n")))`,
			`^TestEnvelopeRoundTripExactBytes$/^trailing-lf-and-leading-bom$`, `envelope bytes changed`},
		{"trim-event-text", "cli", "envelope.go", "\treturn text, nil\n}", "\treturn strings.TrimRight(text, \" \\t\\n\"), nil\n}",
			`^TestEnvelopeRoundTripExactBytes$/^text-from-stdin-is-not-trimmed$`, `event text changed`},
		{"skip-event-budget", "cli", "envelope.go", `if err := matrix.CheckEventContentBudget(content); err != nil {`, `if err := error(nil); err != nil {`,
			`^TestPostEnvelopeRefusals$/^event-budget-50000$`, `exit 0, want 1`},
		{"skip-text-alteration-check", "cli", "envelope.go", `if sanitizeText(text) != text {`, `if false {`,
			`^TestPostEnvelopeRefusals$/^text-with-cr$`, `exit 0, want 1`},
		// The intended-code assertion: without the spec-limit check the file is
		// still refused (by the event budget), so only a test that asserts the
		// specific code can notice.
		{"skip-spec-size-limit", "matrix", "envelope.go", `if len(envelope) > EnvelopeMaxBytes {`, `if false {`,
			`^TestPostEnvelopeRefusals$/^oversize-by-one$`, `lacks "envelope refused: envelope_too_large"`},
		// --- fetch bounds (internal/matrix) ---
		{"skip-fetch-deadline", "matrix", "envelope.go", `ctx, cancel := withTimeout(ctx)`, `ctx, cancel := context.WithCancel(ctx)`,
			`^TestEnvelopeEventDeadline$`, `envelope fetch has no deadline`},
		{"accept-non-200", "matrix", "envelope.go", `if resp.StatusCode != http.StatusOK {`, `if false && resp.StatusCode != http.StatusOK {`,
			`^TestEnvelopeEventRequiresOK$`, `status 201: want response_invalid`},
		// --- redaction (internal/matrix, internal/cli) ---
		{"skip-server-redaction-marker", "matrix", "envelope.go", `return zero, Refuse(RefuseEventRedacted, "")`, `_ = 0`,
			refusals + `redacted-with-content$`, `redacted-with-content: got code "" want "event_redacted"`},
		{"skip-pre-fetch-tombstone", "cli", "envelope.go", `if len(events) != 1 || events[0].IsRedacted() {`, `if false && (len(events) != 1 || events[0].IsRedacted()) {`,
			`^TestEnvelopeInputLocalTombstones$`, `pre-fetch refusal lost`},
		{"skip-post-fetch-tombstone", "cli", "envelope.go", `if len(filtered) != 1 || filtered[0].IsRedacted() {`, `if false && (len(filtered) != 1 || filtered[0].IsRedacted()) {`,
			`^TestEnvelopeInputLocalTombstones$`, `unsynced edit of a redacted target escaped`},
	}
	hashes := map[string][32]byte{}
	for _, p := range plants {
		path := filepath.Join(root, "internal", p.pkg, p.file)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hashes[path] = sha256.Sum256(b)
	}
	for _, p := range plants {
		t.Run(p.name, func(t *testing.T) {
			source := filepath.Join(root, "internal", p.pkg, p.file)
			original, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(original), p.old) != 1 {
				t.Fatalf("plant anchor absent or ambiguous: %q", p.old)
			}
			dir := t.TempDir()
			replacement := filepath.Join(dir, p.file)
			if err := os.WriteFile(replacement, []byte(strings.Replace(string(original), p.old, p.next, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			overlay := filepath.Join(dir, "overlay.json")
			data, _ := json.Marshal(map[string]any{"Replace": map[string]string{source: replacement}})
			if err := os.WriteFile(overlay, data, 0600); err != nil {
				t.Fatal(err)
			}
			// The test package is the one that holds the asserting test.
			target := "./internal/matrix"
			if strings.HasPrefix(p.mask, `^TestPostEnvelope`) || strings.HasPrefix(p.mask, `^TestEnvelopeRoundTrip`) || strings.HasPrefix(p.mask, `^TestEnvelopeInput`) {
				target = "./internal/cli"
			}
			run := func(overlay string) ([]byte, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()
				args := []string{"test", "-p", "1", "-count=1", "-v"}
				if overlay != "" {
					args = append(args, "-overlay", overlay)
				}
				args = append(args, target, "-run", p.mask)
				cmd := exec.CommandContext(ctx, "go", args...)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "GOMAXPROCS=2")
				var out bytes.Buffer
				cmd.Stdout, cmd.Stderr = &out, &out
				if err := cmd.Start(); err != nil {
					return nil, err
				}
				t.Logf("owned child pid=%d overlay=%t target=%s", cmd.Process.Pid, overlay != "", target)
				err := cmd.Wait()
				if ctx.Err() != nil {
					t.Fatalf("control timed out: %s", out.String())
				}
				return out.Bytes(), err
			}
			positive, err := run("")
			if err != nil || !bytes.Contains(positive, []byte("--- PASS:")) {
				t.Fatalf("pre-plant positive did not pass: %v\n%s", err, positive)
			}
			negative, err := run(overlay)
			if err == nil || !bytes.Contains(negative, []byte("--- FAIL:")) || !bytes.Contains(negative, []byte(p.assertion)) {
				t.Fatalf("intended control assertion not reached: %v\n%s", err, negative)
			}
			restored, err := run("")
			if err != nil || !bytes.Contains(restored, []byte("--- PASS:")) {
				t.Fatalf("restored positive did not pass: %v\n%s", err, restored)
			}
			t.Logf("intended FAIL observed (%q), original source unchanged, restored positive PASS\n%s", p.assertion, negative)
		})
	}
	for path, want := range hashes {
		b, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(b) != want {
			t.Fatalf("source changed during isolated controls: %s", path)
		}
	}
}
