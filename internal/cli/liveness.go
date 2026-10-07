package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const StatusEventType = "ai.relux.a2a.status"
const StatusRoom = "#status"

type livenessConfigError struct{ msg string }

func (e livenessConfigError) Error() string { return e.msg }

func typedStatusArgs(args []string) bool {
	if len(args) < 2 || args[0] != "set" {
		return false
	}
	switch args[1] {
	case "working", "waiting-human", "blocked", "stopping":
		return true
	}
	for _, arg := range args[2:] {
		if strings.HasPrefix(arg, "--") {
			return true
		}
	}
	return false
}

func statusValid(record config.AgentStatus) error {
	switch record.State {
	case "working", "idle", "waiting-human", "blocked", "stopping":
	default:
		return fmt.Errorf("invalid liveness state")
	}
	every, err := time.ParseDuration(record.HeartbeatEvery)
	if err != nil || every <= 0 {
		return fmt.Errorf("heartbeat must be an explicit positive duration")
	}
	switch record.Runtime {
	case "claude", "codex", "none":
	default:
		return fmt.Errorf("runtime must be claude, codex or none")
	}
	switch record.Hooks {
	case "installed", "none":
	default:
		return fmt.Errorf("hooks must be installed or none")
	}
	switch record.Doorbell {
	case "armed", "none":
	default:
		return fmt.Errorf("doorbell must be armed or none")
	}
	if record.Product != "" && !productNameRE.MatchString(record.Product) {
		return fmt.Errorf("invalid product")
	}
	if len(record.Detail) > 500 {
		return fmt.Errorf("status detail too long (max 500 bytes)")
	}
	return nil
}

func cmdStatusInit(ctx context.Context, a *App, args []string) error {
	if len(args) != 0 {
		return usagef("usage: a2a status init")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	alias := StatusRoom + ":" + s.server
	room, err := s.client.ResolveAlias(ctx, alias)
	if errors.Is(err, matrix.ErrUnknownAlias) {
		room, err = s.client.CreateRoom(ctx, matrix.CreateRoomRequest{
			Preset: "public_chat", Visibility: "private", RoomAliasName: "status", Name: "Agent liveness",
			Topic:                     "Self-written liveness. State keys are the sender's own Matrix user ID; no paths, secrets or customer data.",
			PowerLevelContentOverride: map[string]any{"state_default": 50, "events": map[string]int{StatusEventType: 0, "m.room.power_levels": 100, "m.room.tombstone": 100, "m.room.server_acl": 100, "m.room.encryption": 100, "m.room.history_visibility": 100}},
		})
		if matrix.IsCode(err, "M_ROOM_IN_USE") {
			room, err = s.client.ResolveAlias(ctx, alias)
		}
	}
	if err != nil {
		return err
	}
	if _, err = s.client.Join(ctx, room); err != nil {
		return err
	}
	var levels struct {
		Events map[string]int `json:"events"`
	}
	if err = s.client.GetState(ctx, room, "m.room.power_levels", "", &levels); err != nil {
		return err
	}
	level, ok := levels.Events[StatusEventType]
	if !ok || level != 0 {
		return fmt.Errorf("#status policy requires power_levels.events[%q]=0; an existing room administrator must fix it", StatusEventType)
	}
	for _, key := range []string{"m.room.power_levels", "m.room.tombstone", "m.room.server_acl", "m.room.encryption", "m.room.history_visibility"} {
		if levels.Events[key] < 100 {
			return fmt.Errorf("#status policy requires protected event %s at power 100; an existing room administrator must fix it", key)
		}
	}
	a.printf("#status %s (self-key status writes at power 0)\n", room)
	return nil
}

func cmdStatusRecord(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("status set")
	heartbeat := fs.Duration("heartbeat", 0, "explicit declared heartbeat interval")
	runtime := fs.String("runtime", "", "claude, codex or none")
	hooks := fs.String("hooks", "", "installed or none")
	doorbell := fs.String("doorbell", "", "armed or none")
	product := fs.String("product", "", "product scope, if any")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("status set requires state and --heartbeat, --runtime, --hooks, --doorbell")
	}
	record := config.AgentStatus{State: pos[0], Detail: sanitizeLine(strings.Join(pos[1:], " ")), HeartbeatEvery: heartbeat.String(), Runtime: *runtime, Hooks: *hooks, Doorbell: *doorbell, Product: *product}
	if err := statusValid(record); err != nil {
		return usagef("%s", err)
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	if err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error { return s.writeLiveness(ctx, record) }); err != nil {
		return err
	}
	a.printf("%s %s heartbeat=%s hooks=%s\n", s.name, record.State, record.HeartbeatEvery, record.Hooks)
	return nil
}

func (s *session) writeLiveness(ctx context.Context, record config.AgentStatus) error {
	return s.writeLivenessSource(ctx, record, "manual")
}

