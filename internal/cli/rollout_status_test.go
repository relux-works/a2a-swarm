package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// A lead-only route arms stuck-notice alerts, not stale-mail alarms. Owner
// and operator neighbors show their distinct scopes in text and JSON.
func TestRolloutAlarmRoutes(t *testing.T) {
	c := config.MailAlarmConfig{Minutes: 20, Owner: "auto", Lead: "lead", LeadRoom: "operations", LeadTopic: "incidents"}
	for _, tc := range []struct {
		name, stale, stuck string
		targets            []config.MailAlarmJob
		roles              []string
	}{
		{"lead-only", "disabled", "partial", nil, []string{"lead"}},
		{"owner-and-lead", "armed", "armed", []config.MailAlarmJob{{Room: "!owner:fixture.invalid"}}, []string{"owner", "lead"}},
		{"operator-and-lead", "armed", "partial", []config.MailAlarmJob{{Target: "operator", Room: "operations", Topic: "alarms"}}, []string{"lead"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := ""
			if len(tc.targets) == 0 {
				reason = "no destinations (no active owner DM)"
			}
			policy := c
			if tc.name == "operator-and-lead" {
				policy.Operator, policy.Room, policy.Topic = "operator", "operations", "alarms"
			}
			got := alarmStatusForPolicy("worker", config.State{}, policy, tc.targets, reason)
			if got.State != tc.stale || got.StuckNotice.State != tc.stuck {
				t.Fatalf("alarm scopes: %+v", got)
			}
			var roles []string
			for _, target := range got.StuckNotice.Targets {
				roles = append(roles, target.Role)
			}
			if !reflect.DeepEqual(roles, tc.roles) {
				t.Fatalf("stuck targets = %v", roles)
			}
			var out bytes.Buffer
			a := &App{Stdout: &out}
			a.printAlarmStatus(got)
			for _, want := range []string{"mail-alarm=" + tc.stale, "stuck-notice=" + tc.stuck, "stuck-notice-target=lead target=lead room=operations topic=incidents"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("text missing %q: %s", want, out.String())
				}
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var decoded mailAlarmStatus
			if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(decoded, got) {
				t.Fatalf("JSON lost route state: %s (%v)", raw, err)
			}
		})
	}
}

// A failed queued stuck notice is visible in its own scope and does not turn
// a disabled stale-mail alarm into an error or inflate its pending count.
func TestRolloutAlarmQueueScopes(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	c := config.MailAlarmConfig{Minutes: 20, Owner: "auto", Lead: "lead", LeadRoom: "operations", LeadTopic: "incidents"}
	st := config.State{MailAlarm: config.MailAlarm{Jobs: []config.MailAlarmJob{{
		ID: "a2a-mail-alarm-" + strings.Repeat("a", 32), Identity: "worker", Host: "fixture-host",
		Kind: strandedAlarmKind, Since: now.Add(-time.Hour).Format(time.RFC3339), At: now,
		Reason: orphanReason, Room: "operations", Target: "lead", Topic: "incidents", Failure: "rejected",
	}}}}
	got := alarmStatusForPolicy("worker", st, c, nil, "no destinations (no active owner DM)")
	if got.State != "disabled" || got.Pending != 0 || got.Outstanding || got.StuckNotice.State != "error" || got.StuckNotice.Pending != 1 || !got.StuckNotice.Outstanding || got.StuckNotice.Reason != "alarm destination rejected delivery" {
		t.Fatalf("mixed alarm scopes: %+v", got)
	}
}

// Set and both status commands expose lead-only arming and keep exit 0.
// No binding is installed, so this command fixture cannot inspect a terminal.
func TestRolloutAlarmCommandStatus(t *testing.T) {
	e := newEnv(t)
	e.agent("worker")
	set := e.as("worker", "mail-alarm", "set", "--lead", "lead", "--lead-room", "operations", "--lead-topic", "incidents").ok(t)
	for _, want := range []string{"mail-alarm=disabled", "stuck-notice=partial", "stuck-notice-target=lead"} {
		if !strings.Contains(set.stdout, want) {
			t.Fatalf("set feedback missing %q: %s", want, set.stdout)
		}
	}
	for _, command := range []string{"mail-alarm", "bind"} {
		text := e.as("worker", command, "status").ok(t).stdout
		if !strings.Contains(text, "mail-alarm=disabled") || !strings.Contains(text, "stuck-notice-target=lead") {
			t.Fatalf("%s text: %s", command, text)
		}
		raw := e.as("worker", command, "status", "--json").ok(t).stdout
		var got mailAlarmStatus
		if command == "bind" {
			var wrapper struct {
				MailAlarm mailAlarmStatus `json:"mail_alarm"`
			}
			if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
				t.Fatal(err)
			}
			got = wrapper.MailAlarm
		} else if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		if got.State != "disabled" || got.StuckNotice.State != "partial" || len(got.StuckNotice.Targets) != 1 || got.StuckNotice.Targets[0].Target != "lead" {
			t.Fatalf("%s JSON: %s", command, raw)
		}
	}
	// A real stale-mail destination is the valid neighbor to lead-only setup.
	set = e.as("worker", "mail-alarm", "set", "--owner", "operations").ok(t)
	if !strings.Contains(set.stdout, "mail-alarm=armed") || !strings.Contains(set.stdout, "stuck-notice=armed") {
		t.Fatalf("owner feedback: %s", set.stdout)
	}
}

