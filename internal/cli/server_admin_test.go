package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Profile selection follows the command parser, including flag values that look like another flag.
func TestServerAdminProfileSelection(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "explicit separated connection", profile: "", args: []string{"run", "--out", "reply.txt", "--connection", "admin", "!admin status"}, want: "admin"},
		{name: "explicit equals connection", profile: "", args: []string{"run", "--out=reply.txt", "--connection=admin", "!admin status"}, want: "admin"},
		{name: "output path looks like connection flag", profile: "current", args: []string{"run", "--out", "--connection", "!admin status"}, want: "current"},
		{name: "conflicting global profile", profile: "different", args: []string{"run", "--out", "reply.txt", "--connection", "admin", "!admin status"}, wantErr: true},
		{name: "duplicate connection", profile: "", args: []string{"run", "--out", "reply.txt", "--connection", "admin", "--connection=other", "!admin status"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := serverAdminProfile(tt.profile, tt.args)
			if (err != nil) != tt.wantErr || err == nil && profile != tt.want {
				t.Fatal("server-admin profile selection did not match the command flags")
			}
		})
	}
}

func runServerAdminFixture(f *admin198Fixture, stdin io.Reader, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runServerAdminFixtureWithWriters(f, stdin, &stdout, &stderr, args...)
	return code, stdout.String(), stderr.String()
}

func runServerAdminFixtureWithWriters(f *admin198Fixture, stdin io.Reader, stdout, stderr io.Writer, args ...string) int {
	a := f.app(stdout, stderr)
	if stdin != nil {
		a.Stdin = stdin
	}
	all := append([]string{"--as", "admin"}, args...)
	return a.Run(context.Background(), all)
}

func serverAdminRoomFromSendPath(path string) string {
	const roomSegment = "/rooms/"
	index := strings.Index(path, roomSegment)
	if index < 0 {
		return ""
	}
	rest := path[index+len(roomSegment):]
	segment, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	roomID, err := url.PathUnescape(segment)
	if err != nil {
		return ""
	}
	return roomID
}

type serverAdminFailWriter struct{ called bool }

func (w *serverAdminFailWriter) Write([]byte) (int, error) {
	w.called = true
	return 0, errors.New("injected stdout failure")
}

func setAdmin198OtherCurrent(t *testing.T, f *admin198Fixture) {
	t.Helper()
	other := f.c
	other.ID = "other"
	other.AdminRegistration = nil
	raw, err := json.Marshal(config.ConnectionRegistry{
		SchemaVersion: 1,
		Current:       "other",
		Profiles:      map[string]config.Connection{"test": f.c, "other": other},
	})
	if err != nil || os.WriteFile(filepath.Join(f.root, "connections.json"), raw, 0600) != nil {
		t.Fatal("alternate connection fixture could not be created")
	}
}

func serverAdminOutPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "reply.txt")
}

func serverAdminRunArgs(outPath, source string) []string {
	return []string{"server-admin", "run", "--out", outPath, source}
}

func serverAdminRootSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		snapshot[rel] = info.Mode().String() + ":" + hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal("fixture persistent-state snapshot failed")
	}
	return snapshot
}

func serverAdminStoreSnapshot(f *admin198Fixture) (map[config.SecretReference]string, []config.SecretReference, []config.SecretReference) {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	items := make(map[config.SecretReference]string, len(f.store.items))
	for key, value := range f.store.items {
		items[key] = value
	}
	return items, append([]config.SecretReference(nil), f.store.reads...), append([]config.SecretReference(nil), f.store.creates...)
}

func assertServerAdminPayload(t *testing.T, payload []byte, want string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || len(fields) != 2 {
		t.Fatal("fake homeserver did not receive a two-field message payload")
	}
	var msgtype, body string
	if json.Unmarshal(fields["msgtype"], &msgtype) != nil || msgtype != "m.text" || json.Unmarshal(fields["body"], &body) != nil || body != want {
		t.Fatal("fake homeserver payload did not preserve the exact command")
	}
	if _, ok := fields["m.mentions"]; ok {
		t.Fatal("a2a envelope field reached the server-admin message")
	}
}

