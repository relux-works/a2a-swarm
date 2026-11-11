package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const agentDMContextLimit = 64

type agentDMContexts struct {
	Contract   string   `json:"contract"`
	Caller     string   `json:"caller_mxid"`
	Connection string   `json:"connection_id"`
	Rooms      []string `json:"rooms"`
}

// Presence is independent of directory privacy for delivery only. Validate the
// selected root and its feature namespace; prefixes above that admitted root
// retain the resolver's existing semantics. Existing files still pass the
// unchanged strict command reader, including all of its ancestor checks.
func agentDeliveryRead(root, dir, name string) ([]byte, error) {
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("agent-dm: delivery path outside selected root")
	}
	for cur := dir; ; cur = filepath.Dir(cur) {
		st, err := os.Lstat(cur)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil && !st.IsDir() {
			return nil, errors.New("agent-dm: non-physical delivery path")
		}
		if cur == root {
			break
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
		return nil, err
	}
	return agentRead(dir, name)
}

func (s *session) agentDeliveryRead(dir, name string) ([]byte, error) {
	return agentDeliveryRead(s.paths.Base, dir, name)
}

func parseAgentContexts(raw []byte, caller, connection string) (agentDMContexts, error) {
	out := agentDMContexts{Contract: agentDMVersion, Caller: caller, Connection: connection, Rooms: []string{}}
	if len(raw) == 0 {
		return out, nil
	}
	var parsed agentDMContexts
	if agentDecode(raw, &parsed) != nil || parsed.Contract != agentDMVersion || parsed.Caller != caller || parsed.Connection != connection || len(parsed.Rooms) > agentDMContextLimit {
		return out, errors.New("agent-dm: unknown retained room context")
	}
	seen := map[string]bool{}
	for _, room := range parsed.Rooms {
		if !matrix.ValidVerificationID(room, '!') || seen[room] {
			return out, errors.New("agent-dm: invalid retained room context")
		}
		seen[room] = true
	}
	return parsed, nil
}

// Persist conservative classification before dependent mapping/JOIN effects.
// Use the established state writer, preserving its seal, migration fence and
// unrelated opaque fields. Context never activates a pair or grants admission.
func (s *session) retainAgentContexts(ctx context.Context, rooms []string) error {
	if len(rooms) == 0 {
		return nil
	}
	connection, err := s.agentConnection()
	if err != nil {
		return err
	}
	_, err = config.UpdateStateContext(ctx, s.paths, s.name, time.Time{}, func(st *config.State) error {
		known, err := parseAgentContexts(st.AgentDMContexts, s.creds.UserID, connection)
		if err != nil {
			return err
		}
		for _, room := range rooms {
			if !matrix.ValidVerificationID(room, '!') {
				return errors.New("agent-dm: invalid room context")
			}
			if !slices.Contains(known.Rooms, room) {
				known.Rooms = append(known.Rooms, room)
			}
		}
		if len(known.Rooms) > agentDMContextLimit {
			return errors.New("agent-dm: retained room context capacity reached")
		}
		slices.Sort(known.Rooms)
		raw, err := json.Marshal(known)
		if err != nil {
			return err
		}
		st.AgentDMContexts = raw
		return nil
	})
	return err
}

func (s *session) checkAgentContextCapacity() error {
	state, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	connection, err := s.agentConnection()
	if err != nil {
		return err
	}
	known, err := parseAgentContexts(state.AgentDMContexts, s.creds.UserID, connection)
	if err != nil {
		return err
	}
	if len(known.Rooms) >= agentDMContextLimit {
		return errors.New("agent-dm: retained room context capacity reached")
	}
	return nil
}

func (s *session) agentLocalGate() (agentDMDelivery, agentDMRecords, error) {
	records, _, recordErr := s.agentRecordsWithReader(s.agentDeliveryRead)
	out := agentDMDelivery{Rooms: map[string]agentDMGate{}, Unknown: recordErr != nil, Stamp: s.agentGateStamp()}
	state, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return out, records, err
	}
	if len(state.AgentDMContexts) > 0 {
		connection, err := s.agentConnection()
		if err != nil {
			return out, records, err
		}
		contexts, err := parseAgentContexts(state.AgentDMContexts, s.creds.UserID, connection)
		out.Unknown = out.Unknown || err != nil
		for _, room := range contexts.Rooms {
			out.Rooms[room] = agentDMGate{}
		}
	}
	if recordErr != nil {
		// Partially decoded records carry no identity or mapping authority.
		records.Pairs = map[string]agentDMRecord{}
	} else {
		for peer, pair := range records.Pairs {
			if pair.Room != nil {
				out.Rooms[*pair.Room] = agentDMGate{Peer: peer}
			}
		}
	}
	for _, gate := range out.Rooms {
		if gate.Peer == "" {
			out.Unknown = true
		}
	}
	if out.Unknown {
		s.warnAgentDelivery()
	}
	return out, records, nil
}

func (s *session) warnAgentDelivery() {
	if s.app != nil && s.app.Stderr != nil && !s.agentDeliveryWarned {
		fmt.Fprintln(s.app.Stderr, "agent-dm: delivery context/storage unknown; known pair mail retained, ordinary and principal delivery continue")
		s.agentDeliveryWarned = true
	}
}

// Show durable ordinary/principal mail before this pause at each consumer's
// next poll. A retained cursor must not create an immediate replay busy loop.
func (s *session) pauseAgentReplay(ctx context.Context, deadline time.Time) error {
	if !s.agentReplayDeferred {
		return nil
	}
	pause := 5 * time.Second
	if !deadline.IsZero() {
		remaining := deadline.Sub(s.app.Now())
		if remaining <= 0 {
			return errWaitTimeout
		}
		pause = min(pause, remaining)
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		s.agentReplayDeferred = false
		if !deadline.IsZero() && !s.app.Now().Before(deadline) {
			return errWaitTimeout
		}
		return nil
	}
}