// CLI localparts normalize to exact stored MXIDs; display abbreviation is not
// an audit key. Manual and managed duplicates count once and repair adds none.
func TestRolloutPrincipalExistingCoverage(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	const room = "!room:fixture.invalid"
	for _, principal := range []string{"@principal:fixture.invalid", "@delegate:remote.invalid"} {
		for _, spelling := range []string{principal, strings.Split(strings.TrimPrefix(principal, "@"), ":")[0]} {
			from, err := normalizeUser(spelling, "fixture.invalid")
			if err != nil {
				t.Fatal(err)
			}
			row := Sub{ID: "sub-manual", SubSelector: SubSelector{Room: room, From: from, AllEvents: true}}
			list := SubList{V: 1, Subs: []Sub{row}}
			covered := from == principal
			if (len(missingRoomPrincipals(room, []string{principal}, list, now)) == 0) != covered {
				t.Fatalf("coverage %s via %s: %+v", principal, spelling, list)
			}
			if covered {
				duplicate := row
				duplicate.ID, duplicate.Source = "sub-managed", principalSubSource
				list.Subs = append(list.Subs, duplicate)
			}
			before, _ := json.Marshal(list)
			if changed := repairPrincipalSubs(&list, room, []string{principal}, now); changed == covered {
				t.Fatalf("repair changed=%t covered=%t", changed, covered)
			}
			if covered {
				after, _ := json.Marshal(list)
				if !bytes.Equal(before, after) {
					t.Fatal("repair rewrote existing coverage/duplicates")
				}
			} else if len(list.Subs) != 2 || !coversPrincipal(list.Subs[1], room, principal, now) {
				t.Fatal("wrong-server row suppressed necessary repair")
			}
		}
	}
}

// Check and sync accept a pre-existing full-MXID manual row on the selected
// server. Neither a single row nor existing duplicates need another row/write.
func TestRolloutPrincipalCheckSync(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "existing-duplicate"}[duplicate], func(t *testing.T) {
			e := newEnv(t)
			worker := e.agent("worker")
			principal := "@principal:" + testServer
			room := e.hs.addRoom("fixture-room", worker, principal)
			list := SubList{V: 1, Subs: []Sub{
				{ID: "sub-kinds", SubSelector: SubSelector{Room: room, Kinds: onboardKinds}},
				{ID: "sub-manual", SubSelector: SubSelector{Room: room, From: principal, AllEvents: true}},
			}}
			if duplicate {
				row := list.Subs[1]
				row.ID, row.Source = "sub-managed", principalSubSource
				list.Subs = append(list.Subs, row)
			}
			e.hs.setAccountData(worker, SubsType, list)
			e.as("worker", "host", "principals", "set", principal).ok(t)
			e.as("worker", "subs", "check").ok(t)
			puts := e.hs.CallCount(accountDataPut)
			e.as("worker", "subs", "sync").ok(t)
			e.as("worker", "subs", "check", "--json").ok(t)
			if e.hs.CallCount(accountDataPut) != puts || !reflect.DeepEqual(e.serverSubs("worker"), list) {
				t.Fatal("sync duplicated or rewrote existing principal coverage")
			}
		})
	}
}

// Bind projects only a closed refresh token, even from malformed stored text.
// A foreign-host fixture prevents all live terminal inspection in this test.
func TestRolloutBindRefreshProjection(t *testing.T) {
	e := newEnv(t)
	e.agent("worker")
	for _, token := range []string{"changed_pending_attempt", "changed_cooldown", "changed_reservation", "changed_lease", "current", "PRIVATE SCREEN\nargv=secret"} {
		_, err := config.UpdateState(e.paths(), "worker", func(st *config.State) error {
			st.Terminal = &config.TerminalBinding{Host: "foreign-fixture-host", RefreshStatus: token}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := safeTerminalRefreshStatus(token)
		text := e.as("worker", "bind", "status").ok(t).stdout
		if !strings.Contains(text, "refresh="+want+"\n") || strings.Contains(text, "PRIVATE SCREEN") || strings.Contains(text, "argv=") {
			t.Fatalf("refresh text leaked or omitted token: %s", text)
		}
		raw := e.as("worker", "bind", "status", "--json").ok(t).stdout
		var got struct {
			Refresh string `json:"refresh"`
		}
		if err := json.Unmarshal([]byte(raw), &got); err != nil || got.Refresh != want || strings.Contains(raw, "PRIVATE SCREEN") || strings.Contains(raw, "argv=") {
			t.Fatalf("refresh JSON leaked or omitted token: %s (%v)", raw, err)
		}
	}
}