func (s *session) writeLivenessSource(ctx context.Context, record config.AgentStatus, source string) error {
	if err := statusValid(record); err != nil {
		return err
	}
	room, _, err := s.resolveRoom(ctx, StatusRoom)
	if err != nil {
		return fmt.Errorf("liveness: join #status after a2a status init: %w", err)
	}
	deadline := time.Now().Add(nativeRPCTimeout)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	writeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	_, err = config.UpdateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
		if (source == "wait-heartbeat" || source == "wait-disarm" || (source == "wait" && manualWakeGate(*st) != "")) && st.Liveness != nil {
			doorbell, every := record.Doorbell, record.HeartbeatEvery
			record = *st.Liveness
			record.Doorbell = doorbell
			record.HeartbeatEvery = every
			source = st.LivenessSource
		}
		if source == "runtime" && manualWakeGate(*st) != "" {
			record.State = st.Liveness.State
			record.Detail = st.Liveness.Detail
			source = st.LivenessSource
		}
		now := s.app.Now().UTC()
		record.Name = s.name
		record.Host = machineLabel()
		record.Version = 1
		record.A2AVersion = binaryVersion()
		record.HeartbeatAt = now
		record.Since = now
		record.Detail = s.app.secrets.Scrub(record.Detail)
		if st.Liveness != nil && st.Liveness.State == record.State {
			record.Since = st.Liveness.Since
		}
		if st.Runtime != nil {
			record.Wake = st.Wake.Outcome
		}
		_, err := s.client.PutState(writeCtx, room, StatusEventType, s.creds.UserID, record)
		if matrix.IsCode(err, "M_FORBIDDEN") {
			return fmt.Errorf("liveness: administrator must set power_levels.events[%q]=0; agent power escalation is not a remedy", StatusEventType)
		}
		if err != nil {
			return err
		}
		st.Liveness = &record
		st.LivenessSource = source
		return nil
	})
	return err
}

type whoRow struct {
	Name            string              `json:"name"`
	State           string              `json:"state"`
	Health          string              `json:"health"`
	Age             string              `json:"age"`
	Record          *config.AgentStatus `json:"record,omitempty"`
	Unanswered      string              `json:"unanswered"`
	UnansweredSince time.Time           `json:"unanswered_since,omitempty"`
}

