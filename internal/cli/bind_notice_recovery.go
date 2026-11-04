package cli

import (
	"errors"

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
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		carryTerminalNoticeEpisode(st.Terminal, next, automatic)
		st.Terminal = next
		return nil
	})
	return err
}

// Consumer startup records only its metadata; a report committed since the
// replacement was installed remains authoritative under this state lock too.
func persistTerminalConsumer(s *session, started *config.TerminalBinding) error {
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		if st.Terminal == nil || st.Terminal.Generation != started.Generation {
			return errors.New("binding changed before consumer startup")
		}
		st.Terminal.DaemonPID = started.DaemonPID
		st.Terminal.DaemonBirth = started.DaemonBirth
		return nil
	})
	return err
}
