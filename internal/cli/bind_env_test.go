package cli

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Proves the detached consumer environment is an allowlist: session-scoped
// runtime variables and registration secrets are dropped, process basics and
// identity/routing selectors pass, and the explicit config root and session
// id replace inherited values exactly once. Pure function; the spawn path is
// covered by TestBindDetachedLifecycle.
func TestDetachedConsumerEnv(t *testing.T) {
	inherited := []string{
		"HOME=/home/alice", "PATH=/usr/bin:/bin", "TMPDIR=/tmp/x", "USER=alice", "LANG=C.UTF-8", "LC_ALL=C", "LC_CTYPE=UTF-8", "TZ=UTC",
		"HTTPS_PROXY=http://proxy:3128", "no_proxy=localhost", "SSL_CERT_FILE=/etc/ca.pem",
		"A2A_AGENT=alice", "A2A_MATRIX_URL=https://hs.example.invalid", "A2A_MATRIX_SERVER_NAME=example.invalid", "A2A_SECRET_STORE=file",
		"CLAUDE_CODE_MESSAGING_TOKEN=planted", "CLAUDE_CODE_MESSAGING_SOCKET=/tmp/sock", "CLAUDE_CODE_SESSION_ID=planted", "CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_EXECPATH=/x", "CLAUDECODE=1", "CLAUDE_PID=1", "CLAUDE_EFFORT=high", "CLAUDE_ENV_FILE=/x",
		"CLAUDE_PROJECT_DIR=/x", "CLAUDE_CONFIG_DIR=/x", "CLAUDE_CODE_OAUTH_TOKEN=planted", "ANTHROPIC_API_KEY=planted",
		"CODEX_THREAD_ID=planted", "CODEX_HOME=/x", "OPENAI_API_KEY=planted",
		"A2A_MATRIX_REGISTRATION_TOKEN=planted", "A2A_CONFIG_DIR=/inherited", "A2A_SESSION_ID=inherited",
		"TMUX=/tmp/tmux,1,0", "TMUX_PANE=%1", "SSH_AUTH_SOCK=/tmp/agent", "GITHUB_TOKEN=planted", "malformed-without-equals",
	}
	got := detachedConsumerEnv(inherited, "/cfg", "session-1")

	want := []string{
		"HOME=/home/alice", "PATH=/usr/bin:/bin", "TMPDIR=/tmp/x", "USER=alice", "LANG=C.UTF-8", "LC_ALL=C", "LC_CTYPE=UTF-8", "TZ=UTC",
		"HTTPS_PROXY=http://proxy:3128", "no_proxy=localhost", "SSL_CERT_FILE=/etc/ca.pem",
		"A2A_AGENT=alice", "A2A_MATRIX_URL=https://hs.example.invalid", "A2A_MATRIX_SERVER_NAME=example.invalid", "A2A_SECRET_STORE=file",
		"A2A_CONFIG_DIR=/cfg", "A2A_SESSION_ID=session-1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("detached env\n got %q\nwant %q", got, want)
	}
	for _, kv := range got {
		if strings.Contains(kv, "planted") || strings.Contains(kv, "inherited") {
			t.Fatalf("session-scoped or secret value survived: %q", kv)
		}
	}
}

// Proves an empty parent environment still yields the explicit selectors, so
// the consumer never falls back to an ambient config root or session.
func TestDetachedConsumerEnvEmpty(t *testing.T) {
	got := detachedConsumerEnv(nil, "/cfg", "s")
	if want := []string{"A2A_CONFIG_DIR=/cfg", "A2A_SESSION_ID=s"}; !slices.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

// processEnviron reads a same-UID process environment for tests: exact on
// Linux, whitespace-split `ps -E` on Darwin (values are compared, never logged).
func processEnviron(t *testing.T, pid int) []string {
	t.Helper()
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ"); err == nil {
		return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("process environment inspection unsupported on " + runtime.GOOS)
	}
	out, err := exec.Command("ps", "-wwE", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("inspect detached consumer environment: %v", err)
	}
	return strings.Fields(string(out))
}
