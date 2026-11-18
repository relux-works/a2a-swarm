//go:build phase1measure && phase1cache && darwin && arm64

package config

func phase1MeasurementPaths(base Paths, identity string) (Paths, func(), func()) {
	scope := NewStateDecodeScope(identity)
	p := base.WithReadCache(NewReadCache(identity)).WithStateDecodeScope(scope)
	return p, scope.Open, scope.Close
}

func phase1MeasurementStats() phase1MeasureStats {
	s := ReadStateDecodeStats()
	return phase1MeasureStats{
		available:      true,
		loadStateCalls: s.LoadStateCalls, stateReadAttempts: s.StateReadAttempts, stateReadErrors: s.StateReadErrors, stateReadBytes: s.StateReadBytes,
		unmarshalStatePasses: s.UnmarshalStatePasses, unmarshalStateBytes: s.UnmarshalStateBytes,
		scopeHits: s.ScopeHits, scopeMisses: s.ScopeMisses, scopeFallbacks: s.ScopeFallbacks, writerClones: s.WriterClones,
		stateDigests: s.StateDigests, canonicalStates: s.CanonicalStates, sealedStates: s.SealedStates, writerSealChecks: s.WriterSealChecks,
		stateEncodes: s.StateEncodes, mailboxDecodes: s.MailboxDecodes, stateWriteBytes: s.StateWriteBytes, mailboxWriteBytes: s.MailboxWriteBytes,
	}
}
