//go:build linux || darwin

package tmuxwake

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubMuse writes a `muse` shell double into dir. It records argv, the
// send body from stdin and the carrier-relevant environment of every call;
// the file MUSE_STUB_DIR/mode selects its reply: "ok", "ok-token" (exit 0
// mentioning a refusal token), "sleep", "xdg" (the sessions registry honors
// XDG_DATA_HOME), or any refusal token printed as an error_code with exit 1.
func stubMuse(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$MUSE_STUB_DIR/argv.log"
printf 'MUSE_NO_AUTO_UPDATE=%s XDG_DATA_HOME=%s\n' "$MUSE_NO_AUTO_UPDATE" "$XDG_DATA_HOME" >> "$MUSE_STUB_DIR/env.log"
mode=$(cat "$MUSE_STUB_DIR/mode")
if [ "$1" = "sessions" ]; then
  case "$mode" in
    sleep) sleep 5 ;;
    xdg)
      if [ "$XDG_DATA_HOME" = "$MUSE_STUB_DIR/wantdata" ]; then
        printf '{"sessions":[{"sessionId":"muse-session-1"}]}'
      else
        printf '{"sessions":[]}'
      fi ;;
    *) printf '{"sessions":[{"sessionId":"muse-session-1"}]}' ;;
  esac
  exit 0
fi
if [ "$1" = "session-message" ]; then
  cat > "$MUSE_STUB_DIR/stdin.log"
  case "$mode" in
    ok) printf '{"ok":true}'; exit 0 ;;
    ok-token) printf '{"note":"ingress_closed sender_unverified"}'; exit 0 ;;
    sleep) sleep 5; exit 0 ;;
    *) printf '{"error_code":"%s"}' "$mode"; exit 1 ;;
  esac
