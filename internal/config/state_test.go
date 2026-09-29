package config

import (
	"strconv"
	"testing"
)

// Proves the legacy pending batch and acked ring (pre-mailbox state files)
// and the legacy subscription map still load from the 0600 state file, so the
// first sync can migrate them.
func TestStateLegacyRoundTrip(t *testing.T) {
	p := testPaths(t)
	want := State{NextBatch: "s1", LegacyPending: []byte(`{"next_batch":"s2","events":["$e"]}`), LegacyAcked: []string{"$a"},
		LegacySubscriptions: map[string][]string{LegacyAllRooms: {"system"}}}
	if err := SaveState(p, "a", want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p, "a")
	if err != nil || len(got.LegacyPending) == 0 || got.LegacyAcked[0] != "$a" || got.LegacySubscriptions["*"][0] != "system" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
}

// Proves the Stop-hook loop guard map counts per session, forgets a session
// set to 0 (the other one stays), and keeps only the HookSessionsMax most
// recently updated sessions.
func TestStateHookSessions(t *testing.T) {
	var s State
	s.SetHookBlocks("a", 1)
	s.SetHookBlocks("b", 2)
	s.SetHookBlocks("a", 3)
	if s.HookBlocks("a") != 3 || s.HookBlocks("b") != 2 || s.HookBlocks("c") != 0 {
		t.Fatalf("counts = %+v", s.HookSessions)
	}
	s.SetHookBlocks("a", 0)
	if s.HookBlocks("a") != 0 || s.HookBlocks("b") != 2 || len(s.HookSessions) != 1 {
		t.Fatalf("after reset = %+v", s.HookSessions)
	}
	s.SetHookBlocks("b", 0)
	if s.HookSessions != nil {
		t.Fatalf("empty guard map not dropped: %+v", s.HookSessions)
	}
	for i := 0; i < HookSessionsMax+3; i++ {
		s.SetHookBlocks("s"+strconv.Itoa(i), 1)
	}
	if len(s.HookSessions) != HookSessionsMax || s.HookBlocks("s2") != 0 || s.HookBlocks("s3") != 1 {
		t.Fatalf("bound not kept: %d entries", len(s.HookSessions))
	}
}
