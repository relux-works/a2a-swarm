package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Proves identity ensure --runtime muse onboards like the other session
// runtimes: Muse hooks in .muse/hooks.json, bind wake guidance, and exactly
// the standard baseline subscriptions (project and lobby) — onboarding
// creates no muse-specific or otherwise forced subscription beyond the
// baseline and the standing host-principal rule (none configured here, so
// the summary lists exactly those two rows). A re-run creates nothing new.
func TestEnsureMuseOnboarding(t *testing.T) {
	e := newEnv(t)
	e.extra["USER"] = "ops"
	out := e.ensure("--role", "orch", "--project", "proj", "--runtime", "muse").ok(t).stdout
	hooks := filepath.Join(e.projDir, ".muse", "hooks.json")
	for _, want := range []string{
		"hooks (muse, --runtime):\ninstalled " + hooks + " (Muse): hooks SessionStart, UserPromptSubmit, Stop for --as mini_proj_orch\n",
		"human step: Muse project hooks run only after native human trust",
		"For idle wake, bind this session: `a2a bind --runtime muse --owner-pid <own muse pid>` (session id from MUSE_SESSION_ID)",
		"identity:  mini_proj_orch\n",
		"hooks:     " + hooks + " (Muse)\n",
	} {
		tuitestkit.ContainsStr(t, out, want)
	}
	subs := e.subsOf("mini_proj_orch")
	if len(subs) != 2 || subs[0].Room != "!projRoomID" || subs[1].Room != "!lobbyRoomID" ||
		kindSet(subs[0].Kinds) != kindSet(onboardKinds) || kindSet(subs[1].Kinds) != kindSet(onboardKinds) ||
		subs[0].From != "" || subs[1].From != "" || subs[0].AllEvents || subs[1].AllEvents {
		t.Fatalf("muse onboarding subscriptions: %+v", subs)
	}
	summary := out[strings.Index(out, "== a2a onboarding summary =="):]
	for _, sub := range subs {
		tuitestkit.ContainsStr(t, out, "created "+sub.ID+"\t")
		tuitestkit.ContainsStr(t, summary, sub.ID+"\t"+roomSummaryTail(sub)+"\n")
	}
	again := e.ensure("--role", "orch", "--project", "proj", "--runtime", "muse").ok(t).stdout
	tuitestkit.ContainsStr(t, again, "already "+subs[0].ID+"\t")
	tuitestkit.ContainsStr(t, again, "unchanged "+hooks)
	if n := len(e.subsOf("mini_proj_orch")); n != 2 {
		t.Fatalf("%d subscriptions after the second muse ensure", n)
	}
}

// roomSummaryTail is the summary-list rendering of sub after its id: the
// ensure summary prints `id<TAB><summary>` lines for every row in effect.
func roomSummaryTail(sub Sub) string {
	label := "#proj"
	if sub.Room == "!lobbyRoomID" {
		label = "#lobby"
	}
	return sub.summary(label, testServer)
}
