package config

import (
	"testing"
	"time"
)

// Proves cloneState deep-copies the wake-class policy: mutating the clone's
// reservations leaves the source untouched, beside the absent-policy
// control that stays absent.
func TestCloneStateWakeClasses(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	src := &State{WakeClasses: &WakeClassPolicy{Mode: WakeClassEconomy, OwnerPerHour: 20, PeerPerHour: 20, BatchInterval: time.Hour, StartedAt: at, OwnerReservations: []time.Time{at}, PeerReservations: []time.Time{at, at}}}
	got, ok := cloneState(src)
	if !ok || got.WakeClasses == nil {
		t.Fatalf("clone refused or lost policy: ok=%t %+v", ok, got.WakeClasses)
	}
	got.WakeClasses.OwnerReservations[0] = at.Add(time.Hour)
	got.WakeClasses.PeerReservations = append(got.WakeClasses.PeerReservations, at)
	got.WakeClasses.Reason = "changed"
	if !src.WakeClasses.OwnerReservations[0].Equal(at) || len(src.WakeClasses.PeerReservations) != 2 || src.WakeClasses.Reason != "" {
		t.Fatalf("clone shares policy memory with source: %+v", src.WakeClasses)
	}
	got, ok = cloneState(&State{})
	if !ok || got.WakeClasses != nil {
		t.Fatalf("absent policy not preserved: ok=%t %+v", ok, got.WakeClasses)
	}
}

// Proves per-class reservations survive a restart (save/load) and a binding
// generation replacement (a Terminal-only state update), exactly like the
// legacy hourly reservations beside them.
func TestWakeClassesSurviveRestartAndGenerationReplacement(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := State{
		Terminal:    &TerminalBinding{Generation: "one", Reservations: []time.Time{at}},
		WakeClasses: &WakeClassPolicy{Mode: WakeClassEconomy, OwnerPerHour: 20, PeerPerHour: 20, BatchInterval: 30 * time.Minute, StartedAt: at, LastBatchWake: at, OwnerReservations: []time.Time{at}, PeerReservations: []time.Time{at, at}},
	}
	if err := SaveState(p, "alice", st); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateState(p, "alice", func(cur *State) error {
		cur.Terminal = &TerminalBinding{Generation: "two", Reservations: cur.Terminal.Reservations}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p, "alice")
	if err != nil {
		t.Fatal(err)
	}
	w := got.WakeClasses
	if got.Terminal.Generation != "two" || w == nil || w.Mode != WakeClassEconomy || len(w.OwnerReservations) != 1 || len(w.PeerReservations) != 2 || !w.LastBatchWake.Equal(at) || !w.StartedAt.Equal(at) {
		t.Fatalf("reservations lost across restart/replacement: %+v", w)
	}
}
