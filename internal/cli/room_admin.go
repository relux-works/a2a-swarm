package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type roomAdminError struct {
	rule, location string
	code           int
}

func (e *roomAdminError) Error() string { return "RULE " + e.rule + ": " + e.location }
func adminError(rule, location string, code int) *roomAdminError {
	return &roomAdminError{rule, location, code}
}

// Detection reads only command words and global selectors, never credentials.
// Old room branches and their diagnostic/result behavior remain untouched.
func roomAdminInvocation(args []string) bool {
	var words []string
	for i := 0; i < len(args); i++ {
		name, _, eq := strings.Cut(args[i], "=")
		switch name {
		case "--as", "-as", "--profile", "--connection-fingerprint":
			if !eq {
				i++
			}
		case "--human":
		default:
			words = append(words, args[i])
			if len(words) == 1 && words[0] == "space" {
				return true
			}
			if len(words) == 2 {
				return words[0] == "room" && words[1] == "name"
			}
		}
	}
	return false
}

type roomAdminInput struct {
	child, mutate, asJSON        bool
	op, target, peer             string
	expected, expectedPeer, name string
	via                          []string
}

// Global selection retains the existing extractor, but a local scalar's value
// is opaque even if it happens to spell a global flag.
func adminGlobalArgs(args []string) (local, global []string, err *roomAdminError) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			local = append(local, args[i:]...)
			break
		}
		key, value, eq := strings.Cut(arg, "=")
		canonical := key
		if key == "-as" {
			canonical = "--as"
		}
		switch canonical {
		case "--as", "--profile", "--connection-fingerprint", "--human":
			if seen[canonical] || eq && (canonical == "--human" || value == "") {
				return nil, nil, adminError("admin_usage", "argv", ExitUsage)
			}
			seen[canonical] = true
			global = append(global, arg)
			if canonical != "--human" && !eq {
				if i+1 == len(args) || args[i+1] == "" {
					return nil, nil, adminError("admin_usage", "argv", ExitUsage)
				}
				i++
				global = append(global, args[i])
			}
		default:
			local = append(local, arg)
			name := strings.TrimLeft(key, "-")
			switch name {
			case "name", "child", "expect-room-id", "expect-space-id", "expect-child-id", "via":
				if strings.HasPrefix(arg, "-") && !eq && i+1 < len(args) {
					i++
					local = append(local, args[i])
				}
			}
		}
	}
	return local, global, nil
}

func parseRoomAdmin(args []string) (roomAdminInput, []string, *roomAdminError) {
	var in roomAdminInput
	usage := adminError("admin_usage", "argv", ExitUsage)
	local, global, e := adminGlobalArgs(args)
	if e != nil {
		return in, nil, e
	}
	if len(local) < 3 || local[0] != "room" && local[0] != "space" || local[0] == "room" && local[1] != "name" || local[0] == "space" && local[1] != "child" {
		return in, nil, usage
	}
	in.child, in.op = local[0] == "space", local[2]
	if in.op != "get" && (!in.child && in.op != "set" || in.child && in.op != "list" && in.op != "add" && in.op != "remove") {
		return in, nil, usage
	}
	in.mutate = in.op != "get" && in.op != "list"
	fs := newFlagSet("room administration")
	var asJSON propertyBool
	var peer, expected, expectedPeer, name propertyScalar
	var via stringList
	fs.Var(&asJSON, "json", "typed result")
	if in.child && in.op != "list" {
		fs.Var(&peer, "child", "explicit child selector")
	}
	if in.mutate {
		if in.child {
			fs.Var(&expected, "expect-space-id", "exact resolved Space ID")
			fs.Var(&expectedPeer, "expect-child-id", "exact resolved child ID")
			if in.op == "add" {
				fs.Var(&via, "via", "explicit server; repeatable")
			}
		} else {
			fs.Var(&expected, "expect-room-id", "exact resolved room ID")
			fs.Var(&name, "name", "literal display name")
		}
	}
	pos, err := parseInterspersed(fs, local[3:])
	if err != nil || len(pos) != 1 || in.child && in.op != "list" && !peer.seen {
		return in, nil, usage
	}
	probe := &App{}
	if _, err := probe.extractGlobal(global); err != nil {
		return in, nil, usage
	}
	in.target, in.peer, in.expected, in.expectedPeer, in.name = pos[0], peer.value, expected.value, expectedPeer.value, name.value
	in.via, in.asJSON = via, asJSON.value == "true"
	validSelector := func(s string) bool {
		if strings.HasPrefix(s, "!") {
			return matrix.ValidVerificationID(s, '!')
		}
		_, err := normalizeRoom(s, "unused")
		return err == nil
	}
	if !validSelector(in.target) {
		return in, nil, adminError("admin_selector", "$.target", ExitUsage)
	}
	if in.child && in.op != "list" && !validSelector(in.peer) {
		return in, nil, adminError("admin_selector", "$.child", ExitUsage)
	}
	if in.mutate && (!matrix.ValidVerificationID(in.expected, '!') || in.child && (!matrix.ValidVerificationID(in.expectedPeer, '!') || in.expected == in.expectedPeer)) {
		return in, nil, adminError("admin_selector", "$.expected", ExitUsage)
	}
	if in.mutate && !in.child && !matrix.ValidRoomName(in.name) {
		return in, nil, adminError("admin_name", "$.name", ExitUsage)
	}
	if in.op == "add" {
		if i, bad := matrix.RoomChildViaError(in.via); bad {
			location := "$.via"
			if i >= 0 {
				location += "[" + strconv.Itoa(i) + "]"
			}
			return in, nil, adminError("admin_via", location, ExitUsage)
		}
	}
	return in, global, nil
}

