package devicelist

import "time"

// Tip names one block of the active chain: its hash in display byte order,
// its height and its header time in POSIX seconds. It is a struct so that the
// gateway contract can add a member without a change of any signature here.
type Tip struct {
	Hash   string
	Height uint64
	Time   uint64
}

// NameOp locates the last confirmed name operation of the owner's name: its
// txid, the height of its block and the hash of that block (LIP-0010 SH3).
// The approximation of SH5 in Admit reads it.
type NameOp struct {
	TxID      string
	Height    uint64
	BlockHash string
}

// NameView is what step 4 of section 11 reads from the owner's name: the
// network of the chain, the owner's id/ name, the device-list pin of the name
// value if the value holds a valid one, the location of the name operation
// that carries the value, and the tip the read was made at. An expired name
// never gives a pin: HasPin is false whenever Expired is true.
type NameView struct {
	Network string
	Owner   string
	HasPin  bool
	Pin     Pin
	Expired bool
	Op      NameOp
	Tip     Tip
}

// SealState is the state of a seal outpoint in a chain view, mempool
// included. The set is closed.
type SealState string

const (
	// SealUnspent: confirmed (at least one confirmation at Tip) and not
	// spent by a mempool transaction.
	SealUnspent SealState = "unspent"
	// SealUnconfirmed: present only in the mempool, with zero confirmations.
	SealUnconfirmed SealState = "unconfirmed"
	// SealSpendPending: confirmed, but spent by a mempool transaction.
	SealSpendPending SealState = "spend_pending"
	// SealNotInUTXOSet: absent from both the confirmed set and the
	// mempool-inclusive view. This is not proof of a spending transaction.
	SealNotInUTXOSet SealState = "not_in_utxo_set"
)

// Freshness is the label of an observation. The consumer computes it; no
// snapshot file asserts it about itself, and no label authorizes enforcement.
type Freshness string

const (
	LabelFresh   Freshness = "fresh"   // Young, within the tip lag, and its tip is the independently known live tip.
	LabelStale   Freshness = "stale"   // Too old, tip lag exceeded, or its tip differs from the live tip.
	LabelUnknown Freshness = "unknown" // No trustworthy clock, no independent live tip, or no statement about the seal.
	LabelInvalid Freshness = "invalid" // No usable view: written in the future, or schema, filesystem, network or watch-set failure.
)

// Check is what the consumer knows at the time of one check. Admit takes it
// per call, so the age of a view is judged at every use and not only when the
// view was made. The zero Check has no trusted clock and no live tip, and
// yields the label unknown.
type Check struct {
	// Now is the consumer's time of the check; it counts only when
	// ClockTrusted is true.
	Now          time.Time
	ClockTrusted bool
	// LiveTip is the tip the consumer learned independently of the view at
	// check time (its own getblockcount over a live gateway). It is nil on a
	// host that has only the snapshot file; no label is fresh then.
	LiveTip *Tip
}

// ChainView is the per-command view of the chain that steps 4 and 8 of
// section 11 need. An implementation holds no credential that an agent could
// read; how a live view gets its data is not decided here.
//
// Live reports whether the view sees the node's current state, mempool
// included, at the time of the call. A view that is not live cannot see a
// seal spend after it was made, so a verdict on it is for observation only.
// Snapshot and UnavailableView are not live. Freshness is a trusted adapter
// boundary: a foreign implementation can assert any label. Admit validates
// its closed set, but cannot establish a foreign adapter's honesty.
type ChainView interface {
	// Name reads the owner's name. An error means the view is unavailable
	// and no verdict can be made.
	Name() (NameView, error)
	// Seal reports the state of the outpoint in the view (gettxout with the
	// mempool). ErrSealUnknown means the view holds no statement about the
	// outpoint; it is not a statement that it is absent.
	Seal(seal Outpoint) (SealState, error)
	// Freshness computes the label of the view for one check.
	Freshness(at Check) Freshness
	Live() bool
}

// worseLabel combines labels without improving an existing worse label.
// The order is invalid > stale > unknown > fresh; an unrecognized value is invalid.
func worseLabel(a, b Freshness) Freshness {
	rank := func(label Freshness) int {
		switch label {
		case LabelFresh:
			return 0
		case LabelUnknown:
			return 1
		case LabelStale:
			return 2
		default:
			return 3
		}
	}
	if rank(a) == 3 || rank(b) == 3 {
		return LabelInvalid
	}
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// UnavailableView is an exported no-usable-view ChainView. Use Absent for a
// missing snapshot (unknown); its zero value means present but refused (invalid).
// It contains no path, upstream error or usable name or seal data.
// Admit returns CHAIN_VIEW_UNAVAILABLE at step 4 for an otherwise valid event.
type UnavailableView struct{ Absent bool }

func (UnavailableView) Name() (NameView, error)          { return NameView{}, ErrChainView }
func (UnavailableView) Seal(Outpoint) (SealState, error) { return "", ErrChainView }
func (v UnavailableView) Freshness(Check) Freshness {
	if v.Absent {
		return LabelUnknown
	}
	return LabelInvalid
}
func (UnavailableView) Live() bool { return false }
