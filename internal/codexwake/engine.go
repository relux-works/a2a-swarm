package codexwake

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

var token = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var ErrRate = errors.New("wake suppressed: rate")

// Binding is registered by the owning launcher, never derived from a peer
// event or saved-thread discovery. Generation fences every submission.
type Binding struct {
	Identity       string        `json:"identity"`
	ThreadID       string        `json:"thread_id"`
	SessionID      string        `json:"session_id"`
	Endpoint       string        `json:"endpoint"`
	EndpointID     string        `json:"endpoint_id"`
	ServerVersion  string        `json:"server_version"`
	Generation     string        `json:"generation"`
	Host           string        `json:"host"`
	Cwd            string        `json:"cwd"`
	OwnerPID       int           `json:"owner_pid"`
	OwnerStart     string        `json:"owner_start"`
	Gate           string        `json:"gate"`
	MaxPerHour     int           `json:"max_wakes_per_hour"`
	HeartbeatEvery time.Duration `json:"heartbeat_every"`
}

func (b Binding) Validate() error {
	if b.Identity == "" || !token.MatchString(b.ThreadID) || b.SessionID != b.ThreadID || !token.MatchString(b.Generation) || b.Host == "" || !filepath.IsAbs(b.Cwd) || b.OwnerPID <= 0 || b.OwnerStart == "" || b.EndpointID == "" || b.ServerVersion == "" || b.MaxPerHour <= 0 || b.HeartbeatEvery <= 0 {
		return errors.New("runtime binding requires own identity/thread/session, generation, host/cwd, launcher process birth, explicit wake limit and heartbeat")
	}
	if _, err := EndpointPath(b.Endpoint); err != nil {
		return err
	}
	switch b.Gate {
	case "ready", "paused", "budget", "stopping", "unknown":
	default:
		return errors.New("runtime gate must be ready, paused, budget, stopping or unknown")
	}
	return nil
}

// Admission reports separate host, lifecycle and runtime reachability facts.
// It does not turn a healthy Matrix waiter into proof of a healthy runtime.
func Admission(b Binding, host string, ownerAlive, hooksPaused bool, remote Snapshot) string {
	if b.Validate() != nil {
		return "unavailable: binding"
	}
	if b.Host != host {
		return "unavailable: host"
	}
	if !ownerAlive {
		return "unavailable: launcher"
	}
	if hooksPaused {
		return "wake suppressed: paused"
	}
	if b.Gate != "ready" {
		return "wake suppressed: " + b.Gate
	}
	if remote.ID != b.ThreadID {
		return "unavailable: thread"
	}
	if filepath.Clean(remote.Cwd) != filepath.Clean(b.Cwd) {
		return "unavailable: cwd"
	}
	switch remote.Status.Type {
	case "idle":
		if len(remote.Status.ActiveFlags) > 0 {
			return "unavailable: lifecycle"
		}
		return "ready"
	case "active":
		for _, flag := range remote.Status.ActiveFlags {
			if flag == "waitingOnApproval" || flag == "waitingOnUserInput" {
				return "wake suppressed: human"
			}
		}
		return "wake suppressed: busy"
	case "notLoaded":
		return "unavailable: notLoaded"
	default:
		return "unavailable: lifecycle"
	}
}

type Attempt struct {
	ID         string    `json:"id"`
	Generation string    `json:"generation"`
	Output     string    `json:"output"`
	IDs        []string  `json:"mail_ids"`
	Started    time.Time `json:"started"`
}

// Ledger is persisted before sending an RPC. An inconclusive attempt remains
// fenced; absence from a history snapshot is not proof a request never ran.
// Delivery records are not mailbox receipts.
type Ledger struct {
	Attempt        *Attempt    `json:"attempt,omitempty"`
	Rung           []string    `json:"rung,omitempty"`
	Reservations   []time.Time `json:"reservations,omitempty"`
	LastTurn       string      `json:"last_turn,omitempty"`
	LastTurnStatus string      `json:"last_turn_status,omitempty"`
	LastMailIDs    []string    `json:"last_mail_ids,omitempty"`
	Outcome        string      `json:"outcome,omitempty"`
}

func (l *Ledger) NewIDs(pending []string) []string {
	var out []string
	for _, id := range pending {
		if id != "" && !slices.Contains(l.Rung, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func (l *Ledger) PruneRung(pending []string) {
	l.Rung = slices.DeleteFunc(l.Rung, func(id string) bool { return !slices.Contains(pending, id) })
}

func (l *Ledger) Begin(b Binding, pending []string, notice string, now time.Time) (*Attempt, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if l.Attempt != nil {
		return nil, errors.New("wake uncertain: reconcile existing attempt before another submission")
	}
	ids := l.NewIDs(pending)
	if len(ids) == 0 {
		return nil, nil
	}
	l.Reservations = slices.DeleteFunc(l.Reservations, func(at time.Time) bool { return !at.After(now.Add(-time.Hour)) })
	if len(l.Reservations) >= b.MaxPerHour {
		l.Outcome = ErrRate.Error()
		return nil, ErrRate
	}
	if notice == "" {
		return nil, errors.New("empty host notice")
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(bytes[:])
	attempt := &Attempt{ID: id, Generation: b.Generation, Output: fmt.Sprintf("%s\n[a2a-wake generation=%s id=%s]", notice, b.Generation, id), IDs: slices.Clone(ids), Started: now}
	l.Attempt = attempt
	l.Reservations = append(l.Reservations, now)
	l.Outcome = "wake uncertain: submission reserved"
	return attempt, nil
}

func (l *Ledger) Accept(b Binding, turn string) error {
	if l.Attempt == nil || l.Attempt.Generation != b.Generation || turn == "" {
		return errors.New("wake attempt generation/turn mismatch")
	}
	for _, id := range l.Attempt.IDs {
		if !slices.Contains(l.Rung, id) {
			l.Rung = append(l.Rung, id)
		}
	}
	l.LastMailIDs = slices.Clone(l.Attempt.IDs)
	l.Attempt = nil
	l.LastTurn = turn
	l.LastTurnStatus = "accepted"
	l.Outcome = "wake accepted"
	return nil
}
