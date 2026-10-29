package codexwake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// currentProtocol describes an inspected wire schema, NOT native capability
// admission. Dial deliberately does not use it: a schema cannot qualify an
// attached TUI, ownership, idle wake, or safe scheduling under human control.
// Sources and complete schema fixtures live in testdata/issue122/.
type currentProtocol struct {
	version string
}

// inspectCurrentProtocol pins the complete exported source schemas, rather
// than treating a version string or an unverified lookalike as qualification.
// The local 0.163.0 export differs from its release schema outside this wire
// slice; both exact fingerprints were inspected. Supplied .165 exports were
// verified here, without executing that binary or qualifying native behavior.
func inspectCurrentProtocol(version string, turnSchema, historySchema []byte) (currentProtocol, error) {
	turnHash := sha256.Sum256(turnSchema)
	historyHash := sha256.Sum256(historySchema)
	turn := hex.EncodeToString(turnHash[:])
	history := hex.EncodeToString(historyHash[:])
	const releaseTurn = "835e0d4ca1a0ddc4fb6db1e2b3249dd67f07c50ac2506092ba81543efc92829b"
	valid := false
	switch version {
	case "0.163.0":
		valid = (turn == releaseTurn || turn == "abf0d6d29d223b38c650ff1155cb9c9f9b4211e6d0e20c57d750a9e1babfdb41") && history == "0ecf37ff62571c8411aab271ea65d22be4da1108cc2fd2dffb15a7034bfedb28"
	case "0.165.0":
		valid = (turn == releaseTurn || turn == "f671d01c24995e99fcc110862ff3251c26d334fba212c7dc12857fb939506e47") && history == "fa5a85adff4a594f6c7183aca89414338fbe51b692d5f5ff8edd88d01f51dc4d"
	}
	if !valid {
		return currentProtocol{}, errors.New("runtime current wire schema unverified")
	}
	return currentProtocol{version: version}, nil
}

func (p currentProtocol) valid() bool { return p.version == "0.163.0" || p.version == "0.165.0" }

// currentHostOutput is a transport bound, not an encoder or provenance check.
// The caller MUST supply immutable output from its durable attempt, created
// using the shared CLI doorbell encoder. It must not pass peer text here.
// Generation/lease/lifecycle/rate admission and persistence remain with the
// existing controller; these helpers perform no I/O or ledger mutation.
func currentHostOutput(output string) bool {
	// Existing shared notice <=300 bytes plus bounded generation/attempt marker.
	if output == "" || len(output) > 512 || !utf8.ValidString(output) {
		return false
	}
	for _, r := range output {
		if (unicode.IsControl(r) && r != '\n') || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

// wakeParams preserves tool authority and explicitly requests retained history
// for same-attempt reconciliation. No user input, approvals or policy overrides
// are expressible in the constructed request. Returning params is NOT sending
// or native acceptance, and never clears an uncertain attempt.
func (p currentProtocol) wakeParams(thread, output string) (map[string]any, error) {
	if !p.valid() || !token.MatchString(thread) || !currentHostOutput(output) {
		return nil, errors.New("runtime current wake requires verified schema, thread and bounded host output")
	}
	return map[string]any{
		"threadId": thread,
		"input":    []any{},
		"toolOutput": map[string]any{
			"name": ToolName, "namespace": ToolNamespace, "output": output, "retain": true,
		},
	}, nil
}

// matchesOutput recognizes the current schema's snake_case content tag. It
// compares only the exact expected journal output; assistant/peer lookalikes,
// stale markers and mixed media do not establish authoritative acceptance.
// The caller must already have scoped history to the owned thread/turn and
// generation. A nonmatch is inconclusive, never permission to resend.
func (p currentProtocol) matchesOutput(item Item, expected string) bool {
	return p.valid() && currentOutputMatches(item, expected)
}

// currentOutputMatches is shared with the legacy client history reader.
// Recognizing exact host output in this additive shape does not admit a server.
func currentOutputMatches(item Item, expected string) bool {
	if !currentHostOutput(expected) || len(item.Output) > 4<<20 || item.Type != "functionCallOutput" || item.Name != ToolName || item.Namespace != ToolNamespace {
		return false
	}
	var text string
	if json.Unmarshal(item.Output, &text) == nil {
		return text == expected
	}
	var parts []currentTextPart
	if json.Unmarshal(item.Output, &parts) != nil || len(parts) == 0 || len(parts) > 512 {
		return false
	}
	var body strings.Builder
	for _, part := range parts {
		if part.kind != "input_text" || body.Len()+len(part.text) > len(expected) {
			return false
		}
		body.WriteString(part.text)
	}
	return body.String() == expected
}

type currentTextPart struct{ kind, text string }

// Reject unknown, duplicate, missing and non-string fields rather than letting
// malformed content acquire meaning through encoding/json's last-key wins.
func (p *currentTextPart) UnmarshalJSON(raw []byte) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return errors.New("runtime malformed text content")
	}
	fields := map[string]string{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return errors.New("runtime malformed text content")
		}
		name, ok := key.(string)
		if !ok || (name != "type" && name != "text") {
			return errors.New("runtime unexpected text content field")
		}
		if _, seen := fields[name]; seen {
			return errors.New("runtime duplicate text content field")
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil || len(value) == 0 || value[0] != '"' {
			return errors.New("runtime non-string text content field")
		}
		var text string
		if json.Unmarshal(value, &text) != nil {
			return errors.New("runtime malformed text content field")
		}
		fields[name] = text
	}
	if _, err := dec.Token(); err != nil {
		return errors.New("runtime malformed text content")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || len(fields) != 2 {
		return errors.New("runtime incomplete text content")
	}
	p.kind, p.text = fields["type"], fields["text"]
	return nil
}