// Text, @file, and stdin inputs reach the fake homeserver unchanged; replies stay private and local state remains untouched.
func TestServerAdminReplySavedPrivately(t *testing.T) {
	command := "!admin inspect café “queue”"
	tests := []struct {
		name    string
		mode    string
		command string
		roomID  string
		source  func(t *testing.T) (string, io.Reader)
		flags   []string
		wantID  string
	}{
		{
			name: "text with explicit connection", mode: "server-admin", command: command,
			roomID: "!server-admin-target:test.invalid",
			source: func(*testing.T) (string, io.Reader) { return command, nil },
			flags:  []string{"--connection", "test"}, wantID: "$admin-reply",
		},
		{
			name: "file with thread relation", mode: "server-admin-thread", command: command,
			source: func(t *testing.T) (string, io.Reader) {
				path := filepath.Join(t.TempDir(), "command.txt")
				if err := os.WriteFile(path, []byte(command), 0600); err != nil {
					t.Fatal("command fixture file could not be created")
				}
				return "@" + path, nil
			}, wantID: "$admin-reply",
		},
		{
			name: "stdin takes first related reply", mode: "server-admin-multiple", command: command,
			source: func(*testing.T) (string, io.Reader) { return "-", strings.NewReader(command) },
			wantID: "$admin-reply",
		},
		{
			name: "maximum command length", mode: "server-admin", command: "!admin " + strings.Repeat("x", 1017),
			source: func(*testing.T) (string, io.Reader) { return "!admin " + strings.Repeat("x", 1017), nil },
			wantID: "$admin-reply",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAdmin198Backend(t, tt.mode, "file")
			outPath := serverAdminOutPath(t)
			if tt.roomID != "" {
				f.c.AdminRegistration.RoomID = tt.roomID
				f.save()
			}
			if tt.name == "text with explicit connection" {
				setAdmin198OtherCurrent(t, f)
			}
			f.mu.Lock()
			f.serverAdminOutputPath = outPath
			f.mu.Unlock()
			beforeRoot := serverAdminRootSnapshot(t, f.root)
			beforeItems, beforeReads, beforeCreates := serverAdminStoreSnapshot(f)
			source, stdin := tt.source(t)
			args := append([]string{"server-admin", "run", "--out", outPath}, tt.flags...)
			args = append(args, source)
			code, stdout, stderr := runServerAdminFixture(f, stdin, args...)
			if code != ExitOK || stderr != "" {
				t.Fatal("valid server-admin command did not complete cleanly")
			}
			wantCommand := tt.command
			if wantCommand == "" {
				wantCommand = command
			}
			body, err := os.ReadFile(outPath)
			if err != nil || string(body) != admin198Secret {
				t.Fatal("reply body was not written exactly to the output file")
			}
			info, err := os.Stat(outPath)
			f.mu.Lock()
			payload := append([]byte(nil), f.serverAdminPayload...)
			since := append([]string(nil), f.serverAdminSyncSince...)
			requests := f.httpRequests
			outputMode := f.serverAdminOutputMode
			createFlags := f.serverAdminOutputCreateFlags
			createMode := f.serverAdminOutputCreateMode
			createActualMode := f.serverAdminOutputCreateActualMode
			sendRoom := f.serverAdminSendRoom
			registrationEffects := f.issues + f.auths + f.registrations + f.redactions
			subscriptionWrites := f.subscriptionWrites
			f.mu.Unlock()
			if err != nil || info.Mode().Perm() != 0600 || outputMode != 0600 || createActualMode != 0600 || createMode != 0600 || createFlags&(os.O_CREATE|os.O_EXCL) != os.O_CREATE|os.O_EXCL {
				t.Fatal("reply file was not created exclusively with mode 0600 and private at send")
			}
			if tt.roomID != "" && sendRoom != tt.roomID {
				t.Fatal("server-admin send URL did not use the configured room ID")
			}
			if registrationEffects != 0 || subscriptionWrites != 0 || !reflect.DeepEqual(beforeRoot, serverAdminRootSnapshot(t, f.root)) {
				t.Fatal("command changed registration, binding, hook, mailbox, journal, or consumer state")
			}
			items, reads, creates := serverAdminStoreSnapshot(f)
			if !reflect.DeepEqual(beforeItems, items) || !reflect.DeepEqual(beforeReads, reads) || !reflect.DeepEqual(beforeCreates, creates) {
				t.Fatal("command changed the credential store")
			}
			assertServerAdminPayload(t, payload, wantCommand)
			if requests == 0 || len(since) < 2 || since[0] != "" || since[1] != "baseline" {
				t.Fatal("send did not follow a pre-send sync baseline and its cursor")
			}
			if fields := strings.Fields(stdout); len(fields) != 5 {
				t.Fatal("success stdout did not contain exactly five fields")
			} else {
				digest := sha256.Sum256(body)
				if fields[0] != "sent_event_id=$admin-sent" || fields[1] != "reply_event_id="+tt.wantID || fields[2] != "reply_bytes="+strconv.Itoa(len(body)) || fields[3] != "reply_sha256="+hex.EncodeToString(digest[:]) {
					t.Fatal("success stdout fields did not match the reply receipt")
				}
				gotPath, err := strconv.Unquote(strings.TrimPrefix(fields[4], "out="))
				if err != nil || gotPath != outPath {
					t.Fatal("success stdout did not report the output path")
				}
			}
			f.assertNoExports([]byte(stdout), []byte(stderr))
		})
	}
}

