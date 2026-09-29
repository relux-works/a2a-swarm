//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// Proves UpdateStateBy's bounded wait: while another holder keeps the state
// lock it returns ErrLockTimeout by about the deadline without running fn or
// writing the file; once the lock is free it runs fn and saves (positive control).
func TestUpdateStateByTimesOut(t *testing.T) {
	p := testPaths(t)
	if err := SaveState(p, "a", State{NextBatch: "s1"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(p.StateFile("a")), "a.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ran := false
	start := time.Now()
	_, err = UpdateStateBy(p, "a", time.Now().Add(100*time.Millisecond), func(s *State) error { ran = true; s.NextBatch = "s2"; return nil })
	if !errors.Is(err, ErrLockTimeout) || ran || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v, ran %v after %v", err, ran, time.Since(start))
	}
	if st, _ := LoadState(p, "a"); st.NextBatch != "s1" {
		t.Fatalf("state written under a held lock: %+v", st)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if st, err := UpdateStateBy(p, "a", time.Now().Add(100*time.Millisecond), func(s *State) error { s.NextBatch = "s2"; return nil }); err != nil || st.NextBatch != "s2" {
		t.Fatalf("free lock: %+v, %v", st, err)
	}
}

// Proves a hook session entry keeps its rung set bounded to HookRungMax, keeps
// the rest of the entry when the block counter changes, and is forgotten only
// when it is entirely empty.
func TestStateHookSessionRung(t *testing.T) {
	var s State
	var rung []string
	for i := 0; i < HookRungMax+5; i++ {
		rung = append(rung, "$e"+strconv.Itoa(i))
	}
	s.PutHookSession(HookSession{ID: "a", Rung: rung, LastBlock: 7})
	if h := s.HookSession("a"); len(h.Rung) != HookRungMax || h.Rung[0] != "$e0" || h.LastBlock != 7 {
		t.Fatalf("entry = %+v", h)
	}
	s.SetHookBlocks("a", 2)
	s.SetHookBlocks("a", 0)
	if h := s.HookSession("a"); h.Blocks != 0 || h.LastBlock != 7 || len(h.Rung) != HookRungMax {
		t.Fatalf("counter change lost the entry: %+v", h)
	}
	s.PutHookSession(HookSession{ID: "a"})
	if s.HookSessions != nil {
		t.Fatalf("empty entry kept: %+v", s.HookSessions)
	}
}
