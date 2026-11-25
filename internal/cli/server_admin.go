package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const serverAdminUsage = "usage: a2a server-admin run --out FILE [--connection NAME] <command text|@file|->"

func serverAdminError(stage string) error {
	return fmt.Errorf("server-admin stage=%s", stage)
}

// serverAdminProfile selects the command's connection before App.Run performs
// its normal connection preflight. The command parser checks the flag again.
func serverAdminProfile(profile string, args []string) (string, error) {
	if len(args) == 0 || args[0] != "run" {
		return profile, nil
	}
	selected := ""
	seen := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name := strings.TrimLeft(arg, "-")
		value, hasValue := "", false
		if at := strings.IndexByte(name, '='); at >= 0 {
			value, hasValue = name[at+1:], true
			name = name[:at]
		}
		if name != "connection" || !strings.HasPrefix(arg, "-") {
			if strings.HasPrefix(arg, "-") && name == "out" && !hasValue && i+1 < len(args) {
				i++
			}
			continue
		}
		if seen {
			return "", usagef("server-admin run: duplicate --connection")
		}
		seen = true
		if !hasValue {
			if i+1 >= len(args) || args[i+1] == "--" {
				return "", usagef("server-admin run: --connection needs a name")
			}
			i++
			value = args[i]
		}
		if value == "" || config.ValidateName(value) != nil {
			return "", usagef("server-admin run: invalid --connection")
		}
		selected = value
	}
	if selected == "" {
		return profile, nil
	}
	if profile != "" && profile != selected {
		return "", usagef("server-admin run: --connection conflicts with --profile")
	}
	return selected, nil
}

func readServerAdminCommand(a *App, source string) ([]byte, error) {
	var reader io.Reader
	switch {
	case source == "-":
		reader = a.Stdin
		if reader == nil {
			reader = os.Stdin
		}
	case strings.HasPrefix(source, "@"):
		path := strings.TrimPrefix(source, "@")
		if path == "" {
			return nil, serverAdminError("command_input")
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, serverAdminError("command_input")
		}
		defer file.Close()
		reader = file
	default:
		return []byte(source), nil
	}
	body, err := io.ReadAll(io.LimitReader(reader, 1025))
	if err != nil {
		return nil, serverAdminError("command_input")
	}
	return body, nil
}

func validateServerAdminCommand(body []byte) error {
	if len(body) > 1024 || !utf8.Valid(body) || !strings.HasPrefix(string(body), "!admin ") {
		return serverAdminError("command_refused")
	}
	for _, r := range string(body) {
		if unicode.IsControl(r) {
			return serverAdminError("command_refused")
		}
	}
	return nil
}

func reserveServerAdminOutput(path string, open func(string, int, os.FileMode) (*os.File, error)) (*os.File, error) {
	if path == "" {
		return nil, serverAdminError("output")
	}
	f, err := open(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, serverAdminError("output_exists")
		}
		return nil, serverAdminError("output")
	}
	info, err := f.Stat()
	if err != nil || info.Mode().Perm() != 0600 {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, serverAdminError("output_mode")
	}
	return f, nil
}

