package devicelist

import "sync"

// Journal bounds never evict or grow at run time. Per-owner replay capacity
// reserves progress for other owners until the separate total bound is reached.
const (
	MaxReplayKeys      = 1 << 16
	MaxOwnerReplayKeys = 1 << 12
	MaxClosures        = 1 << 12
	MaxSeenUnspent     = 1 << 12
	MaxJournalOwners   = 16
)

type replayKey struct {
	session string
	index   uint64
}
type journalOwner struct {
	network string
	owner   string
}

// Head is the observed list pin, its earliest name operation and the tip at
// which the reader most recently recorded an ordered observation of that head
// (LIP-0010 SH5 approximation).
type Head struct {
	Seq        uint64
	SHA256     string
	Op         NameOp
	ObservedAt Tip
}

// observeClosure is an observe-grade closure, not evidence of a spend: no
// spending transaction was validated. Absence from the confirmed set can also
// come from a reorganisation that removed the funding of the seal. This record
// is private and has no export, enumeration or serialization function.
type observeClosure struct{}

type ownerJournal struct {
	hasHead     bool
	head        Head
	closedKeys  map[string]observeClosure
	closedSeals map[Outpoint]observeClosure
	seenUnspent map[Outpoint]Tip
	replay      map[replayKey]bool
	// full is sticky after losing a closure or a seen-unspent record.
	full bool
}

// ObserveJournal holds only observe-grade, in-process state. All records are
// per (network, owner), with separate per-owner and total replay bounds. It has
// no persistence, serialized form or closure-export API. Per-command bracketed
// observations are not continuous O8.3 observation and must never feed enforcement.
// Stale, older and equal-height different-hash observations write no
// chain-derived state. Consumer-side replay pairs are recorded for every
// instruct observation, even under such views. A closure requires a later
// height or the same height and hash as this reader's unspent observation;
// transient states create none.
// Lost closure/seen-unspent capacity marks that owner full for the journal's
// lifetime. Replay exhaustion refuses new pairs, without eviction. A new owner
// beyond MaxJournalOwners refuses at step 4.
type ObserveJournal struct {
	mu             sync.Mutex
	owners         map[journalOwner]*ownerJournal
	replayCount    int
	maxOwners      int
	maxReplay      int
	maxOwnerReplay int
	maxClosures    int
	maxSeen        int
}

// NewObserveJournal returns an empty observe-grade journal.
func NewObserveJournal() *ObserveJournal {
	return &ObserveJournal{owners: map[journalOwner]*ownerJournal{}, maxOwners: MaxJournalOwners,
		maxReplay: MaxReplayKeys, maxOwnerReplay: MaxOwnerReplayKeys, maxClosures: MaxClosures, maxSeen: MaxSeenUnspent}
}

// Close imports an external observe-grade closure; it never exports closure
// data. Invalid spellings refuse ErrInvalid. Capacity loss marks the owner full.
func (j *ObserveJournal) Close(network, owner, key string, seal Outpoint) error {
	if !validNetwork(network) || !validName(owner) {
		return ErrInvalid
	}
	if _, ok := lowerHex(key, 32); !ok {
		return ErrInvalid
	}
	if _, ok := lowerHex(seal.TxID, 32); !ok {
		return ErrInvalid
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	state := j.state(network, owner, true)
	if state == nil {
		return ErrJournalFull
	}
	return j.close(state, key, seal)
}

// closedFor is intentionally private: closures cannot leave this package.
func (j *ObserveJournal) closedFor(network, owner, key string, seal Outpoint) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	state := j.state(network, owner, false)
	return state != nil && state.closed(key, seal)
}

// SeenUnspent reports whether this reader observed a seal as unspent.
func (j *ObserveJournal) SeenUnspent(network, owner string, seal Outpoint) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	state := j.state(network, owner, false)
	if state == nil {
		return false
	}
	_, ok := state.seenUnspent[seal]
	return ok
}

// Full reports whether the owner lost required closure or observation capacity.
func (j *ObserveJournal) Full(network, owner string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	state := j.state(network, owner, false)
	return state != nil && state.full
}

// Head returns the journaled list head; it contains no closures.
func (j *ObserveJournal) Head(network, owner string) (Head, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	state := j.state(network, owner, false)
	if state == nil || !state.hasHead {
		return Head{}, false
	}
	return state.head, true
}

func emptyOwnerJournal() *ownerJournal {
	return &ownerJournal{closedKeys: map[string]observeClosure{}, closedSeals: map[Outpoint]observeClosure{},
		seenUnspent: map[Outpoint]Tip{}, replay: map[replayKey]bool{}}
}

func (j *ObserveJournal) state(network, owner string, create bool) *ownerJournal {
	key := journalOwner{network, owner}
	if state, ok := j.owners[key]; ok {
		return state
	}
	if !create || len(j.owners) >= j.maxOwners {
		return nil
	}
	state := emptyOwnerJournal()
	j.owners[key] = state
	return state
}

func (j *ObserveJournal) close(state *ownerJournal, key string, seal Outpoint) error {
	_, hasKey := state.closedKeys[key]
	_, hasSeal := state.closedSeals[seal]
	if (!hasKey && len(state.closedKeys) >= j.maxClosures) || (!hasSeal && len(state.closedSeals) >= j.maxClosures) {
		state.full = true
		return ErrJournalFull
	}
	state.closedKeys[key] = observeClosure{}
	state.closedSeals[seal] = observeClosure{}
	return nil
}

// sawUnspentSet checks capacity for the complete set before changing any
// seen-unspent entry, including the tips of entries already present.
func (j *ObserveJournal) sawUnspentSet(state *ownerJournal, observations map[Outpoint]Tip) error {
	additional := 0
	for seal := range observations {
		if _, ok := state.seenUnspent[seal]; !ok {
			additional++
		}
	}
	if len(state.seenUnspent)+additional > j.maxSeen {
		state.full = true
		return ErrJournalFull
	}
	for seal, tip := range observations {
		if observed, ok := state.seenUnspent[seal]; !ok || observationCanFollow(tip, observed) {
			state.seenUnspent[seal] = tip
		}
	}
	return nil
}

func (s *ownerJournal) closed(key string, seal Outpoint) bool {
	_, hasKey := s.closedKeys[key]
	_, hasSeal := s.closedSeals[seal]
	return hasKey || hasSeal
}

// observationCanFollow excludes older heights and unordered competing hashes
// at equal height. Repeated observations at the same height and hash can follow
// one another (for example after a mempool change).
func observationCanFollow(tip, observed Tip) bool {
	return tip.Height > observed.Height || (tip.Height == observed.Height && tip.Hash == observed.Hash)
}

type headDecision int

const (
	headKeep headDecision = iota
	headTake
	headRollback
)

// decideHead applies the contract's height approximation only after the caller
// excludes older or equal-height different-hash tips. Equal sequences with
// different pinned hashes are conflicts, never reorganisation candidates. Equal pins at greater name
// heights retain the earliest operation. Closures are untouched by head changes.
func decideHead(state *ownerJournal, pin Pin, op NameOp) headDecision {
	if !state.hasHead {
		return headTake
	}
	if pin.Seq == state.head.Seq && pin.SHA256 != state.head.SHA256 {
		return headRollback
	}
	if pin.Seq > state.head.Seq {
		return headTake
	}
	journaled := state.head.Op
	if pin.Seq < state.head.Seq {
		if op.Height > journaled.Height {
			return headRollback
		}
		return headTake
	}
	if op.Height < journaled.Height || (op.Height == journaled.Height && op.BlockHash != journaled.BlockHash) {
		return headTake
	}
	return headKeep
}