// A stdout failure after the reply is saved removes the private output file.
func TestServerAdminStdoutFailureRemovesReply(t *testing.T) {
	f := newAdmin198Backend(t, "server-admin", "file")
	outPath := serverAdminOutPath(t)
	f.mu.Lock()
	f.serverAdminOutputPath = outPath
	f.mu.Unlock()
	writer := &serverAdminFailWriter{}
	var stderr bytes.Buffer
	code := runServerAdminFixtureWithWriters(f, nil, writer, &stderr, serverAdminRunArgs(outPath, "!admin status")...)
	if code == ExitOK || !writer.called || !strings.Contains(stderr.String(), "server-admin stage=stdout") {
		t.Fatal("stdout write failure did not return the stdout refusal")
	}
	f.assertNoExports(nil, stderr.Bytes())
	if _, err := os.Lstat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stdout write failure left the reply output file")
	}
	f.mu.Lock()
	sent := f.serverAdminSent
	f.mu.Unlock()
	if !sent {
		t.Fatal("stdout failure fixture did not receive the admin command")
	}
}

// A selected identity whose homeserver Whoami differs from the binding cannot send.
func TestServerAdminWhoamiMismatchStopsSend(t *testing.T) {
	f := newAdmin198Backend(t, "server-admin-whoami-mismatch", "file")
	outPath := serverAdminOutPath(t)
	f.mu.Lock()
	f.serverAdminOutputPath = outPath
	f.mu.Unlock()
	code, stdout, stderr := runServerAdminFixture(f, nil, serverAdminRunArgs(outPath, "!admin status")...)
	if code == ExitOK || stdout != "" || !strings.Contains(stderr, "server-admin stage=session_identity") {
		t.Fatal("Whoami mismatch did not refuse the bound admin identity")
	}
	f.mu.Lock()
	sends := f.sends
	f.mu.Unlock()
	if sends != 0 {
		t.Fatal("Whoami mismatch reached the message send endpoint")
	}
	if _, err := os.Lstat(outPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Whoami mismatch left an output file")
	}
}

// A widened output file supplied at the open boundary is rejected and removed.
func TestServerAdminRefusesWidenedOutputMode(t *testing.T) {
	path := serverAdminOutPath(t)
	openWide := func(path string, flags int, _ os.FileMode) (*os.File, error) {
		file, err := os.OpenFile(path, flags, 0644)
		if err != nil {
			return nil, err
		}
		if err := file.Chmod(0644); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
	file, err := reserveServerAdminOutput(path, openWide)
	if file != nil {
		_ = file.Close()
	}
	if err == nil || err.Error() != "server-admin stage=output_mode" {
		t.Fatal("widened output mode passed the creation boundary")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("widened output mode refusal left a file")
	}
}

// Invalid command text, missing binding, and an existing output path must stop before any HTTP request.
func TestServerAdminPreflightRefusalsMakeNoRequests(t *testing.T) {
	tests := []struct {
		name    string
		command string
		missing bool
		invalid bool
		exists  bool
	}{
		{name: "missing prefix", command: "admin status"},
		{name: "wrong prefix separator", command: "!admin\tstatus"},
		{name: "newline", command: "!admin status\nnext"},
		{name: "control", command: "!admin status\x01"},
		{name: "invalid UTF-8", command: "!admin " + string([]byte{0xff})},
		{name: "over 1024 bytes", command: "!admin " + strings.Repeat("x", 1018)},
		{name: "missing binding", command: "!admin status", missing: true},
		{name: "invalid binding", command: "!admin status", invalid: true},
		{name: "existing output", command: "!admin status", exists: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAdmin198Backend(t, "server-admin", "file")
			outPath := serverAdminOutPath(t)
			if tt.missing || tt.invalid {
				if tt.missing {
					f.c.AdminRegistration = nil
				} else {
					f.c.AdminRegistration.ReplyDeadlineSeconds = 31
				}
				f.save()
			}
			if tt.exists {
				if err := os.WriteFile(outPath, []byte("preserve me"), 0600); err != nil {
					t.Fatal("existing output fixture could not be created")
				}
			}
			code, stdout, stderr := runServerAdminFixture(f, nil, serverAdminRunArgs(outPath, tt.command)...)
			if code == ExitOK || stdout != "" || stderr == "" {
				t.Fatal("invalid server-admin request was not refused")
			}
			f.mu.Lock()
			requests := f.httpRequests
			f.mu.Unlock()
			if requests != 0 {
				t.Fatal("preflight refusal made an HTTP request")
			}
			if tt.exists {
				body, err := os.ReadFile(outPath)
				if err != nil || string(body) != "preserve me" {
					t.Fatal("existing output was changed")
				}
			} else if _, err := os.Lstat(outPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused command left an output file")
			}
		})
	}
}