func cmdServerAdmin(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return usagef(serverAdminUsage)
	}
	fs := newFlagSet("server-admin run")
	outPath := fs.String("out", "", "private reply output file")
	connection := fs.String("connection", "", "named connection profile")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 || *outPath == "" {
		return usagef(serverAdminUsage)
	}
	if *connection != "" {
		if config.ValidateName(*connection) != nil || a.profile != "" && a.profile != *connection {
			return usagef("server-admin run: invalid or conflicting --connection")
		}
		a.profile = *connection
		a.connectionResolved = false
		a.connection = nil
		a.connectionError = nil
	}
	command, err := readServerAdminCommand(a, pos[0])
	if err != nil {
		return err
	}
	if err := validateServerAdminCommand(command); err != nil {
		return err
	}

	// O_EXCL reserves the path before the first network call and creates it
	// private from the outset. Every failure before a complete reply removes it.
	openOutput := a.serverAdminOutputOpen
	if openOutput == nil {
		openOutput = os.OpenFile
	}
	out, err := reserveServerAdminOutput(*outPath, openOutput)
	if err != nil {
		return err
	}
	keepOutput := false
	defer func() {
		_ = out.Close()
		if !keepOutput {
			_ = os.Remove(*outPath)
		}
	}()

	c, err := a.selectedConnection()
	if err != nil || c == nil || c.AdminRegistration == nil || c.Validate() != nil {
		return serverAdminError("configuration")
	}
	r := c.AdminRegistration
	name, err := a.identity()
	if err != nil || fmt.Sprintf("@%s:%s", name, c.ServerName) != r.AdminUserID {
		return serverAdminError("identity")
	}
	s, err := a.session()
	if err != nil {
		return serverAdminError("session")
	}
	s.client = s.client.ForAdminRegistration()
	who, err := s.client.Whoami(ctx)
	if err != nil || who.UserID != r.AdminUserID {
		return serverAdminError("session_identity")
	}
	if err := a.checkAdminRegistrationRoom(ctx, s); err != nil {
		return serverAdminError("room_check")
	}

	baselineCtx, baselineCancel := context.WithTimeout(ctx, matrix.DefaultRequestTimeout)
	baseline, err := s.client.AdminRegistrationSync(baselineCtx, r.RoomID, "", 0)
	baselineCancel()
	if err != nil || baseline.NextBatch == "" {
		return serverAdminError("reply_position")
	}
	deadline := time.Duration(r.ReplyDeadlineSeconds) * time.Second
	started := a.Now()
	waitCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	sentID, err := s.client.SendMessage(waitCtx, r.RoomID, map[string]string{
		"msgtype": "m.text",
		"body":    string(command),
	})
	if err != nil {
		return serverAdminError("send")
	}
	if !matrix.ValidVerificationID(sentID, '$') {
		return serverAdminError("send_ack")
	}
	replyID, replyBody, err := waitForServerAdminReply(waitCtx, s, r.RoomID, r.ServerBotUserID, baseline.NextBatch, sentID, started, deadline, a.Now)
	if err != nil {
		return err
	}
	a.secrets.Add(string(replyBody))
	if n, err := out.Write(replyBody); err != nil || n != len(replyBody) {
		return serverAdminError("output_write")
	}
	if err := out.Close(); err != nil {
		return serverAdminError("output_close")
	}
	digest := sha256.Sum256(replyBody)
	if _, err := fmt.Fprintf(a.Stdout, "sent_event_id=%s reply_event_id=%s reply_bytes=%d reply_sha256=%s out=%q\n", sentID, replyID, len(replyBody), hex.EncodeToString(digest[:]), *outPath); err != nil {
		return serverAdminError("stdout")
	}
	keepOutput = true
	return nil
}

func waitForServerAdminReply(ctx context.Context, s *session, roomID, botID, since, sentID string, started time.Time, deadline time.Duration, now func() time.Time) (string, []byte, error) {
	deadlineAt := started.Add(deadline)
	for {
		if ctx.Err() != nil {
			return "", nil, serverAdminError("no_reply")
		}
		batch, err := s.client.AdminRegistrationSync(ctx, roomID, since, time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return "", nil, serverAdminError("no_reply")
			}
			return "", nil, serverAdminError("reply_wait")
		}
		if batch.NextBatch == "" {
			return "", nil, serverAdminError("reply_position")
		}
		for joinedRoom, timeline := range batch.Join {
			if joinedRoom != roomID || timeline.Limited {
				return "", nil, serverAdminError("reply_history")
			}
			for _, event := range timeline.Events {
				if event.RoomID != "" && event.RoomID != roomID {
					return "", nil, serverAdminError("reply_room")
				}
				if event.Type != "m.room.message" || event.Sender != botID {
					continue
				}
				var fields map[string]json.RawMessage
				if json.Unmarshal(event.Content, &fields) != nil {
					return "", nil, serverAdminError("reply_content")
				}
				if !adminReplyRelates(fields["m.relates_to"], sentID) {
					continue
				}
				if !matrix.ValidVerificationID(event.EventID, '$') {
					return "", nil, serverAdminError("reply_event")
				}
				if event.OriginServerTS < started.UnixMilli() || event.OriginServerTS > deadlineAt.UnixMilli() || !now().Before(deadlineAt) {
					return "", nil, serverAdminError("no_reply")
				}
				var body string
				if json.Unmarshal(fields["body"], &body) != nil {
					return "", nil, serverAdminError("reply_body")
				}
				return event.EventID, []byte(body), nil
			}
		}
		since = batch.NextBatch
		// Some servers return immediately from a long-poll with an unchanged
		// empty timeline. Bound that case without spinning through the deadline.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", nil, serverAdminError("no_reply")
		case <-timer.C:
		}
	}
}
