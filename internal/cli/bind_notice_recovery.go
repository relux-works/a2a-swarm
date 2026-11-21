package cli

import (
	"errors"
	"reflect"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// A consumer replacement continues an episode, not an input capability. Manual
// rebinding, a different session/target and an explicit unbind start afresh.
func carryTerminalNoticeEpisode(old, next *config.TerminalBinding, automatic bool) {
	if old == nil || !automatic || old.Outcome == "unbound" || old.SessionID != next.SessionID || !ownTerminalTarget(old, next.Target) {
		return
	}
	next.OwnNotice = old.OwnNotice
	next.OwnNotice.PossiblyOrphaned = nil
	if noticeValid(old.OwnNotice) == nil && old.OwnNotice.Since != "" && old.NoticeReport != nil && old.NoticeReport.Since == old.OwnNotice.Since {
		next.NoticeReport = old.NoticeReport
	}
}

// The report writer runs outside the control lock. Carry its latest committed
// pin and ACK under the same state lock that installs the replacement, never
// from bind's earlier journal snapshot. Identity/connection scope is s.paths/name.
func persistTerminalReplacement(s *session, next *config.TerminalBinding, automatic bool) error {
	state, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if config.BindPartMarked(state) && !config.BindPartActive(state) {
		return config.ErrBindPartOperation
	}
	if !config.BindPartMarked(state) {
		deadline := time.Now().Add(nativeRPCTimeout)
		holder := s.app.leaseHolderFor(s.paths, next.SessionID, deadline, s.longConsumer)
		state, err = config.EnsureBindPart(s.paths, s.name, config.BindPartOpenOptions{Holder: holder, Now: s.app.Now(), Dead: s.app.leaseDeadBy(s.paths, deadline)})
		if err != nil {
			return err
		}
	}
	view, err := config.ReadBindPartView(s.paths, s.name, state)
	if err != nil {
		return err
	}
	prepared := *next
	carryTerminalNoticeEpisode(state.Terminal, &prepared, automatic)
	if !reflect.DeepEqual(prepared.OwnNotice, view.OwnNotice) || prepared.NoticePublication != view.NoticePublication {
		if prepared.NoticePublication == "published" && !reflect.DeepEqual(prepared.OwnNotice, view.OwnNotice) {
			prepared.NoticePublication = "unavailable: own notice changed"
		}
	}
	proof := config.BindControlProof{TargetVerified: true, OwnerPID: prepared.OwnerPID, OwnerBirth: prepared.OwnerBirth, Target: prepared.Target}
	if state.Terminal != nil {
		proof.OwnerPID = state.Terminal.OwnerPID
		proof.OwnerBirth = state.Terminal.OwnerBirth
		proof.Target = state.Terminal.Target
	}
	view, err = config.SetBindControl(s.paths, s.name,
		config.BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID},
		config.BindControlTarget{Generation: next.Generation, SessionID: next.SessionID, Stopped: next.Stopped, Kind: "create"}, proof)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(prepared.OwnNotice, view.OwnNotice) || prepared.NoticePublication != view.NoticePublication {
		view, err = config.RecordBindNotice(s.paths, s.name, view.Revision, next.Generation, prepared.OwnNotice, prepared.NoticePublication)
		if err != nil {
			return err
		}
	}
	_, err = s.app.updateState(s.paths, s.name, func(st *config.State) error {
		carryTerminalNoticeEpisode(st.Terminal, next, automatic)
		st.Terminal = next
		return nil
	})
	return err
}

// Consumer startup records only its metadata; a report committed since the
// replacement was installed remains authoritative under this state lock too.
func persistTerminalConsumer(s *session, started *config.TerminalBinding) error {
	_, err := s.app.updateState(s.paths, s.name, func(st *config.State) error {
		if st.Terminal == nil || st.Terminal.Generation != started.Generation {
			return errors.New("binding changed before consumer startup")
		}
		st.Terminal.DaemonPID = started.DaemonPID
		st.Terminal.DaemonBirth = started.DaemonBirth
		return nil
	})
	return err
}
