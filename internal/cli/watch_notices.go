package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

const noticeCoalesce = 30 * time.Second

// noticeWindow is a fixed coalescing window, never a sliding debounce: a
// continuous burst cannot postpone mail forever. Priority closes it early.
type noticeWindow struct{ due time.Time }

func (w *noticeWindow) ready(now time.Time, pending, priority bool) bool {
	if !pending {
		w.due = time.Time{}
		return false
	}
	if w.due.IsZero() {
		w.due = now.Add(noticeCoalesce)
	}
	return priority || !now.Before(w.due)
}

func (a *App) watchNotices(ctx context.Context, rooms []string, all, asJSON, listener bool, predicates deliveryPredicates) error {
	// CM's notice owner lives in Daemon. No ambient local session can arm it.
	if !a.daemonEngine {
		return daemon.NotAllowed
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if st.MonitorOwner == nil || st.Lease == nil {
		return config.ErrMonitorOwner
	}
	sessionID := a.Getenv(EnvSessionID)
	if !daemon.ValidSession(sessionID) {
		return config.ErrMonitorOwner
	}
	birthCtx, birthCancel := context.WithTimeout(ctx, time.Second)
	consumerBirth, birthErr := tmuxwake.Birth(birthCtx, os.Getpid())
	ownerBirth, ownerErr := tmuxwake.Birth(birthCtx, st.Lease.PID)
	birthCancel()
	if birthErr != nil || ownerErr != nil {
		return config.ErrMonitorOwner
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return daemon.Unavailable
	}
	// The lease supplies only a candidate; L03 validates the owner's hash,
	// runtime, freshness, host and exact existing lease again under its locks.
	record := config.Monitor{Version: 1, Kind: "monitor", Runtime: "muse", Generation: hex.EncodeToString(id[:]), SessionID: sessionID, Host: machineLabel(), OwnerPID: st.Lease.PID, OwnerBirth: ownerBirth, ConsumerPID: os.Getpid(), ConsumerBirth: consumerBirth, Transport: "stdout", Phase: "streaming", HeartbeatEvery: daemon.KeepaliveInterval, HeartbeatAt: time.Now().UTC()}
	err = config.WithMonitorGeneration(s.paths, s.name, time.Now().Add(time.Second), config.MonitorAutomatic, record, func(g *config.MonitorGeneration) error {
		if err := g.RenewOwnerLease(record.Generation); err != nil {
			return err
		}
		filter, err := s.resolveFilter(ctx, rooms, all)
		if err != nil {
			return err
		}
		filter.predicates = predicates.forSession(s)
		return a.noticeLoop(ctx, s, g, record, filter, asJSON, listener)
	})
	if errors.Is(err, config.ErrMonitorOwner) || errors.Is(err, config.ErrMonitorLease) {
		return config.ErrMonitorConflict
	}
	return err
}

func (a *App) noticeLoop(ctx context.Context, s *session, g *config.MonitorGeneration, record config.Monitor, filter deliveryFilter, asJSON, listener bool) (result error) {
	defer func() {
		if ctx.Err() != nil {
			result = nil
		}
	}()
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	var window noticeWindow
	for ctx.Err() == nil {
		if err := g.RenewOwnerLease(record.Generation); err != nil {
			return err
		}
		s.principals = s.readDeliveryPrincipals()
		filter.predicates.principals = s.principals
		var items []waitItem
		st, err = s.lockedDeliver(ctx, time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
			for _, entry := range mb.Pending() {
				if entry.Shown || !filter.shows(entry) {
					continue
				}
				items = append(items, itemOf(entry, s.server))
			}
			return nil, nil
		})
		if err != nil {
			return err
		}
		priority := noticeOwnerSender(items, ownerFiles(s.paths, s.name).pin().Owner)
		if !st.HooksPaused && window.ready(time.Now(), len(items) != 0, priority) {
			payload, err := s.noticeSummary(ctx, items).encode(asJSON)
			if err != nil {
				return err
			}
			if len(payload) != 0 {
				sink, err := admitMonitorSink(g, record.Generation, func() error {
					data := append(payload, '\n')
					n, err := a.Stdout.Write(data)
					if err == nil && n != len(data) {
						err = io.ErrShortWrite
					}
					return err
				})
				if err != nil {
					return err
				}
				if err := sink.announce(); err != nil {
					return err
				}
				if listener {
					if err := g.RecordNotice(record.Generation, a.Now()); err != nil {
						return err
					}
				}
				// Flow ACKs are not the C2+C3 receipt barrier. Watch never
				// calls Delivered/afterPrint, AddReceived or inbox rendering.
				_, err = s.lockedDeliver(ctx, time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) { return nil, mb.MarkShown(itemIDs(items)) })
				if err != nil {
					return err
				}
			}
			window.due = time.Time{}
		}
		poll := daemon.KeepaliveInterval
		if !window.due.IsZero() && !st.HooksPaused {
			poll = min(poll, max(time.Millisecond, time.Until(window.due)))
		}
		if err := s.pauseAgentReplay(ctx, time.Time{}); err != nil {
			return err
		}
		pollCtx, cancel := context.WithTimeout(ctx, poll)
		resp, err := s.waitSync(pollCtx, st.NextBatch, poll, deliverySyncFilter(st.NextBatch))
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		if err != nil {
			return err
		}
		if st.NextBatch == "" {
			st, err = s.savePositionAll(ctx, resp, time.Time{}, filter.allRooms())
		} else {
			subs, readErr := s.getSubs(ctx)
			if readErr != nil {
				return readErr
			}
			st, err = s.ingest(ctx, resp, st.NextBatch, subs.Subs, filter.allRooms(), time.Time{})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Only the server-authenticated sender matching the locally pinned owner can
// close the window early. Header from/kind, body text and urgency confer none.
func noticeOwnerSender(items []waitItem, owner string) bool {
	if !ownerMXID.MatchString(owner) {
		return false
	}
	for _, item := range items {
		if item.ev != nil && item.ev.Type == "m.room.message" && item.ev.Sender == owner {
			return true
		}
	}
	return false
}
