package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const adminIssueCommand = "!admin token issue --once --max-age 5m"
const adminCleanupWarning = "a2a: warning: admin registration redaction incomplete; rotation is not needed; the single-use token remains in room history until it expires (at most five minutes after issue), and may still be unused if registration failed; expiry invalidates the token but does not erase history\n"

func sameAdminPolicy(a, b *config.AdminRegistration) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (s *session) adminRoomExcluded(room string) bool {
	return room != "" && room == s.client.ProtectedRoom
}

// Never print arbitrary errcode or server text. Unknown codes have a fixed
// replacement: an errcode field is itself untrusted server content.
func adminRegistrationError(stage string, err error) error {
	code, status := "LOCAL_REFUSAL", 0
	var me *matrix.Error
	if errors.As(err, &me) {
		status = me.Status
		code = "SERVER_ERROR"
		switch me.ErrCode {
		case "M_FORBIDDEN", "M_NOT_FOUND", "M_UNKNOWN_TOKEN", "M_MISSING_TOKEN", "M_USER_IN_USE", "M_LIMIT_EXCEEDED", "M_BAD_JSON", "M_UNRECOGNIZED", "M_INVALID_PARAM", "M_UNAUTHORIZED",
			"M_INVALID_USERNAME", "M_EXCLUSIVE", "M_NOT_JSON", "M_MISSING_PARAM":
			code = me.ErrCode
		}
	}
	return fmt.Errorf("admin registration stage=%s errcode=%s HTTP=%d; no automatic retry", stage, code, status)
}

func cmdAdminRoomRegister(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("connection register")
	via := fs.Bool("via-admin-room", false, "use explicitly configured admin-room admission")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !*via {
		return usagef("usage: a2a --as ADMIN connection register NAME --via-admin-room")
	}
	if err = config.ValidateName(pos[0]); err != nil {
		return adminRegistrationError("destination", err)
	}
	c, err := a.selectedConnection()
	if err != nil || c == nil || c.AdminRegistration == nil || a.human {
		return adminRegistrationError("configuration", err)
	}
	if err = c.Validate(); err != nil {
		return adminRegistrationError("configuration", err)
	}
	name, err := a.identity()
	if err != nil || a.as == "" || "@"+name+":"+c.ServerName != c.AdminRegistration.AdminUserID {
		return adminRegistrationError("session", err)
	}
	s, err := a.session()
	if err != nil {
		return adminRegistrationError("session", err)
	}
	s.client = s.client.ForAdminRegistration()
	who, err := s.client.Whoami(ctx)
	if err != nil {
		return adminRegistrationError("session-server", err)
	}
	if who.UserID != c.AdminRegistration.AdminUserID {
		return adminRegistrationError("session-server-mismatch", nil)
	}
	if err = a.checkAdminRegistrationRoom(ctx, s); err != nil {
		return err
	}
	_, path, err := a.registerNamedAgentUsing(ctx, pos[0], s)
	if err != nil {
		return err
	}
	a.printf("registered %s; credentials: %s\n", pos[0], path)
	return nil
}

