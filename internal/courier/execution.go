package courier

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/deliverypolicy"
)

// Request is the single bounded invocation following the C1-compatible hello.
// Files are keyed by argv index; paths are never execution inputs on the daemon.
type Request struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Argv    []string          `json:"argv"`
	Stdin   []byte            `json:"stdin"`
	Files   map[int]InputFile `json:"files"`
	Session string            `json:"session,omitempty"`
}
type InputFile struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// Invocation exposes only the authenticated registry identity, request content,
// output streams and the invocation-bound station delivery barrier.
type Invocation struct {
	Identity       string
	Request        Request
	Stdout, Stderr io.Writer
	Delivered      func() error
	// Refuse reports a closed invocation failure without transmitting backend text.
	Refuse func(Diagnostic) int
}
type Handler func(context.Context, Invocation) int

// Selection contains assertions, never connection selection instructions.
type Selection struct{ As, Profile, Fingerprint string }

// Plan describes the closed command schema and the argv positions that carry
// input file contents. The same parser is used before client I/O and execution.
type Plan struct {
	Command      string
	Args         []string
	Selection    Selection
	Inputs       map[int]bool // true: attachment (bare path); false: @body input
	Stdin        bool         // only an actual stdin operand or hook input, never a flag value
	LongConsumer bool
}

// Authorize uses a positive command/option schema. New CLI flags fail closed
// until their I/O contract is explicitly admitted here.
func Authorize(argv []string, identity string) (Plan, error) {
	p := Plan{Inputs: map[int]bool{}}
	// App's globals are accepted anywhere before --. Check every occurrence,
	// including duplicate --as, rather than allowing a last-value override.
	var rest []string
	indices := []int{}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			for j := i; j < len(argv); j++ {
				rest = append(rest, argv[j])
				indices = append(indices, j)
			}
			break
		}
		name, value, eq := strings.Cut(arg, "=")
		switch name {
		case "--human":
			// Station identities cannot execute commands as human accounts.
			// Hook discovery happens before this gate and retains fail-open.
			return p, errRefused
		case "--as", "-as", "--profile", "--connection-fingerprint":
			if !eq {
				i++
				if i >= len(argv) {
					return p, errRefused
				}
				value = argv[i]
			}
			if value == "" {
				return p, errRefused
			}
			switch name {
			case "--as", "-as":
				if value != identity {
					return p, IdentityMismatch
				}
				p.Selection.As = value
			case "--profile":
				if p.Selection.Profile != "" && p.Selection.Profile != value {
					return p, errRefused
				}
				p.Selection.Profile = value
			case "--connection-fingerprint":
				if p.Selection.Fingerprint != "" && p.Selection.Fingerprint != value {
					return p, errRefused
				}
				p.Selection.Fingerprint = value
			}
		default:
			rest = append(rest, arg)
			indices = append(indices, i)
		}
	}
	if len(rest) == 0 {
		return p, errRefused
	}
	p.Command = rest[0]
	p.Stdin = p.Command == "hook"
	waitTimeout := deliverypolicy.DefaultWaitTimeout
	start := 1
	values, bools := "", ""
	minPos, maxPos := 0, 0
	bodyPos, attachPos := -1, -1
	switch p.Command {
	case "courier":
		if len(rest) != 2 || rest[1] != "status" {
			return p, errRefused
		}
		start = 2
	case "post":
		values = "topic kind re to reply-by trunk subtype subject operation-id envelope"
		bools = "reply-main json"
		minPos = 2
		maxPos = 256
		bodyPos = 1
	case "inbox":
		if len(rest) > 1 && rest[1] == "show" {
			start = 2
			values = "from"
			minPos = 1
			maxPos = 1
		} else {
			values = "limit width"
			bools = "json"
		}
	case "read":
		values = "topic last event"
		bools = "json verification-input envelope-input"
		minPos = 1
		maxPos = 1
	case "topics":
		minPos = 1
		maxPos = 1
	case "topic":
		if len(rest) < 2 || rest[1] != "new" {
			return p, errRefused
		}
		start = 2
		values = "body to kind"
		bools = "json"
		minPos = 3
		maxPos = 256
	case "attach":
		values = "topic kind to"
		bools = "pin json"
		minPos = 2
		maxPos = 256
		attachPos = 1
		bodyPos = 2
	case "files":
		values = "topic last"
		bools = "json"
		minPos = 1
		maxPos = 1
	case "wait", "watch":
		values = "room match-text from kind"
		bools = "all to-me-only json"
		if p.Command == "watch" {
			bools += " notices listen"
		}
		if p.Command == "wait" {
			values += " timeout status heartbeat runtime hooks product"
		}
	case "hook":
		if len(rest) < 2 || (rest[1] != "stop" && rest[1] != "session-start" && rest[1] != "prompt" && rest[1] != "session-end") {
			return p, errRefused
		}
		start = 2
		values = "runtime"
	case "bind":
		if len(rest) < 2 || rest[1] != "status" {
			return p, errRefused
		}
		start = 2
		bools = "json"
	case "who":
		values = "stale-after product history-pages-per-poll poll unanswered-since unanswered-after alert-room keeper reply-alias"
		bools = "json watch alert"
	case "whoami":
	case "post-operation":
		if len(rest) < 2 || rest[1] != "status" {
			return p, errRefused
		}
		start = 2
		bools = "json"
		minPos = 1
		maxPos = 1
	default:
		return p, errRefused
	}
	allowed := func(names, name string) bool {
		for _, n := range strings.Fields(names) {
			if n == name {
				return true
			}
		}
		return false
	}
	var pos []int
	literal := false
	for i := start; i < len(rest); i++ {
		arg := rest[i]
		if !literal && arg == "--" {
			literal = true
			continue
		}
		if !literal && strings.HasPrefix(arg, "-") && arg != "-" {
			name, value, eq := strings.Cut(strings.TrimLeft(arg, "-"), "=")
			if allowed(bools, name) {
				if eq && value != "true" && value != "false" {
					return p, errRefused
				}
				continue
			}
			if !allowed(values, name) {
				return p, errRefused
			}
			vi := i
			if !eq {
				i++
				if i >= len(rest) {
					return p, errRefused
				}
				vi = i
				value = rest[i]
			}
			if p.Command == "topic" && name == "body" && value == "-" {
				p.Stdin = true
			}
			if p.Command == "wait" && name == "timeout" {
				d, err := time.ParseDuration(value)
				if err != nil || d < 0 {
					return p, errRefused
				}
				waitTimeout = d
			}
			if (p.Command == "topic" && name == "body" || p.Command == "post" && name == "envelope" || p.Command == "inbox" && name == "from") && strings.HasPrefix(value, "@") {
				p.Inputs[indices[vi]] = false
			}
			// inbox show --from is a path, so only explicit streamed @input is admitted.
			if p.Command == "inbox" && name == "from" && !strings.HasPrefix(value, "@") {
				return p, errRefused
			}
			if p.Command == "post" && name == "envelope" && !strings.HasPrefix(value, "@") {
				return p, errRefused
			}
			continue
		}
		pos = append(pos, i)
	}
	if len(pos) < minPos || len(pos) > maxPos {
		return p, errRefused
	}
	if bodyPos >= 0 && bodyPos < len(pos) && rest[pos[bodyPos]] == "-" {
		p.Stdin = true
	}
	if bodyPos >= 0 && bodyPos < len(pos) && strings.HasPrefix(rest[pos[bodyPos]], "@") {
		p.Inputs[indices[pos[bodyPos]]] = false
	}
	if attachPos >= 0 {
		p.Inputs[indices[pos[attachPos]]] = true
	}
	p.LongConsumer = deliverypolicy.LongConsumer(p.Command, waitTimeout)
	p.Args = rest[1:]
	return p, nil
}

