package cli

import "github.com/relux-works/a2a-swarm/internal/config"

// A consumer replacement continues an episode, not an input capability. Manual
// rebinding, a different session/target and an explicit unbind start afresh.
func carryTerminalNoticeEpisode(old, next *config.TerminalBinding, automatic bool) {
	if !automatic || old.Outcome == "unbound" || old.SessionID != next.SessionID || !ownTerminalTarget(old, next.Target) {
		return
	}
	next.OwnNotice = old.OwnNotice
	next.OwnNotice.PossiblyOrphaned = nil
	if noticeValid(old.OwnNotice) == nil && old.OwnNotice.Since != "" && old.NoticeReport != nil && old.NoticeReport.Since == old.OwnNotice.Since {
		next.NoticeReport = old.NoticeReport
	}
}