// An unexpected member or a room state naming the wrong bot prevents the command send.
func TestServerAdminRoomCheckStopsSend(t *testing.T) {
	for _, mode := range []string{"extra-member", "wrong-bot"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmin198Backend(t, mode, "file")
			outPath := serverAdminOutPath(t)
			f.mu.Lock()
			f.serverAdminOutputPath = outPath
			f.mu.Unlock()
			beforeRoot := serverAdminRootSnapshot(t, f.root)
			beforeItems, beforeReads, beforeCreates := serverAdminStoreSnapshot(f)
			code, stdout, stderr := runServerAdminFixture(f, nil, serverAdminRunArgs(outPath, "!admin status")...)
			if code == ExitOK || stdout != "" || !strings.Contains(stderr, "server-admin stage=room_check") {
				t.Fatal("invalid admin room was not refused")
			}
			f.mu.Lock()
			sends := f.sends
			f.mu.Unlock()
			if sends != 0 {
				t.Fatal("room check failure reached the message send endpoint")
			}
			if _, err := os.Lstat(outPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("room refusal left an output file")
			}
			if !reflect.DeepEqual(beforeRoot, serverAdminRootSnapshot(t, f.root)) {
				t.Fatal("room refusal changed local binding, hooks, mailbox, journal, or consumer state")
			}
			items, reads, creates := serverAdminStoreSnapshot(f)
			if !reflect.DeepEqual(beforeItems, items) || !reflect.DeepEqual(beforeReads, reads) || !reflect.DeepEqual(beforeCreates, creates) {
				t.Fatal("room refusal changed the credential store")
			}
		})
	}
}

// Wrong-sender, unrelated, and pre-send history events never satisfy the post-send reply contract.
func TestServerAdminIgnoresUnrelatedRepliesAndHistory(t *testing.T) {
	for _, mode := range []string{"server-admin-wrong-sender", "server-admin-unrelated", "server-admin-stale-history"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdmin198Backend(t, mode, "file")
			outPath := serverAdminOutPath(t)
			f.mu.Lock()
			f.serverAdminOutputPath = outPath
			f.mu.Unlock()
			beforeRoot := serverAdminRootSnapshot(t, f.root)
			beforeItems, beforeReads, beforeCreates := serverAdminStoreSnapshot(f)
			code, stdout, stderr := runServerAdminFixture(f, nil, serverAdminRunArgs(outPath, "!admin status")...)
			if code == ExitOK || stdout != "" || !strings.Contains(stderr, "server-admin stage=no_reply") {
				t.Fatal("unrelated event was accepted instead of reaching no_reply")
			}
			f.mu.Lock()
			sent, since := f.serverAdminSent, append([]string(nil), f.serverAdminSyncSince...)
			f.mu.Unlock()
			if !sent || len(since) < 2 || since[0] != "" || since[1] != "baseline" {
				t.Fatal("server-admin did not establish and use a post-send sync cursor")
			}
			if _, err := os.Lstat(outPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("no_reply left an output file")
			}
			if !reflect.DeepEqual(beforeRoot, serverAdminRootSnapshot(t, f.root)) {
				t.Fatal("no_reply changed local binding, hooks, mailbox, journal, or consumer state")
			}
			items, reads, creates := serverAdminStoreSnapshot(f)
			if !reflect.DeepEqual(beforeItems, items) || !reflect.DeepEqual(beforeReads, reads) || !reflect.DeepEqual(beforeCreates, creates) {
				t.Fatal("no_reply changed the credential store")
			}
			f.assertNoExports([]byte(stdout), []byte(stderr))
		})
	}
}
