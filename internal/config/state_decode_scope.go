package config

import (
	"bytes"
	"hash/maphash"
	"sync"
	"sync/atomic"
)

// Frozen admission bounds of one decode epoch. They limit what the memo
// retains; they are never limits on accepted State input. An input or graph
// beyond them takes the ordinary decoder with no new refusal.
const (
	stateMemoMaxEntries     = 32
	stateMemoMaxInput       = 1 << 20  // bytes of one exact input
	stateMemoMaxRetainedRaw = 16 << 20 // total exact input bytes retained
	stateMemoMaxCharge      = 32 << 20 // conservative parsed-graph charge
	stateMemoMaxNodes       = 262144   // retained parsed-graph nodes
)

type stateMemoLimits struct {
	entries                 int
	input, raw, charge, nod int64
}

var frozenStateMemoLimits = stateMemoLimits{
	entries: stateMemoMaxEntries,
	input:   stateMemoMaxInput,
	raw:     stateMemoMaxRetainedRaw,
	charge:  stateMemoMaxCharge,
	nod:     stateMemoMaxNodes,
}

// StateDecodeScope reuses immutable parsed State data for exact fresh input
// bytes within one explicitly opened epoch of its owning terminal consumer.
// It owns no authority: every LoadState still performs its complete file read
// and every caller guard, and every writer still verifies seals and fences
// under its own lock. A lookup matches only the identical state path and the
// complete identical bytes; the hash is only an index. A hit returns an
// independent typed copy, never the stored frame.
//
// A Paths value carries a scope only through WithStateDecodeScope. A Paths
// without one, a closed epoch, another identity, an oversized input and every
// admission failure use the ordinary decoder.
type StateDecodeScope struct {
	identity string
	seed     maphash.Seed
	limits   stateMemoLimits

	mu     sync.Mutex
	epoch  uint64
	open   bool
	tick   uint64
	frames []*stateFrame
	flight []*stateFlight
	raw    int64
	charge int64
	nodes  int64
	stats  StateDecodeScopeStats
}

// StateDecodeScopeStats are this scope's own diagnostic counts.
type StateDecodeScopeStats struct {
	Epochs, Hits, Misses, Parses, Waits, Admits, Evictions, WriterClones    int64
	FallbackClosed, FallbackIdentity, FallbackOversize, FallbackUnsupported int64
	FallbackNotAdmitted, FallbackUnpublished, FallbackWriterClone           int64
	PeakRaw, PeakCharge, PeakNodes, PeakEntries                             int64
}

type stateFrame struct {
	path     string
	sum      uint64
	state    State // immutable; state.rawState is the exact input
	charge   int64
	nodes    int64
	used     uint64
	borrowed int
}

type stateFlight struct {
	path  string
	sum   uint64
	size  int
	epoch uint64
	done  chan struct{}
}

// NewStateDecodeScope returns a closed scope for identity's own State.
func NewStateDecodeScope(identity string) *StateDecodeScope {
	return &StateDecodeScope{identity: identity, seed: maphash.MakeSeed(), limits: frozenStateMemoLimits}
}

// WithStateDecodeScope returns a copy of p carrying sc. Copies of the returned
// value carry the same scope; newly resolved Paths values carry none.
func (p Paths) WithStateDecodeScope(sc *StateDecodeScope) Paths {
	p.stateDecode = sc
	return p
}

// StateDecodeScope reports the scope p carries, if any.
func (p Paths) StateDecodeScope() *StateDecodeScope { return p.stateDecode }

