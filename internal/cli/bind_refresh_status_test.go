package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func withoutRefreshDiagnostic(st config.State) config.State {
	if st.Terminal != nil {
		b := *st.Terminal
		b.RefreshStatus = ""
		st.Terminal = &b
	}
	return st
}

func refreshStatusFixture(now time.Time) config.State {
	return config.State{
		Terminal: &config.TerminalBinding{Generation: "generation", Host: "fixture-host", DaemonPID: 42, DaemonBirth: "birth", SessionID: "session"},
		Lease:    &config.Lease{Holder: "session", Host: "fixture-host", Expires: now.Add(time.Hour)},
	}
}

// Each live safety refusal has a closed reason. Settled attempts and the
// notification cooldown alone remain safe, preserving the existing gate.
func TestTerminalRefreshBlockReasons(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, want string
		change     func(*config.State)
	}{
		{"settled", "", func(*config.State) {}},
		{"historical-attempt", "", func(st *config.State) {
			st.Terminal.AttemptAt = now
			st.Terminal.NoticeCount = 1
			st.Terminal.Retried = true
		}},
		{"notification-cooldown", "", func(st *config.State) {
			st.Notification = config.Notification{Version: 1, NotBefore: now.Add(30 * time.Second)}
		}},
		{"attempt", "changed_pending_attempt", func(st *config.State) { st.Terminal.AttemptIDs = []string{"event"} }},
		{"typed", "changed_pending_attempt", func(st *config.State) { st.Terminal.Typed = true }},
		{"notice", "changed_pending_attempt", func(st *config.State) { st.Terminal.Notice = "PRIVATE SCREEN" }},
		{"reservation", "changed_reservation", func(st *config.State) {
			st.Notification = config.Notification{Version: 1, NotBefore: now, Reservation: &config.NotificationReservation{Token: strings.Repeat("a", 32), Path: "terminal", Session: "session", At: now, Previous: now}}
		}},
		{"invalid-ledger", "changed_invalid_notification", func(st *config.State) { st.Notification.Version = 99 }},
		{"missing-lease", "changed_lease", func(st *config.State) { st.Lease = nil }},
		{"expired-lease", "changed_lease", func(st *config.State) { st.Lease.Expires = now }},
		{"foreign-holder", "changed_lease", func(st *config.State) { st.Lease.Holder = "other" }},
		{"foreign-lease-host", "changed_lease", func(st *config.State) { st.Lease.Host = "other" }},
		{"generation", "changed_authority", func(st *config.State) { st.Terminal.Generation = "other" }},
		{"birth", "changed_authority", func(st *config.State) { st.Terminal.DaemonBirth = "other" }},
		{"pid", "changed_authority", func(st *config.State) { st.Terminal.DaemonPID++ }},
		{"host", "changed_authority", func(st *config.State) { st.Terminal.Host = "other" }},
		{"stopped", "changed_authority", func(st *config.State) { st.Terminal.Stopped = true }},
		{"unbound", "changed_authority", func(st *config.State) { st.Terminal = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := refreshStatusFixture(now)
			tc.change(&st)
			before, _ := json.Marshal(st)
			if got := terminalRefreshBlockReason(st, "generation", "birth", "fixture-host", 42, now); got != tc.want {
				t.Fatalf("reason=%q want=%q", got, tc.want)
			}
			after, _ := json.Marshal(st)
			if string(before) != string(after) {
				t.Fatal("diagnostic mutated safety state")
			}
		})
	}
}

// The pure recorder deduplicates identical tokens and preserves their JSON
// round-trip. Foreign/retired consumers cannot overwrite them; unknown text is hidden.
func TestTerminalRefreshRecordedStatus(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	st := refreshStatusFixture(now)
	before := withoutRefreshDiagnostic(st)
	for _, token := range []string{"changed_stabilizing", "changed_pending_attempt", "changed_cooldown", "changed_reservation", "changed_lease", "current"} {
		if !recordTerminalRefreshStatus(&st, "generation", "fixture-host", 42, token) || recordTerminalRefreshStatus(&st, "generation", "fixture-host", 42, token) {
			t.Fatal("observation not recorded once")
		}
		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		var restored config.State
		if err := json.Unmarshal(raw, &restored); err != nil || terminalRefreshStatus(restored.Terminal) != token {
			t.Fatalf("lost refresh diagnostic: %v", err)
		}
		if !reflect.DeepEqual(before, withoutRefreshDiagnostic(st)) {
			t.Fatal("refresh diagnostic changed other state")
		}
	}
	for _, tc := range []struct {
		generation, host string
		pid              int
	}{{"other", "fixture-host", 42}, {"generation", "other", 42}, {"generation", "fixture-host", 43}} {
		if recordTerminalRefreshStatus(&st, tc.generation, tc.host, tc.pid, "changed_ready") || terminalRefreshStatus(st.Terminal) != "current" {
			t.Fatal("foreign consumer changed diagnostic")
		}
	}
	for _, unknown := range []string{"changed_connection", "PRIVATE SCREEN\nargv=/private/path"} {
		st.Terminal.RefreshStatus = unknown
		if terminalRefreshStatus(st.Terminal) != "unavailable" {
			t.Fatal("removed or unrecognized metadata escaped closed projection")
		}
	}
	st.Terminal.Stopped = true
	if recordTerminalRefreshStatus(&st, "generation", "fixture-host", 42, "current") || terminalRefreshStatus(st.Terminal) != "unavailable" || terminalRefreshStatus(nil) != "unavailable" {
		t.Fatal("retired or absent consumer reported live refresh")
	}
}

// Cooldown retains safety and post-safety failure causes between checks;
// non-failure and unknown tokens cannot invent a cause.
func TestTerminalRefreshCooldownReason(t *testing.T) {
	for _, reason := range []string{"changed_pending_attempt", "changed_reservation", "changed_lease", "changed_authority", "changed_invalid_notification", "changed_safety_unavailable", "changed_probe_unavailable", "changed_pin_unavailable", "changed_candidate_unavailable"} {
		if got := terminalRefreshCooldownStatus(reason); got != reason {
			t.Fatalf("cooldown hid failure cause %s as %s", reason, got)
		}
	}
	for _, previous := range []string{"", "current", "changed_stabilizing", "private arbitrary data"} {
		if got := terminalRefreshCooldownStatus(previous); got != "changed_cooldown" {
			t.Fatalf("cooldown invented or leaked cause: %s", got)
		}
	}
}
