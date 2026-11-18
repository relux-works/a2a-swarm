// Package wakediag contains bounded historical observations, never wake authority.
package wakediag

import (
	"encoding/json"
	"time"
)

type Kind string

const (
	TerminalLayoutRefused    Kind = "terminal_pre_send_layout_refused"
	NativeDefiniteRefusal    Kind = "native_definite_refusal"
	NativeTransportUncertain Kind = "native_transport_uncertain"
)

// Record retains one observed stage/result without errors, input or mail metadata.
type Record struct {
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`
}

func (r Record) Valid() bool {
	return !r.At.IsZero() && (r.Kind == TerminalLayoutRefused || r.Kind == NativeDefiniteRefusal || r.Kind == NativeTransportUncertain)
}

// UnmarshalJSON is forgiving for optional history. Unknown data cannot prevent
// loading delivery state or become arbitrary status text.
func (r *Record) UnmarshalJSON(data []byte) error {
	type plain Record
	var next plain
	*r = Record{}
	if json.Unmarshal(data, &next) == nil && Record(next).Valid() {
		*r = Record(next)
		r.At = r.At.UTC()
	}
	return nil
}

// Retain coalesces identical observations without refreshing their first time.
func Retain(old *Record, kind Kind, at time.Time) *Record {
	if old != nil && old.Valid() && old.Kind == kind {
		return old
	}
	next := &Record{Kind: kind, At: at.UTC()}
	if !next.Valid() {
		return old
	}
	return next
}

// Project copies only a valid record of one of the caller's allowed kinds.
func Project(r *Record, allowed ...Kind) *Record {
	if r != nil && r.Valid() {
		for _, kind := range allowed {
			if r.Kind == kind {
				copy := *r
				copy.At = copy.At.UTC()
				return &copy
			}
		}
	}
	return nil
}
