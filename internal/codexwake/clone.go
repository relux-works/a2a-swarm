package codexwake

import "time"

// Clone returns a Ledger that shares no mutable memory with l: the attempt
// record and every slice get their own backing storage. Nil and non-nil empty
// slices keep their distinction. Strings and time values are immutable.
func (l Ledger) Clone() Ledger {
	out := l
	if l.Attempt != nil {
		a := *l.Attempt
		a.IDs = cloneStrings(l.Attempt.IDs)
		out.Attempt = &a
	}
	out.Rung = cloneStrings(l.Rung)
	out.Reservations = cloneTimes(l.Reservations)
	out.LastMailIDs = cloneStrings(l.LastMailIDs)
	return out
}

func cloneStrings(v []string) []string {
	if v == nil {
		return nil
	}
	return append(make([]string, 0, len(v)), v...)
}

func cloneTimes(v []time.Time) []time.Time {
	if v == nil {
		return nil
	}
	return append(make([]time.Time, 0, len(v)), v...)
}