// Open starts a new epoch. Frames of any earlier epoch are discarded. A nil
// scope is a no-op.
func (sc *StateDecodeScope) Open() {
	if sc == nil {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.resetLocked()
	sc.epoch++
	sc.open = true
	sc.stats.Epochs++
}

// Close ends the current epoch and releases its frames. In-flight parses
// finish against their own bytes and are not published. A nil scope is a no-op.
func (sc *StateDecodeScope) Close() {
	if sc == nil {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.resetLocked()
	sc.open = false
}

func (sc *StateDecodeScope) resetLocked() {
	sc.frames = nil
	sc.raw, sc.charge, sc.nodes = 0, 0, 0
}

// Stats returns a snapshot of this scope's counts.
func (sc *StateDecodeScope) Stats() StateDecodeScopeStats {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.stats
}

func (sc *StateDecodeScope) active(name string) bool {
	if sc == nil || name != sc.identity || !stateCloneSchemaOK {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.open
}

// stateScopeParseHook is a package-private test seam run by a miss owner
// before its pure parse. Production code never sets it.
var stateScopeParseHook func()

// decodeFreshState is the zero-destination decode used by LoadState and the
// post-save reconstruction. Without an active scope it is unmarshalState.
func decodeFreshState(p Paths, name, path string, data []byte) (State, error) {
	sc := p.stateDecode
	if sc == nil {
		var s State
		err := unmarshalState(data, &s)
		return s, err
	}
	return sc.decode(name, path, data)
}

func (sc *StateDecodeScope) ordinary(data []byte, counter *int64) (State, error) {
	if counter != nil {
		sc.mu.Lock()
		*counter++
		sc.mu.Unlock()
	}
	globalStateStats.scopeFallbacks.Add(1)
	var s State
	err := unmarshalState(data, &s)
	return s, err
}

func (sc *StateDecodeScope) decode(name, path string, data []byte) (State, error) {
	if name != sc.identity {
		return sc.ordinary(data, &sc.stats.FallbackIdentity)
	}
	if !stateCloneSchemaOK {
		return sc.ordinary(data, &sc.stats.FallbackUnsupported)
	}
	if int64(len(data)) > sc.limits.input {
		return sc.ordinary(data, &sc.stats.FallbackOversize)
	}
	sum := maphash.Bytes(sc.seed, data)
	sc.mu.Lock()
	if !sc.open {
		sc.stats.FallbackClosed++
		sc.mu.Unlock()
		return sc.ordinary(data, nil)
	}
	epoch := sc.epoch
	waited := false
	for {
		if f := sc.findLocked(path, sum, data); f != nil {
			f.borrowed++
			sc.tick++
			f.used = sc.tick
			sc.mu.Unlock()
			out, ok := cloneState(&f.state)
			sc.mu.Lock()
			f.borrowed--
			if ok {
				sc.stats.Hits++
				sc.mu.Unlock()
				globalStateStats.scopeHits.Add(1)
				return out, nil
			}
			sc.stats.FallbackUnsupported++
			sc.mu.Unlock()
			return sc.ordinary(data, nil)
		}
		fl := sc.flightLocked(path, sum, len(data))
		if fl == nil || waited {
			break
		}
		// Another caller is parsing an input with this index. Wait without
		// the mutex, then look again with full byte equality.
		waited = true
		sc.stats.Waits++
		sc.mu.Unlock()
		<-fl.done
		sc.mu.Lock()
		if !sc.open || sc.epoch != epoch {
			sc.stats.FallbackClosed++
			sc.mu.Unlock()
			return sc.ordinary(data, nil)
		}
	}
	if waited {
		sc.stats.FallbackUnpublished++
		sc.mu.Unlock()
		return sc.ordinary(data, nil)
	}
	fl := &stateFlight{path: path, sum: sum, size: len(data), epoch: epoch, done: make(chan struct{})}
	sc.flight = append(sc.flight, fl)
	sc.stats.Misses++
	sc.mu.Unlock()
	globalStateStats.scopeMisses.Add(1)
	return sc.parseAndPublish(fl, path, sum, data)
}

// parseAndPublish runs only the pure decoder outside the mutex. The completion
// marker is always closed, including on parser failure or panic.
func (sc *StateDecodeScope) parseAndPublish(fl *stateFlight, path string, sum uint64, data []byte) (State, error) {
	defer func() {
		sc.mu.Lock()
		for i, x := range sc.flight {
			if x == fl {
				sc.flight = append(sc.flight[:i], sc.flight[i+1:]...)
				break
			}
		}
		sc.mu.Unlock()
		close(fl.done)
	}()
	if stateScopeParseHook != nil {
		stateScopeParseHook()
	}
	var cand State
	sc.mu.Lock()
	sc.stats.Parses++
	sc.mu.Unlock()
	if err := unmarshalState(data, &cand); err != nil {
		return State{}, err // errors are never frames
	}
	out, ok := cloneState(&cand)
	charge := parsedStateCharge(&cand)
	if !ok || charge.unsupported {
		sc.mu.Lock()
		sc.stats.FallbackUnsupported++
		sc.mu.Unlock()
		return cand, nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if !sc.open || sc.epoch != fl.epoch {
		sc.stats.FallbackClosed++
		return cand, nil
	}
	if !sc.admitLocked(&stateFrame{path: path, sum: sum, state: cand, charge: charge.bytes, nodes: charge.nodes}) {
		sc.stats.FallbackNotAdmitted++
		return cand, nil
	}
	return out, nil
}

func (sc *StateDecodeScope) findLocked(path string, sum uint64, data []byte) *stateFrame {
	for _, f := range sc.frames {
		if f.sum == sum && f.path == path && bytes.Equal(f.state.rawState, data) {
			return f
		}
	}
	return nil
}

func (sc *StateDecodeScope) flightLocked(path string, sum uint64, size int) *stateFlight {
	for _, fl := range sc.flight {
		if fl.sum == sum && fl.size == size && fl.path == path && fl.epoch == sc.epoch {
			return fl
		}
	}
	return nil
}

// admitLocked retains f within every bound, evicting least recently used
// unborrowed frames. It reports false when f cannot fit.
func (sc *StateDecodeScope) admitLocked(f *stateFrame) bool {
	raw := int64(len(f.state.rawState))
	l := sc.limits
	if raw > l.input || raw > l.raw || f.charge > l.charge || f.nodes > l.nod || l.entries < 1 {
		return false
	}
	for len(sc.frames) >= l.entries || sc.raw+raw > l.raw || sc.charge+f.charge > l.charge || sc.nodes+f.nodes > l.nod {
		victim := -1
		for i, x := range sc.frames {
			if x.borrowed == 0 && (victim < 0 || x.used < sc.frames[victim].used) {
				victim = i
			}
		}
		if victim < 0 {
			return false
		}
		v := sc.frames[victim]
		sc.frames = append(sc.frames[:victim], sc.frames[victim+1:]...)
		sc.raw -= int64(len(v.state.rawState))
		sc.charge -= v.charge
		sc.nodes -= v.nodes
		sc.stats.Evictions++
	}
	sc.tick++
	f.used = sc.tick
	sc.frames = append(sc.frames, f)
	sc.raw += raw
	sc.charge += f.charge
	sc.nodes += f.nodes
	sc.stats.Admits++
	sc.stats.PeakRaw = max(sc.stats.PeakRaw, sc.raw)
	sc.stats.PeakCharge = max(sc.stats.PeakCharge, sc.charge)
	sc.stats.PeakNodes = max(sc.stats.PeakNodes, sc.nodes)
	sc.stats.PeakEntries = max(sc.stats.PeakEntries, int64(len(sc.frames)))
	return true
}

// writerWorkingCopy returns the independent working copy of a writer's fresh
// old snapshot. With an active scope it is a typed clone of old, which LoadState
// decoded from exactly old.rawState under the caller's lock; otherwise, and
// whenever the clone cannot cover old, it is the ordinary decode of those bytes.
func writerWorkingCopy(p Paths, name string, old *State) (State, error) {
	sc := p.stateDecode
	if sc.active(name) {
		if s, ok := cloneState(old); ok {
			sc.mu.Lock()
			sc.stats.WriterClones++
			sc.mu.Unlock()
			globalStateStats.writerClones.Add(1)
			return s, nil
		}
		sc.mu.Lock()
		sc.stats.FallbackWriterClone++
		sc.mu.Unlock()
	}
	var s State
	err := unmarshalState(old.rawState, &s)
	return s, err
}

// StateDecodeStats are process-wide diagnostic counts of State reads, actual
// typed decodes and writer verification work. They control nothing.
type StateDecodeStats struct {
	LoadStateCalls, StateReadAttempts, StateReadErrors, StateReadBytes int64
	UnmarshalStatePasses, UnmarshalStateBytes                          int64
	ScopeHits, ScopeMisses, ScopeFallbacks, WriterClones               int64
	StateDigests, CanonicalStates, SealedStates, WriterSealChecks      int64
}

type stateStatCounters struct {
	loadStateCalls, stateReadAttempts, stateReadErrors, stateReadBytes atomic.Int64
	unmarshalPasses, unmarshalBytes                                    atomic.Int64
	scopeHits, scopeMisses, scopeFallbacks, writerClones               atomic.Int64
	stateDigests, canonicalStates, sealedStates, writerSealChecks      atomic.Int64
}

var globalStateStats stateStatCounters

// ReadStateDecodeStats returns a snapshot of successive atomic loads.
func ReadStateDecodeStats() StateDecodeStats {
	g := &globalStateStats
	return StateDecodeStats{
		LoadStateCalls: g.loadStateCalls.Load(), StateReadAttempts: g.stateReadAttempts.Load(),
		StateReadErrors: g.stateReadErrors.Load(), StateReadBytes: g.stateReadBytes.Load(),
		UnmarshalStatePasses: g.unmarshalPasses.Load(), UnmarshalStateBytes: g.unmarshalBytes.Load(),
		ScopeHits: g.scopeHits.Load(), ScopeMisses: g.scopeMisses.Load(), ScopeFallbacks: g.scopeFallbacks.Load(),
		WriterClones: g.writerClones.Load(), StateDigests: g.stateDigests.Load(), CanonicalStates: g.canonicalStates.Load(),
		SealedStates: g.sealedStates.Load(), WriterSealChecks: g.writerSealChecks.Load(),
	}
}
