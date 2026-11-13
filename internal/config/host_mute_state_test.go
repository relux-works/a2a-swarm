package config

import "testing"

// The terminal scope generation and draft hold are scalar value copies in the
// reviewed State graph. A resident memo hit must preserve them independently;
// mutating the returned clone cannot change the stored frame or source State.
func TestScopeMuteStateClonePreservesFence(t *testing.T) {
	original := State{Terminal: &TerminalBinding{Generation: "scope", WakeMuteGeneration: 9, WakeMuteHold: true}}
	copied, ok := cloneState(&original)
	if !ok || copied.Terminal == nil || copied.Terminal.WakeMuteGeneration != 9 || !copied.Terminal.WakeMuteHold {
		t.Fatal("state clone lost scope fence metadata")
	}
	copied.Terminal.WakeMuteGeneration = 0
	copied.Terminal.WakeMuteHold = false
	if original.Terminal.WakeMuteGeneration != 9 || !original.Terminal.WakeMuteHold {
		t.Fatal("state clone aliases scope fence metadata")
	}
	p := Paths{Base: t.TempDir(), WakeMuteVersion: 1}
	if err := SaveState(p, "member", original); err != nil {
		t.Fatal(err)
	}
	memo := NewStateDecodeScope("member")
	p = p.WithStateDecodeScope(memo)
	memo.Open()
	defer memo.Close()
	first, err := LoadState(p, "member")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadState(p, "member")
	if err != nil {
		t.Fatal(err)
	}
	if memo.Stats().Hits == 0 || second.Terminal.WakeMuteGeneration != 9 || !second.Terminal.WakeMuteHold || p.WakeMuteVersion != 1 {
		t.Fatal("resident decode lost scope fence metadata")
	}
	first.Terminal.WakeMuteGeneration = 1
	first.Terminal.WakeMuteHold = false
	third, err := LoadState(p, "member")
	if err != nil || third.Terminal.WakeMuteGeneration != 9 || !third.Terminal.WakeMuteHold {
		t.Fatal("resident frame aliases scope fence metadata")
	}
}
