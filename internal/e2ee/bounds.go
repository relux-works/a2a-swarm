package e2ee

// validateBounds rejects persisted violations; loading never silently prunes or
// repairs replay floors, tombstones or protected evidence from a buggy writer.
func (s Snapshot) validateBounds() error {
	if len(s.Engine.Olm) > 1024 || len(s.Engine.Inbound) > 128 || len(s.Engine.Outbound) > 32 || len(s.Replay) > 128 {
		return ErrCapacity
	}
	for session, window := range s.Replay {
		if !text(session, 255) || (!window.HasFloor && window.Floor != 0) {
			return ErrCodec
		}
		if len(window.Entries) > ReplayLimit {
			return ErrCapacity
		}
		for index, entry := range window.Entries {
			if (window.HasFloor && index <= window.Floor) || !text(entry.Event, 255) || !hexSize(entry.Digest, 32) {
				return ErrCodec
			}
		}
	}
	r := s.Retention
	if len(r.Rooms) > MaxRooms || len(r.Verdicts) > MaxVerdicts || len(r.Journal) > MaxJournal {
		return ErrCapacity
	}
	if r.OldestSequence > r.Sequence {
		return ErrCodec
	}
	for hash, room := range r.Rooms {
		if !hexSize(hash, 32) || room == nil {
			return ErrCodec
		}
		if len(room.Retired) > MaxRetired {
			return ErrCapacity
		}
		for retired, present := range room.Retired {
			if !hexSize(retired, 32) || !present {
				return ErrCodec
			}
		}
	}
	for _, group := range []struct {
		records  []Verdict
		maxBytes int
	}{
		{r.Verdicts, MaxSnapshot - RecoveryReserve}, {r.Journal, MaxJournalBytes},
	} {
		total := int64(0)
		for _, v := range group.records {
			if !text(v.ID, 255) || v.Bytes < 0 || v.Bytes > MaxJournalBytes || v.TimeMS < 0 || v.Sequence > r.Sequence {
				return ErrCodec
			}
			total += int64(v.Bytes)
		}
		if total > int64(group.maxBytes) {
			return ErrCapacity
		}
	}
	return nil
}