func (a *App) livenessRows(ctx context.Context, s *session, product string, stale time.Duration, watch bool) ([]whoRow, error) {
	room, _, err := s.resolveRoom(ctx, StatusRoom)
	if err != nil {
		return nil, err
	}
	events, err := s.client.GetRoomState(ctx, room)
	if err != nil {
		return nil, err
	}
	records := map[string]config.AgentStatus{}
	members := map[string]bool{}
	for _, ev := range events {
		if ev.StateKey == nil {
			continue
		}
		name, ok := doorbellName(*ev.StateKey, s.server)
		if !ok {
			continue
		}
		if ev.Type == "m.room.member" {
			var member struct {
				Membership string `json:"membership"`
			}
			if json.Unmarshal(ev.Content, &member) == nil && member.Membership == "join" {
				members[name] = true
			}
		}
		if ev.Type != StatusEventType || ev.Sender != *ev.StateKey {
			continue
		}
		var record config.AgentStatus
		if json.Unmarshal(ev.Content, &record) == nil && record.Name == name {
			records[name] = record
			members[name] = true
		}
	}
	var rows []whoRow
	for _, name := range sortedKeys(members) {
		record, exists := records[name]
		if product != "" && (!exists || record.Product != product) {
			continue
		}
		row := whoRow{Name: name, State: "unknown", Health: "down", Age: "-", Unanswered: "unknown (not scanned)"}
		if exists {
			row.Record = &record
			row.State = record.State
			heartbeat, err := time.ParseDuration(record.HeartbeatEvery)
			if err != nil || statusValid(record) != nil || record.Version != 1 || record.HeartbeatAt.IsZero() {
				row.Health = "invalid"
			} else {
				if heartbeat >= stale {
					if !watch {
						return nil, livenessConfigError{fmt.Sprintf("%s heartbeat_every=%s must be less than stale-after=%s", name, heartbeat, stale)}
					}
					row.Health = "invalid"
					row.Age = "heartbeat >= stale-after"
					rows = append(rows, row)
					continue
				}
				age := a.Now().Sub(record.HeartbeatAt)
				row.Age = age.Round(time.Second).String()
				switch {
				case age < 0:
					row.Health = "clock"
				case record.State == "stopping":
					row.Health = "down"
				case age >= stale:
					row.Health = "stale"
				default:
					row.Health = "fresh"
				}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func cmdWho(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("who")
	stale := fs.Duration("stale-after", 0, "explicit staleness threshold")
	product := fs.String("product", "", "filter self-recorded product")
	asJSON := fs.Bool("json", false, "JSON records")
	watch := fs.Bool("watch", false, "monitor transitions until interrupted")
	historyPages := fs.Int("history-pages-per-poll", 0, "explicit per-room watch history page budget (1..100)")
	poll := fs.Duration("poll", 0, "explicit monitoring interval")
	sinceText := fs.String("unanswered-since", "", "explicit RFC3339 start of visible request history")
	unansweredAfter := fs.Duration("unanswered-after", 0, "explicit unanswered alert threshold")
	alert := fs.Bool("alert", false, "send transition-only alerts to configured keepers")
	alertRoom := fs.String("alert-room", "", "existing room for keeper-routed alerts")
	var keepers, aliases stringList
	fs.Var(&keepers, "keeper", "authorized reply/alert keeper (repeatable)")
	fs.Var(&aliases, "reply-alias", "recorded NAME=ALIAS reply identity (repeatable)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *stale <= 0 {
		return usagef("who requires --stale-after with a positive duration")
	}
	if *product != "" && !productNameRE.MatchString(*product) {
		return usagef("invalid product")
	}
	if (*watch && *poll <= 0) || (!*watch && *poll != 0) {
		return usagef("--watch requires an explicit positive --poll")
	}
	if *alert && (*alertRoom == "" || len(keepers) == 0) {
		return usagef("--alert requires --alert-room and at least one --keeper")
	}
	if *unansweredAfter < 0 || (*unansweredAfter > 0 && *sinceText == "") {
		return usagef("--unanswered-after requires positive duration and --unanswered-since")
	}
	var since time.Time
	if *sinceText != "" {
		since, err = time.Parse(time.RFC3339, *sinceText)
		if err != nil || since.After(a.Now()) {
			return usagef("--unanswered-since requires a nonfuture RFC3339 time")
		}
	}
	if (*watch && !since.IsZero() && (*historyPages < 1 || *historyPages > 100)) || ((!*watch || since.IsZero()) && *historyPages != 0) {
		return usagef("watch with --unanswered-since requires --history-pages-per-poll in 1..100")
	}
	policy, err := newReplyPolicy(keepers, aliases)
	if err != nil {
		return usagef("%s", err)
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	history := &livenessHistory{}
	pollWait := func() bool {
		timer := time.NewTimer(*poll)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		}
	}
	for {
		rows, err := a.livenessRows(ctx, s, *product, *stale, *watch)
		if err != nil {
			if !*watch {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			a.fail(fmt.Errorf("who watch: %s; retry after %s", err, *poll))
			if !pollWait() {
				return nil
			}
			continue
		}
		if !since.IsZero() {
			if *watch {
				err = history.step(ctx, a, s, rows, since, policy, *historyPages)
			} else {
				err = a.addUnanswered(ctx, s, rows, since, policy)
			}
			if err != nil {
				if !*watch {
					return err
				}
				if ctx.Err() != nil {
					return nil
				}
				for i := range rows {
					rows[i].Unanswered = "unknown (history unavailable)"
					rows[i].UnansweredSince = time.Time{}
				}
				a.fail(fmt.Errorf("who watch: %s; unanswered unknown, retry after %s", err, *poll))
			}
		}
		if *alert {
			if err := a.alertLiveness(ctx, s, rows, *alertRoom, keepers, *unansweredAfter, *product, since, *stale); err != nil {
				if !*watch {
					return err
				}
				if ctx.Err() != nil {
					return nil
				}
				a.fail(fmt.Errorf("who watch: %s; retry pending alert after %s", err, *poll))
			}
		}
		if *asJSON {
			if err := a.printJSON(rows); err != nil {
				return err
			}
		} else {
			for _, row := range rows {
				host, detail, hooks, wake := "-", "-", "none", "unavailable: unbound"
				if row.Record != nil {
					host = row.Record.Host
					detail = row.Record.Detail
					hooks = row.Record.Hooks
					if row.Record.Wake != "" {
						wake = row.Record.Wake
					}
				}
				a.printf("%s\t%s\t%s\tage=%s\tunanswered=%s\thost=%s\thooks=%s\twake=%s\t%s\n", row.Name, sanitizeLine(row.State), row.Health, row.Age, row.Unanswered, sanitizeLine(host), sanitizeLine(hooks), sanitizeLine(wake), a.secrets.Scrub(sanitizeLine(detail)))
			}
		}
		if !*watch {
			return nil
		}
		if !pollWait() {
			return nil
		}
	}
}

func binaryVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	version := info.Main.Version
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			version = setting.Value
		}
		if setting.Key == "vcs.modified" && setting.Value == "true" {
			version += "+modified"
		}
	}
	if version == "" {
		return "unknown"
	}
	return version
}
