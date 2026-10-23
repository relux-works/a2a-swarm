package cli

import (
	"strings"
	"testing"
)

// Both public onboarding paths give conditional single-consumer advice, never
// a blanket waiter instruction. Claude alone gets the unbound fallback; Codex
// cannot claim a waiter wakes idle inference. Services get no wake advice.
// All traffic and settings writes here use the synthetic test environment.
func TestOnboardingWakeGuidance(t *testing.T) {
	for _, product := range []bool{false, true} {
		for _, runtime := range []string{"claude", "codex", "none"} {
			name := "plain/" + runtime
			if product {
				name = "preset/" + runtime
			}
			t.Run(name, func(t *testing.T) {
				e := newEnv(t)
				e.swarm()
				args := []string{"--role", "orch", "--project", "proj", "--runtime", runtime}
				if product {
					uid := e.agent("fixture")
					e.as("fixture", "broadcast-room", "init").ok(t)
					e.as("fixture", "product", "new", "alpha").ok(t)
					e.hs.makePublic(e.hs.addRoom("alpha-proj", uid))
					args = append(args, "--product", "alpha", "--preset", "orch")
				}
				out := e.ensure(args...).ok(t).stdout
				if strings.Contains(out, "Also keep one background") {
					t.Fatal("unconditional waiter advice survived")
				}
				if runtime == "none" {
					for _, forbidden := range []string{"a2a wait", "a2a bind status", "supervised consumer"} {
						if strings.Contains(out, forbidden) {
							t.Fatalf("service received session advice: %s", forbidden)
						}
					}
					return
				}
				for _, required := range []string{"a2a inbox", "a2a bind status", "live binding", "do not start a parallel wait/watch or Monitor"} {
					if !strings.Contains(out, required) {
						t.Fatalf("missing single-consumer guidance: %s", required)
					}
				}
				if runtime == "claude" {
					if !strings.Contains(out, "Only for an unbound Claude fallback") || !strings.Contains(out, "a2a wait --timeout 0") || strings.Contains(out, "idle Codex") {
						t.Fatal("wrong Claude fallback advice")
					}
				} else if !strings.Contains(out, "a background `a2a wait` alone cannot wake idle Codex") || strings.Contains(out, "a2a wait --timeout 0") {
					t.Fatal("wrong Codex wake advice")
				}
			})
		}
	}
}