fi
exit 2
`
	if err := os.WriteFile(filepath.Join(dir, "muse"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func stubMode(t *testing.T, dir, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stubLog(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The production sender, found on PATH like the real launcher: exact argv
// for both invocations, the body on stdin, and MUSE_NO_AUTO_UPDATE=1 in
// the environment of both calls even though the caller set 0.
func TestExecMuseSenderOnPath(t *testing.T) {
	dir := stubMuse(t)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MUSE_STUB_DIR", dir)
	t.Setenv("MUSE_NO_AUTO_UPDATE", "0")
	sender := ExecMuseSender{}
	live, err := sender.SessionLive(context.Background(), "muse-session-1")
	if err != nil || !live {
		t.Fatalf("SessionLive = %t, %v", live, err)
	}
	if live, err := sender.SessionLive(context.Background(), "muse-session-9"); err != nil || live {
		t.Fatalf("SessionLive unknown = %t, %v", live, err)
	}
	if err := sender.SendSessionMessage(context.Background(), "muse-session-1", Notice(2)); err != nil {
		t.Fatalf("SendSessionMessage: %v", err)
	}
	argv := strings.Split(strings.TrimRight(stubLog(t, dir, "argv.log"), "\n"), "\n")
	want := []string{"sessions --json", "sessions --json", "session-message send --target muse-session-1 --json"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
	if got := stubLog(t, dir, "stdin.log"); got != Notice(2) {
		t.Fatalf("stdin = %q, want the fixed notice", got)
	}
	envLines := strings.Split(strings.TrimRight(stubLog(t, dir, "env.log"), "\n"), "\n")
	if len(envLines) != 3 {
		t.Fatalf("env log = %q", envLines)
	}
	for _, line := range envLines {
		if !strings.Contains(line, "MUSE_NO_AUTO_UPDATE=1") {
			t.Fatalf("invocation ran without the auto-update opt-out: %q", line)
		}
	}
}

// Every known refusal token, returned with a non-zero exit, is a classified
// sentinel — never a submitted wake.
func TestExecMuseSenderRefusals(t *testing.T) {
	dir := stubMuse(t)
	sender := ExecMuseSender{Binary: filepath.Join(dir, "muse"), Env: []string{"MUSE_STUB_DIR=" + dir, "PATH=" + os.Getenv("PATH")}}
	for token, want := range map[string]error{
		"sender_unverified":             ErrMuseSenderUnverified,
		"external_agent_ingress_closed": ErrMuseIngressClosed,
		"target_not_found":              ErrMuseSessionDead,
		"target_not_running":            ErrMuseSessionDead,
		"wrong_target":                  ErrMuseSessionDead,
		"ambiguous_target_name":         ErrMuseSessionDead,
		"admission_queue_full":          ErrMuseUnavailable,
		"admission_retry_later":         ErrMuseUnavailable,
		"target_unavailable":            ErrMuseUnavailable,
		"target_unverified":             ErrMuseUnavailable,
		"self_send_wrong_path":          ErrMuseUnavailable,
		"session_messaging_unavailable": ErrMuseUnavailable,
		"some_unlisted_failure":         ErrMuseUnavailable,
	} {
		stubMode(t, dir, token)
		if err := sender.SendSessionMessage(context.Background(), "muse-session-1", "body"); !errors.Is(err, want) {
			t.Fatalf("token %s: err = %v, want %v", token, err, want)
		}
	}
}

// Exit 0 is success even when the reply text mentions refusal tokens:
// classification runs only on a non-zero exit.
func TestExecMuseSenderExitZeroIgnoresTokens(t *testing.T) {
	dir := stubMuse(t)
	stubMode(t, dir, "ok-token")
	sender := ExecMuseSender{Binary: filepath.Join(dir, "muse"), Env: []string{"MUSE_STUB_DIR=" + dir, "PATH=" + os.Getenv("PATH")}}
	if err := sender.SendSessionMessage(context.Background(), "muse-session-1", "body"); err != nil {
		t.Fatalf("exit-0 reply classified as a refusal: %v", err)
	}
}

// The registry the sender reads follows XDG_DATA_HOME in its explicit
// environment, so a consumer carrying the bind-time value sees the same
// registry PinMuse admitted; without it the same session reads dead.
func TestExecMuseSenderRegistryEnv(t *testing.T) {
	dir := stubMuse(t)
	stubMode(t, dir, "xdg")
	base := []string{"MUSE_STUB_DIR=" + dir, "PATH=" + os.Getenv("PATH")}
	without := ExecMuseSender{Binary: filepath.Join(dir, "muse"), Env: base}
	if live, err := without.SessionLive(context.Background(), "muse-session-1"); err != nil || live {
		t.Fatalf("without XDG_DATA_HOME: live=%t err=%v, want dead", live, err)
	}
	with := ExecMuseSender{Binary: filepath.Join(dir, "muse"), Env: append(append([]string{}, base...), "XDG_DATA_HOME="+filepath.Join(dir, "wantdata"))}
	if live, err := with.SessionLive(context.Background(), "muse-session-1"); err != nil || !live {
		t.Fatalf("with XDG_DATA_HOME: live=%t err=%v, want live", live, err)
	}
	if env := stubLog(t, dir, "env.log"); !strings.Contains(env, "XDG_DATA_HOME="+filepath.Join(dir, "wantdata")) {
		t.Fatalf("XDG_DATA_HOME did not reach the invocation: %q", env)
	}
}

// A hanging Muse invocation is bounded by the command timeout plus the
// wait delay, for liveness and for delivery alike.
func TestExecMuseSenderTimeout(t *testing.T) {
	dir := stubMuse(t)
	stubMode(t, dir, "sleep")
	old := museCommandTimeout
	museCommandTimeout = 200 * time.Millisecond
	t.Cleanup(func() { museCommandTimeout = old })
	sender := ExecMuseSender{Binary: filepath.Join(dir, "muse"), Env: []string{"MUSE_STUB_DIR=" + dir, "PATH=" + os.Getenv("PATH")}}
	start := time.Now()
	if _, err := sender.SessionLive(context.Background(), "muse-session-1"); !errors.Is(err, ErrMuseUnavailable) {
		t.Fatalf("SessionLive timeout err = %v, want ErrMuseUnavailable", err)
	}
	if err := sender.SendSessionMessage(context.Background(), "muse-session-1", "body"); !errors.Is(err, ErrMuseUnavailable) {
		t.Fatalf("SendSessionMessage timeout err = %v, want ErrMuseUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("two timed-out invocations took %s; the timeout and wait delay must bound them", elapsed)
	}
}