// The public boundary exposes only fixed rules, never native helper errors.
func (a *App) adminFailure(e *roomAdminError) int {
	if e == nil {
		return ExitOK
	}
	_, _ = io.WriteString(a.Stderr, e.Error()+"\n")
	return e.code
}

func cmdSpace(context.Context, *App, []string) error {
	return adminError("admin_usage", "argv", ExitUsage)
}

func (a *App) runRoomAdmin(ctx context.Context, args []string) int {
	in, global, e := parseRoomAdmin(args)
	if e != nil {
		return a.adminFailure(e)
	}
	if _, err := a.extractGlobal(global); err != nil {
		return a.adminFailure(adminError("admin_usage", "argv", ExitUsage))
	}
	if a.human && !a.interactive() {
		return a.adminFailure(adminError("admin_session", "$.session", ExitError))
	}
	// Retain station enrollment's closed authorization. These new commands are
	// not in its allowlist, so enrolled stations refuse without a fallback. Its
	// historical diagnostics cannot escape this new command's closed boundary.
	if !a.daemonEngine {
		stdout, stderr := a.Stdout, a.Stderr
		a.Stdout, a.Stderr = io.Discard, io.Discard
		handled, _ := a.stationInvocation(ctx, args)
		a.Stdout, a.Stderr = stdout, stderr
		if handled {
			return a.adminFailure(adminError("admin_session", "$.session", ExitError))
		}
	}
	if ctx.Err() != nil {
		return a.adminFailure(adminError("admin_context", "$.context", ExitError))
	}
	if err := a.connectionPreflight("room", nil); err != nil {
		if ctx.Err() != nil {
			return a.adminFailure(adminError("admin_context", "$.context", ExitError))
		}
		return a.adminFailure(adminError("admin_session", "$.session", ExitError))
	}
	s, err := a.session()
	if err != nil || !matrix.ValidVerificationID(s.creds.UserID, '@') {
		if ctx.Err() != nil {
			return a.adminFailure(adminError("admin_context", "$.context", ExitError))
		}
		return a.adminFailure(adminError("admin_session", "$.session", ExitError))
	}
	s.client = s.client.WithoutRedirects()
	baseGuard := s.client.BeforeRequest
	guardFailed, propertyStage, admitted := false, false, false
	guard := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if baseGuard != nil {
			return baseGuard()
		}
		return nil
	}
	s.client.BeforeRequest = func() error {
		if propertyStage && !admitted && a.beforeAdminPropertyRequest != nil {
			a.beforeAdminPropertyRequest()
		}
		if err := guard(); err != nil {
			guardFailed = true
			return err
		}
		if propertyStage {
			admitted = true
		}
		return nil
	}
	preflightError := func(rule, location string) int {
		if guardFailed || guard() != nil {
			rule, location = "admin_context", "$.context"
		}
		return a.adminFailure(adminError(rule, location, ExitError))
	}
	room, _, err := s.resolveRoom(ctx, in.target)
	if err != nil {
		return preflightError("admin_target", "$.target")
	}
	if !matrix.ValidVerificationID(room, '!') {
		return preflightError("admin_selector", "$.target")
	}
	if in.mutate && room != in.expected {
		return preflightError("admin_target", "$.target")
	}
	child := ""
	if in.child && in.op != "list" {
		child, _, err = s.resolveRoom(ctx, in.peer)
		if err != nil {
			return preflightError("admin_target", "$.child")
		}
		if !matrix.ValidVerificationID(child, '!') {
			return preflightError("admin_selector", "$.child")
		}
		if child == room || in.mutate && child != in.expectedPeer {
			return preflightError("admin_target", "$.child")
		}
	}
	typ, stateType := "room-name", "m.room.name"
	if in.child {
		typ, stateType = "space-child", "m.space.child"
	}
	var policyEvents []matrix.RoomPolicyEvent
	if in.mutate || in.child {
		if in.child && in.op == "list" {
			propertyStage = true
		}
		policyEvents, err = s.client.GetRoomPolicyState(ctx, room)
		if in.child && in.op == "list" {
			propertyStage = false
		}
		rule, location := "admin_policy", "$.policy"
		if in.child {
			rule, location = "admin_space", "$.space"
		}
		if err != nil {
			if in.child && in.op == "list" && errors.Is(err, matrix.ErrRoomPolicyTooLarge) {
				return preflightError("admin_space_limit", "$.space")
			}
			return preflightError(rule, location)
		}
		if in.child && !adminSpaceSnapshot(policyEvents) {
			return preflightError(rule, location)
		}
		if in.mutate {
			power, err := projectRoomPowerForEvent(policyEvents, s.server, room, s.creds.UserID, stateType)
			if err != nil {
				return preflightError("admin_policy", "$.policy")
			}
			allowed := false
			for _, user := range power.Users {
				if user.MXID == s.creds.UserID {
					allowed = user.Effective.Kind == "creator-infinite" || user.Effective.Value != nil && *user.Effective.Value >= power.Required["state_event"]
				}
			}
			if !allowed {
				return preflightError("admin_power", "$.policy")
			}
		}
	}
	if in.child && in.op == "list" {
		rows, hasInvalid, overLimit := projectSpaceChildList(policyEvents)
		if overLimit {
			return preflightError("admin_child_limit", "$.children")
		}
		if guardFailed || guard() != nil {
			return preflightError("admin_context", "$.context")
		}
		return a.writeSpaceChildList(room, s.creds.UserID, rows, hasInvalid, in.asJSON)
	}
	if guardFailed || guard() != nil {
		return preflightError("admin_context", "$.context")
	}
	out := roomPropertyResult{Type: typ, Version: 1, Operation: in.op, Caller: s.creds.UserID, RoomID: room, StateType: stateType, StateKey: child}
	propertyStage = true
	rule, location := "", ""
	if in.mutate {
		if in.child {
			out.StateWriteResult, err = s.client.SetSpaceChild(ctx, room, child, in.via)
		} else {
			out.StateWriteResult, err = s.client.SetRoomName(ctx, room, in.name)
		}
		if err != nil {
			rule, location = "admin_write", "$.write"
			if out.Acknowledged {
				rule, location = "admin_readback", "$.readback"
			}
		} else if in.child {
			content := map[string]any{}
			if in.op == "add" {
				content["via"] = in.via
			}
			out.Content = content
		} else {
			out.Content = map[string]any{"name": in.name}
		}
	} else {
		var content map[string]any
		var present bool
		if in.child {
			content, present, err = s.client.SpaceChild(ctx, room, child)
		} else {
			content, present, err = s.client.RoomName(ctx, room)
		}
		out.Outcome, out.Readback = "ABSENT", "absent"
		if present && err == nil {
			out.Outcome, out.Readback, out.Content = "EMPTY", "present", content
			if name, ok := content["name"].(string); ok && name != "" {
				out.Outcome = "PRESENT"
			}
			if via, ok := content["via"].([]string); ok && len(via) > 0 {
				out.Outcome = "PRESENT"
			}
		}
		if err != nil {
			out.Outcome, out.Readback, out.Content = "UNKNOWN", "unavailable", nil
			rule, location = "admin_state", "$.content"
		}
	}
	if guardFailed || guard() != nil {
		rule, location = "admin_context", "$.context"
		if !admitted {
			if !in.mutate {
				return a.adminFailure(adminError(rule, location, ExitError))
			}
			out.Outcome, out.Readback = "REFUSED", "not-attempted"
		} else {
			out.Outcome, out.Readback = "UNKNOWN", "unavailable"
			if in.mutate && !out.Acknowledged {
				out.Readback = "not-attempted"
			}
		}
		out.Content = nil
	}
	var output []byte
	if in.asJSON {
		output, err = json.Marshal(out)
		output = append(output, '\n')
	} else {
		content, _ := json.Marshal(out.Content)
		output = []byte(fmt.Sprintf("%s v1 %s room=%s key=%s outcome=%s acknowledged=%t event_id=%s readback=%s content=%s\n", out.Type, in.op, strconv.QuoteToASCII(room), strconv.QuoteToASCII(child), out.Outcome, out.Acknowledged, strconv.QuoteToASCII(out.EventID), out.Readback, content))
	}
	if err != nil && rule == "" {
		return a.adminFailure(adminError("admin_output", "$.output", ExitError))
	}
	if n, err := a.Stdout.Write(output); err != nil || n != len(output) {
		return a.adminFailure(adminError("admin_output", "$.output", ExitError))
	}
	if rule != "" {
		return a.adminFailure(adminError(rule, location, ExitError))
	}
	return ExitOK
}

// Child reads need only strict structural/create proof, not the mutation
// parser's caller JOIN, version or authority requirements.
func adminSpaceSnapshot(events []matrix.RoomPolicyEvent) bool {
	seen := map[[2]string]bool{}
	var create map[string]any
	for _, event := range events {
		key := [2]string{event.Type, event.StateKey}
		if seen[key] {
			return false
		}
		seen[key] = true
		if event.Type == "m.room.create" {
			if event.StateKey != "" || event.Content == nil || !powerExactFields(event.Content, "type") {
				return false
			}
			create = event.Content
		}
	}
	return create != nil && create["type"] == "m.space"
}
