package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// The common defaults are the Source-selected hook policy, not an unbounded
// manual-bind policy. Public CLI acceptance/refusal is paired in SessionAndPolicy.
func TestBindUsabilityDefaultPolicy(t *testing.T) {
	if defaultBindInterval != 2*time.Second || defaultBindRetryMax != 30*time.Second || defaultBindMaxWakes != 30 {
		t.Fatalf("default policy = %v %v %d", defaultBindInterval, defaultBindRetryMax, defaultBindMaxWakes)
	}
}

// Every stage projects to the closed set and wrapped contention retains lock.
// Arbitrary error text and unrecognized stage values can never become a cause.
// This is classification coverage, not lock or dispatch fault injection.
func TestBindUsabilityRetryCauseClosedSet(t *testing.T) {
	sentinel := errors.New("PRIVATE_ERROR_SENTINEL/path/token/body\nINJECTED")
	for _, stage := range []string{"sync", "state", "lock", "lease", "connection", "dispatch", "PRIVATE_ERROR_SENTINEL\nINJECTED", ""} {
		t.Run(fmt.Sprintf("stage-%q", stage), func(t *testing.T) {
			want := stage
			if stage == "lock" || stage == "" || stage == "PRIVATE_ERROR_SENTINEL\nINJECTED" {
				want = "state"
			}
			if got := bindingRetryCause(stage, sentinel); got != want {
				t.Fatalf("cause=%q want=%q", got, want)
			}
			if got := bindingRetryCause(stage, fmt.Errorf("PRIVATE_ERROR_SENTINEL: %w", config.ErrLockTimeout)); got != "lock" {
				t.Fatalf("wrapped lock=%q", got)
			}
		})
	}
}

// Existing explicit/hook/Codex selectors retain precedence, including their
// existing A2A sanitization and invalid-Codex PID fallback. Claude is considered
// only when both higher-priority environment selectors are absent.
func TestBindUsabilityLeasePrecedence(t *testing.T) {
	for _, tc := range []struct{ name, hook, a2a, codex, claude, want string }{
		{name: "hook", hook: "hook", a2a: "explicit", codex: "codex", claude: "native", want: "hook"},
		{name: "a2a", a2a: "explicit", codex: "codex", claude: "native", want: "explicit"},
		{name: "codex", codex: "codex", claude: "native", want: "codex"},
		{name: "claude", claude: "native", want: "native"},
		{name: "invalid-claude", claude: "../bad"},
		{name: "invalid-codex", codex: "../bad", claude: "native"},
		{name: "invalid-a2a", a2a: "../bad", claude: "native", want: "../bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{Getenv: func(k string) string {
				switch k {
				case EnvSessionID:
					return tc.a2a
				case "CODEX_THREAD_ID":
					return tc.codex
				case "CLAUDE_CODE_SESSION_ID":
					return tc.claude
				}
				return ""
			}}
			got := a.leaseHolder(config.Paths{}, tc.hook, time.Time{})
			if tc.want != "" {
				if got != tc.want {
					t.Fatalf("holder=%q want=%q", got, tc.want)
				}
			} else if !strings.HasPrefix(got, fmt.Sprintf("pid:%d@", os.Getpid())) {
				t.Fatalf("invalid selector bypassed PID fallback: %q", got)
			}
		})
	}
}