func (a *App) checkAdminRegistrationRoom(ctx context.Context, s *session) error {
	c, err := a.selectedConnection()
	if err != nil || c == nil || c.AdminRegistration == nil {
		return adminRegistrationError("configuration", err)
	}
	r := c.AdminRegistration
	state, err := s.client.AdminRegistrationRoomState(ctx, r.RoomID)
	if err != nil {
		return adminRegistrationError("room-state", err)
	}
	expected := map[string]bool{}
	for _, m := range r.ExpectedMembers {
		expected[m] = true
	}
	seen := map[string]bool{}
	createSeen := false
	stateKeys := map[string]bool{}
	for _, ev := range state {
		if ev.StateKey == nil || ev.Type == "" {
			return adminRegistrationError("room-state-mismatch", nil)
		}
		key := ev.Type + "\x00" + *ev.StateKey
		if stateKeys[key] {
			return adminRegistrationError("room-state-mismatch", nil)
		}
		stateKeys[key] = true
		if ev.RoomID != "" && ev.RoomID != r.RoomID {
			return adminRegistrationError("room-state-mismatch", nil)
		}
		if ev.Type == "m.room.create" {
			var content struct {
				Federate    *bool  `json:"m.federate"`
				Creator     string `json:"creator"`
				RoomVersion string `json:"room_version"`
			}
			// State has other legitimate fields; strict JSON syntax is validated by
			// the Matrix control reader, then only the immutable safety fields matter.
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(ev.Content, &fields)
			if string(fields["m.federate"]) != "false" || json.Unmarshal(ev.Content, &content) != nil || createSeen || ev.StateKey == nil || *ev.StateKey != "" || content.Federate == nil || *content.Federate || ev.Sender != r.ServerBotUserID || content.Creator != "" && content.Creator != r.ServerBotUserID {
				return adminRegistrationError("room-federation", nil)
			}
			createSeen = true
		}
		if ev.Type == "m.room.member" {
			var m struct {
				Membership string `json:"membership"`
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(ev.Content, &fields)
			if fields["membership"] == nil || ev.StateKey == nil || json.Unmarshal(ev.Content, &m) != nil {
				return adminRegistrationError("room-membership", nil)
			}
			if m.Membership == "join" || m.Membership == "invite" || m.Membership == "knock" {
				if m.Membership != "join" || !expected[*ev.StateKey] || seen[*ev.StateKey] {
					return adminRegistrationError("room-membership", nil)
				}
				seen[*ev.StateKey] = true
			} else if m.Membership != "leave" && m.Membership != "ban" {
				return adminRegistrationError("room-membership", nil)
			}
		}
	}
	if !createSeen || len(seen) != len(expected) {
		return adminRegistrationError("room-membership", nil)
	}
	owner, _, ownerErr := ownerFiles(s.paths, s.name).load()
	if ownerErr != nil {
		return adminRegistrationError("room-owner-dm", ownerErr)
	}
	if owner != nil && owner.Room == r.RoomID {
		return adminRegistrationError("room-owner-dm", nil)
	}
	subs, err := s.getSubs(ctx)
	if err != nil {
		return adminRegistrationError("room-subscriptions", err)
	}
	// Broad selectors might include this room now or after new messages; refuse
	// them conservatively, including expired selectors awaiting removal.
	for _, sub := range subs.Subs {
		if sub.Room == "" || sub.Room == r.RoomID {
			return adminRegistrationError("room-subscriptions", nil)
		}
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return adminRegistrationError("room-mailbox", err)
	}
	if len(st.LegacyPending) != 0 {
		return adminRegistrationError("room-mailbox-legacy", nil)
	}
	mb, err := config.OpenMailbox(s.paths, s.name)
	if err != nil {
		return adminRegistrationError("room-mailbox", err)
	}
	for _, e := range mb.Pending() {
		if e.Room == r.RoomID {
			return adminRegistrationError("room-mailbox", nil)
		}
	}
	return nil
}

type adminRegistrationCleanup struct {
	session        *session
	command, reply string
	attempted      bool
	extraReply     bool
}

func (c *adminRegistrationCleanup) finish() {
	if !c.attempted {
		return
	}
	// A cancelled issue/register still gets one bounded best-effort cleanup.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.session.app.context()), 5*time.Second)
	defer cancel()
	failed := c.reply == "" || c.command == "" || c.extraReply
	for _, id := range []string{c.reply, c.command} {
		if id != "" {
			if err := c.session.client.RedactRegistrationEvent(ctx, c.session.client.ProtectedRoom, id); err != nil {
				failed = true
			}
		}
	}
	if failed {
		fmt.Fprint(c.session.app.Stderr, adminCleanupWarning)
	}
}

// Pinned source: matrix-construct/tuwunel f473d9a4a8342514302884041e0ccfea1ad503fc
// src/admin/token/issue.rs, service/registration_tokens/{mod,data}.rs,
// core/utils/{rand,time}.rs. The entire plaintext notice must match.
var adminTokenReply = regexp.MustCompile("^New registration token issued: `([A-Za-z0-9]{16})` - Token used 0 times\\. Expires after 1 uses or in ([0-9]+\\.[0-9]{2}) (minutes|seconds|milliseconds|microseconds|nanoseconds) \\(([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2})\\)\\.$")

func (a *App) parseAdminToken(body string, now time.Time) (string, error) {
	m := adminTokenReply.FindStringSubmatch(body)
	if m == nil {
		return "", adminRegistrationError("reply-parse", nil)
	}
	token := m[1]
	a.secrets.Add(token) // Before duration/date processing, registration, or errors.
	n, err := strconv.ParseFloat(m[2], 64)
	units := map[string]float64{"minutes": 60, "seconds": 1, "milliseconds": .001, "microseconds": .000001, "nanoseconds": .000000001}
	remaining := n * units[m[3]]
	expiry, dateErr := time.Parse("2006-01-02 15:04:05", m[4])
	if err != nil || dateErr != nil || remaining <= 0 || remaining > 300 || !expiry.After(now) || expiry.After(now.Add(5*time.Minute)) {
		return "", adminRegistrationError("reply-expiry", nil)
	}
	return token, nil
}

func (a *App) issueAdminRegistrationToken(parent context.Context, s *session, cleanup *adminRegistrationCleanup) (string, error) {
	// Recheck the immutable room state immediately before issue, after local
	// destination/secret-store preflight. This remains a snapshot, not a lock.
	if err := a.checkAdminRegistrationRoom(parent, s); err != nil {
		return "", err
	}
	r := a.connection.AdminRegistration
	ctx, cancel := context.WithTimeout(parent, time.Duration(r.ReplyDeadlineSeconds)*time.Second)
	defer cancel()
	baseline, err := s.client.AdminRegistrationSync(ctx, r.RoomID, "", 0)
	if err != nil {
		return "", adminRegistrationError("reply-position", err)
	}
	if baseline.NextBatch == "" {
		return "", adminRegistrationError("reply-position", nil)
	}
	start := a.Now()
	cleanup.attempted = true
	cleanup.command, err = s.client.SendMessage(ctx, r.RoomID, map[string]any{"msgtype": "m.text", "body": adminIssueCommand})
	if err != nil {
		return "", adminRegistrationError("issue", err)
	}
	if !matrix.ValidVerificationID(cleanup.command, '$') {
		return "", adminRegistrationError("issue-ack", nil)
	}
	since := baseline.NextBatch
	for polls := 0; polls < 32; polls++ {
		response, err := s.client.AdminRegistrationSync(ctx, r.RoomID, since, time.Second)
		if err != nil {
			return "", adminRegistrationError("reply-wait", err)
		}
		if ctx.Err() != nil || !a.Now().Before(start.Add(time.Duration(r.ReplyDeadlineSeconds)*time.Second)) {
			return "", adminRegistrationError("reply-deadline", nil)
		}
		if response.NextBatch == "" {
			return "", adminRegistrationError("reply-position", nil)
		}
		since = response.NextBatch
		candidate := ""
		for room, j := range response.Join {
			if room != r.RoomID || j.Limited {
				return "", adminRegistrationError("reply-room", nil)
			}
			for _, ev := range j.Events {
				// Mark strictly shaped secrets before any later envelope/relation error.
				var secretFields map[string]json.RawMessage
				if json.Unmarshal(ev.Content, &secretFields) == nil {
					var body string
					if json.Unmarshal(secretFields["body"], &body) == nil {
						if match := adminTokenReply.FindStringSubmatch(body); match != nil {
							a.secrets.Add(match[1])
						}
					}
				}
				if ev.EventID == cleanup.command && ev.Sender == s.creds.UserID {
					continue
				}
				if ev.RoomID != "" && ev.RoomID != r.RoomID || ev.Type != "m.room.message" || ev.Sender != r.ServerBotUserID || !matrix.ValidVerificationID(ev.EventID, '$') {
					return "", adminRegistrationError("reply-envelope", nil)
				}
				var content struct {
					MsgType       string            `json:"msgtype"`
					Body          string            `json:"body"`
					Format        string            `json:"format,omitempty"`
					FormattedBody string            `json:"formatted_body,omitempty"`
					RelatesTo     *matrix.RelatesTo `json:"m.relates_to"`
				}
				keysOK := true
				for key := range secretFields {
					switch key {
					case "msgtype", "body", "format", "formatted_body", "m.relates_to":
					default:
						keysOK = false
					}
				}
				if !keysOK || config.DecodeConnectionJSON(ev.Content, &content) != nil {
					return "", adminRegistrationError("reply-parse", nil)
				}
				if !adminReplyRelates(secretFields["m.relates_to"], cleanup.command) {
					return "", adminRegistrationError("reply-relation", nil)
				}
				if cleanup.reply == "" {
					cleanup.reply = ev.EventID
				} else if cleanup.reply != ev.EventID {
					cleanup.extraReply = true
				}

				if ev.OriginServerTS < start.UnixMilli() || ev.OriginServerTS > start.Add(time.Duration(r.ReplyDeadlineSeconds)*time.Second).UnixMilli() {
					return "", adminRegistrationError("reply-deadline", nil)
				}
				if content.MsgType != "m.notice" {
					return "", adminRegistrationError("reply-parse", nil)
				}
				if candidate != "" {
					return "", adminRegistrationError("reply-multiple", nil)
				}
				candidate, err = a.parseAdminToken(content.Body, a.Now())
				if err != nil {
					return "", err
				}
			}
		}
		if candidate != "" {
			return candidate, nil
		}
	}
	return "", adminRegistrationError("reply-deadline", nil)
}

// Accept only the two relations emitted by this pinned server dialect. Exact
// keys avoid Go JSON field-folding turning an unrelated field into authority.
func adminReplyRelates(raw json.RawMessage, command string) bool {
	var relation map[string]json.RawMessage
	if json.Unmarshal(raw, &relation) != nil {
		return false
	}
	var kind, event string
	if raw, ok := relation["rel_type"]; ok {
		if json.Unmarshal(raw, &kind) != nil {
			return false
		}
	}
	if raw, ok := relation["event_id"]; ok {
		if json.Unmarshal(raw, &event) != nil {
			return false
		}
	}
	if kind == "m.thread" && event == command {
		for k, v := range relation {
			switch k {
			case "rel_type", "event_id":
			case "is_falling_back":
				if string(v) != "false" {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if len(relation) != 1 {
		return false
	}
	var reply map[string]json.RawMessage
	if json.Unmarshal(relation["m.in_reply_to"], &reply) != nil || len(reply) != 1 {
		return false
	}
	var id string
	return json.Unmarshal(reply["event_id"], &id) == nil && id == command
}