func parseRequest(data []byte) (Request, error) {
	var r Request
	fields, err := object(data, map[string]bool{"type": true, "id": true, "argv": true, "stdin": true, "files": true, "session": true})
	if err != nil || (len(fields) != 5 && len(fields) != 6) {
		return r, errRefused
	}
	for _, key := range []string{"type", "id", "argv", "stdin", "files"} {
		if fields[key] == nil {
			return r, errRefused
		}
	}
	if raw := fields["session"]; raw != nil {
		var session string
		if json.Unmarshal(raw, &session) != nil || !ValidSession(session) {
			_ = json.Unmarshal(fields["id"], &r.ID)
			return r, NotAllowed
		}
	}
	if json.Unmarshal(data, &r) != nil || r.Type != "request" || !validID(r.ID) || len(r.Argv) == 0 || len(r.Argv) > 256 {
		return r, errRefused
	}
	for _, v := range r.Argv {
		if len(v) > MaxFrameBytes || strings.ContainsRune(v, 0) {
			return r, errRefused
		}
	}
	// Nested input entries have an exact schema as well.
	fileFields, err := object(fields["files"], nil)
	if err != nil || len(fileFields) != len(r.Files) {
		return r, errRefused
	}
	for key, raw := range fileFields {
		index, e := strconv.Atoi(key)
		if e != nil || strconv.Itoa(index) != key || index < 0 || index >= len(r.Argv) {
			return r, errRefused
		}
		f, e := object(raw, map[string]bool{"name": true, "data": true})
		if e != nil || len(f) != 2 {
			return r, errRefused
		}
	}
	return r, nil
}
func validID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
