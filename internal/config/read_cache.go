package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/relux-works/a2a-swarm/internal/audience"
)

// ReadCache belongs to one consumer and identity. It retains at most one State
// and one mailbox, never errors or absent files. Legacy State hits compare exact
// bytes; marked State hits reuse the validated State by stamp, then reread and
// validate the current sidecar before using its provider-owned projection.
// Idle bind reuses its writer-seal result only for that exact cached State
// stamp; current connection, migration, sidecar and operation-fence checks run
// on every use.
// Paths without an explicitly attached cache retain the ordinary read path.
// Cache admission limits retained data, never accepted input.
type ReadCache struct {
	identity         string
	mu               sync.Mutex
	statePath        string
	stateStamp       FileStamp
	stateSealStamp   FileStamp
	stateSealChecked bool
	stateSealValid   bool
	stateDir         directoryStamp
	state            State
	durable          bool
	mailboxPath      string
	mailboxStamp     FileStamp
	mailboxRaw       []byte
	mailbox          *Mailbox
	mailboxInc       *mailboxIncState
}

// mailboxIncState is one consumer's incremental mailbox snapshot: the parsed
// mailbox after the complete lines in file[:offset] of the file identified
// by (dev, ino), the digest of that prefix, and the digest and size of the
// whole file as read. It is immutable once stored; a later remember replaces
// the pointer. Every use re-verifies the file identity and the prefix digest
// against the current file, so a stale snapshot can only miss, never serve
// wrong content. visible, when non-nil, is base with a valid unterminated
// final line applied, matching a full parse of the remembered bytes; the
// incremental path itself never applies an unterminated line.
type mailboxIncState struct {
	path      string
	dev, ino  uint64
	offset    int64
	prefixSum [32]byte
	fileSize  int64
	fileSum   [32]byte
	base      *Mailbox
	visible   *Mailbox
}

// incSnapshot returns the stored incremental snapshot for name and path.
func (c *ReadCache) incSnapshot(name, path string) *mailboxIncState {
	if c == nil || name != c.identity {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mailboxInc == nil || c.mailboxInc.path != path {
		return nil
	}
	return c.mailboxInc
}

// rememberInc stores inc as the incremental snapshot. The caller guarantees
// inc's mailboxes are fresh clones that nothing else mutates.
func (c *ReadCache) rememberInc(name string, inc *mailboxIncState) {
	if c == nil || name != c.identity || inc == nil {
		return
	}
	c.mu.Lock()
	c.mailboxInc = inc
	c.mu.Unlock()
}

// updateRawAfterIncremental extends the exact-byte cache after an incremental
// read: the new raw file is the remembered raw prefix plus the bytes read
// past the parsed offset. It runs only when the whole file stays within the
// raw cache admission bound; otherwise the raw entry is dropped, as a full
// parse would drop it.
func (c *ReadCache) updateRawAfterIncremental(name, path string, stamp FileStamp, inc *mailboxIncState, appended []byte, m *Mailbox, total int64) {
	if c == nil || name != c.identity {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if total > mailboxCacheMaxInput {
		if c.mailboxPath == path {
			c.mailbox = nil
			c.mailboxRaw = nil
			c.mailboxPath = ""
		}
		return
	}
	if c.mailboxPath != path || int64(len(c.mailboxRaw)) != inc.fileSize {
		return
	}
	raw := make([]byte, 0, total)
	raw = append(raw, c.mailboxRaw[:inc.offset]...)
	raw = append(raw, appended...)
	if int64(len(raw)) != total {
		return
	}
	c.mailboxPath, c.mailboxStamp, c.mailbox = path, stamp, cloneMailbox(m)
	c.mailboxRaw = raw
}

func NewReadCache(identity string) *ReadCache { return &ReadCache{identity: identity} }

// WithReadCache returns a Paths copy sharing this consumer's cache.
func (p Paths) WithReadCache(c *ReadCache) Paths { p.readCache = c; return p }

// ReadCache returns the explicitly attached consumer cache, if any.
func (p Paths) ReadCache() *ReadCache { return p.readCache }

func (c *ReadCache) forgetStateLocked() {
	c.statePath = ""
	c.stateStamp = FileStamp{}
	c.stateSealStamp = FileStamp{}
	c.stateSealChecked = false
	c.stateSealValid = false
	c.stateDir = directoryStamp{}
	c.state = State{}
	c.durable = false
}

func (c *ReadCache) forgetStateDurabilityLocked() {
	c.durable = false
	c.stateDir = directoryStamp{}
}

func (c *ReadCache) invalidateState(name, path string) {
	if c == nil || name != c.identity {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if path == c.statePath {
		c.forgetStateLocked()
	}
}

func (c *ReadCache) forgetMailboxBytesLocked() {
	c.mailboxPath = ""
	c.mailboxStamp = FileStamp{}
	c.mailboxRaw = nil
	c.mailbox = nil
}

func (c *ReadCache) forgetMailboxLocked() {
	c.forgetMailboxBytesLocked()
	c.mailboxInc = nil
}

// Any mailbox mutation invalidates this consumer's cached payload copies,
// including excluded records. Redaction cannot leave an authorized stale copy.
func (c *ReadCache) invalidateMailbox(path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mailboxPath == path || c.mailboxInc != nil && c.mailboxInc.path == path {
		c.forgetMailboxLocked()
	}
}

func (c *ReadCache) stateHit(name, path string, stamp, partStamp FileStamp) (State, bool) {
	if c == nil || name != c.identity {
		return State{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != c.statePath {
		return State{}, false
	}
	if !stamp.Present {
		c.forgetStateLocked()
		return State{}, false
	}
	if !stamp.Equal(c.stateStamp) {
		c.forgetStateLocked()
		return State{}, false
	}
	durableBeforeRead := c.durable
	if durableBeforeRead && !stampStateDirectory(filepath.Dir(path)).Equal(c.stateDir) {
		c.forgetStateDurabilityLocked()
	}
	if bindPartMarker(c.state) {
		// The State stamp protects the cached legacy document. The sidecar is
		// reopened and authenticated by LoadState on every hit, so its revision
		// can advance without forcing a large State read or trusting its stamp.
		return cloneState(&c.state)
	}
	if partStamp.Present {
		c.forgetStateLocked()
		return State{}, false
	}
	raw, err := readStateFile(path)
	if err != nil {
		c.forgetStateDurabilityLocked()
		return State{}, false
	}
	if !bytes.Equal(raw, c.state.rawState) {
		c.forgetStateLocked()
		return State{}, false
	}
	if durableBeforeRead && !stampStateDirectory(filepath.Dir(path)).Equal(c.stateDir) {
		c.forgetStateDurabilityLocked()
	}
	return cloneState(&c.state)
}

// markedStateGuardHit returns the immutable cached State header to a caller
// that only needs marker, rollback and small authority records. The caller
// must keep the normal State lock and validate the current sidecar before
// acting; the large cached slices are shared read-only and must not escape.
func (c *ReadCache) markedStateGuardHit(name, path string, stamp FileStamp) (State, bool) {
	if c == nil || name != c.identity {
		return State{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != c.statePath {
		return State{}, false
	}
	if !stamp.Present {
		c.forgetStateLocked()
		return State{}, false
	}
	if !stamp.Equal(c.stateStamp) {
		c.forgetStateLocked()
		return State{}, false
	}
	if c.durable && !stampStateDirectory(filepath.Dir(path)).Equal(c.stateDir) {
		c.forgetStateDurabilityLocked()
	}
	if !bindPartMarker(c.state) {
		return State{}, false
	}
	return c.state, true
}

// identityMigrationHit answers the one field CheckIdentityMigration needs
// without copying the cached State's large delivery history. It applies the
// same path stamp, host-notification and bind-sidecar checks as LoadState;
// callers still fall back to LoadState whenever this cache cannot prove a hit.
func (c *ReadCache) identityMigrationHit(p Paths, name string, stamp FileStamp) (*IdentityMigration, bool, error) {
	if c == nil || name != c.identity {
		return nil, false, nil
	}
	path := p.StateFile(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != c.statePath || !stamp.Present || !stamp.Equal(c.stateStamp) {
		return nil, false, nil
	}
	if c.durable && !stampStateDirectory(filepath.Dir(path)).Equal(c.stateDir) {
		c.forgetStateDurabilityLocked()
	}
	state := c.state
	if !bindPartMarker(state) {
		// Unmarked State cache hits require exact-byte comparison. Leave that
		// legacy behavior to LoadState instead of treating a stat stamp as proof.
		return nil, false, nil
	}
	part, sidecarStamp, err := readBindPart(p, name)
	if err != nil {
		return nil, true, err
	}
	base, err := verifiedBindPartBase(state, part, sidecarStamp)
	if err != nil {
		return nil, true, err
	}
	if !reflect.DeepEqual(state.bindPartShadow, base.bindPartShadow) || state.bindPartRollback != base.bindPartRollback {
		// Only provider metadata changes here. Retain the cached State graph
		// and refresh the small compatibility shadow in place under the cache
		// lock, matching LoadState's invalidation/readback behavior.
		c.state.bindPartShadow = base.bindPartShadow
		c.state.bindPartStamp = base.bindPartStamp
		c.state.bindPartRollback = base.bindPartRollback
	}
	if err := state.validateHostNotifications(); err != nil {
		return nil, true, err
	}
	return copyValuePtr(state.Migration), true, nil
}

// terminalBindingHit returns an owned terminal-only projection for the bind
// loop. The legacy cache remains immutable to callers, while large delivery
// slices are not copied when the loop only needs its small terminal record.
func (c *ReadCache) terminalBindingHit(p Paths, name string, stamp FileStamp) (*TerminalBinding, bool, error) {
	if c == nil || name != c.identity {
		return nil, false, nil
	}
	path := p.StateFile(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != c.statePath || !stamp.Present || !stamp.Equal(c.stateStamp) {
		return nil, false, nil
	}
	if c.durable && !stampStateDirectory(filepath.Dir(path)).Equal(c.stateDir) {
		c.forgetStateDurabilityLocked()
	}
	state := c.state
	if !bindPartMarker(state) {
		return nil, false, nil
	}
	part, sidecarStamp, err := readBindPart(p, name)
	if err != nil {
		return nil, true, err
	}
	base, err := verifiedBindPartBase(state, part, sidecarStamp)
	if err != nil {
		return nil, true, err
	}
	if !reflect.DeepEqual(state.bindPartShadow, base.bindPartShadow) || state.bindPartRollback != base.bindPartRollback {
		c.state.bindPartShadow = base.bindPartShadow
		c.state.bindPartStamp = base.bindPartStamp
		c.state.bindPartRollback = base.bindPartRollback
	}
	if err := state.validateHostNotifications(); err != nil {
		return nil, true, err
	}
	var terminal *TerminalBinding
	if state.Terminal != nil {
		var ok bool
		terminal, ok = cloneTerminal(state.Terminal)
		if !ok {
			return nil, false, nil
		}
	}
	if part.Rollback == bindPartRollbackOK {
		view := State{Terminal: terminal}
		applyBindProjection(&view, part.projection())
		terminal = view.Terminal
	}
	return terminal, true, nil
}

func (c *ReadCache) bindIdleDeliveryHit(p Paths, name string, stamp FileStamp) (BindIdleDeliveryState, bool, error) {
	if c == nil || name != c.identity {
		return BindIdleDeliveryState{}, false, nil
	}
	path := p.StateFile(name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != c.statePath || !stamp.Present || !stamp.Equal(c.stateStamp) {
		return BindIdleDeliveryState{}, false, nil
	}
	if c.durable && !stampStateDirectory(filepath.Dir(path)).Equal(c.stateDir) {
		c.forgetStateDurabilityLocked()
	}
	state := c.state
	if !bindPartMarker(state) {
		return BindIdleDeliveryState{}, false, nil
	}
	part, sidecarStamp, err := readBindPart(p, name)
	if err != nil {
		return BindIdleDeliveryState{}, true, err
	}
	base, err := verifiedBindPartBase(state, part, sidecarStamp)
	if err != nil {
		return BindIdleDeliveryState{}, true, err
	}
	if !reflect.DeepEqual(state.bindPartShadow, base.bindPartShadow) || state.bindPartRollback != base.bindPartRollback {
		c.state.bindPartShadow = base.bindPartShadow
		c.state.bindPartStamp = base.bindPartStamp
		c.state.bindPartRollback = base.bindPartRollback
	}
	if err := state.validateHostNotifications(); err != nil {
		return BindIdleDeliveryState{}, true, err
	}
	sealValid := false
	if c.stateSealChecked && c.stateSealStamp.Equal(stamp) {
		sealValid = c.stateSealValid
	} else {
		sealValid = validWriterSeal(state)
		c.stateSealStamp = stamp
		c.stateSealChecked = true
		c.stateSealValid = sealValid
	}
	if !stamp.Equal(StampFile(path)) {
		return BindIdleDeliveryState{}, false, nil
	}
	view := BindIdleDeliveryStateFromState(state)
	view.Marked = true
	view.Active = part.Rollback == bindPartRollbackOK
	view.SealValid = sealValid
	view.stateStamp = stamp
	if part.Rollback == bindPartRollbackOK {
		view.NextBatch = part.NextBatch
		view.Generation = part.Generation
		view.Stopped = part.Stopped
	}
	return view, true, nil
}

func (c *ReadCache) rememberState(name, path string, before FileStamp, s State, durable bool) {
	c.rememberStateWithDirectory(name, path, before, s, durable, directoryStamp{})
}

func (c *ReadCache) rememberStateWithDirectory(name, path string, before FileStamp, s State, durable bool, writtenDir directoryStamp) {
	if c == nil || name != c.identity {
		return
	}
	after := StampFile(path)
	currentDir := stampStateDirectory(filepath.Dir(path))
	c.mu.Lock()
	defer c.mu.Unlock()
	// Invalidate even on uncertainty; no stale frame is a fallback.
	if !before.Present || !before.Equal(after) || before.Size > stateMemoMaxInput {
		c.forgetStateLocked()
		return
	}
	copy, ok := cloneState(&s)
	if !ok {
		c.forgetStateLocked()
		return
	}
	inheritedDurability := c.durable && path == c.statePath && before.Equal(c.stateStamp) && bytes.Equal(s.rawState, c.state.rawState) && currentDir.Equal(c.stateDir)
	if c.durable && !inheritedDurability {
		c.forgetStateDurabilityLocked()
	}
	ownDurability := durable && writtenDir.Equal(currentDir)
	c.durable = ownDurability || inheritedDurability
	c.statePath, c.stateStamp, c.state = path, before, copy
	c.stateSealStamp = FileStamp{}
	c.stateSealChecked = false
	c.stateSealValid = false
	if c.durable {
		c.stateDir = currentDir
	} else {
		c.stateDir = directoryStamp{}
	}
}

// rememberWrittenState binds decoded written bytes to the actual synced
// inode's stamp captured by the atomic writer, never a later path occupant.
func (c *ReadCache) rememberWrittenState(p Paths, name string, s State, data []byte, evidence stateWriteEvidence) {
	if c == nil || name != c.identity {
		return
	}
	// Cache marked State as its on-disk legacy base. The unchanged-write
	// elision path can pass the merged provider view here, so restore the fixed
	// compatibility shadow before retaining it for the next cache hit.
	if bindPartMarker(s) && s.bindPartRollback == bindPartRollbackOK && s.bindPartShadow != nil {
		if s.Terminal != nil {
			terminal := *s.Terminal
			terminal.OwnNotice = cloneOwnNotice(terminal.OwnNotice)
			s.Terminal = &terminal
		}
		applyBindProjection(&s, *s.bindPartShadow)
	}
	s.rawState = cloneBytes(data)
	path := p.StateFile(name)
	if !evidence.file.Known {
		// The strict Host writer does not export its pinned descriptor stamp.
		// It can still refresh decoded data after an exact-byte reread, but
		// this must never mint the ordinary no-op durability shortcut.
		stamp := StampFile(path)
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, data) {
			stamp = FileStamp{}
		}
		c.rememberState(name, path, stamp, s, false)
		return
	}
	c.rememberStateWithDirectory(name, path, evidence.file, s, true, evidence.directory)
}

// unchangedDurable skips even encoding/sync only for our own completed durable
// write, with a still-matching stamp, bytes and separately verified seal under this
// state lock. A foreign compatible seal alone is not durability evidence.
// Unsupported schema, unknown stamps and any typed difference fall back to
// the existing writer, including its durability and strict Host-v2 fences.
func (c *ReadCache) unchangedDurable(name, path string, old, next State) bool {
	if c == nil || name != c.identity || !stateCloneSchemaOK {
		return false
	}
	stamp := StampFile(path)
	dir := stampStateDirectory(filepath.Dir(path))
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.durable || path != c.statePath {
		return false
	}
	if !stamp.Equal(c.stateStamp) || !dir.Equal(c.stateDir) || !bytes.Equal(old.rawState, c.state.rawState) {
		c.forgetStateDurabilityLocked()
		return false
	}
	if old.Writer == nil || next.Writer == nil {
		return false
	}
	raw, err := readStateFile(path)
	if err != nil || !bytes.Equal(raw, c.state.rawState) || !dir.Equal(stampStateDirectory(filepath.Dir(path))) {
		c.forgetStateDurabilityLocked()
		return false
	}
	w := *next.Writer
	w.Seal = old.Writer.Seal
	next.Writer = &w
	return reflect.DeepEqual(old, next)
}

func cloneMailbox(src *Mailbox) *Mailbox {
	m := *src
	m.pendingFilter = nil // A consumer-local view never enters the cached snapshot.
	m.audiences = make(map[audience.Key]audience.Evidence, len(src.audiences))
	for k, e := range src.audiences {
		m.audiences[k] = e
	}
	m.order = cloneStrings(src.order)
	m.received = cloneStrings(src.received)
	m.live = make(map[string]*MailEntry, len(src.live))
	for id, entry := range src.live {
		e := *entry
		e.Event = cloneRaw(entry.Event)
		m.live[id] = &e
	}
	m.recv = make(map[string]bool, len(src.recv))
	for id, v := range src.recv {
		m.recv[id] = v
	}
	m.redacted = make(map[Redaction]bool, len(src.redacted))
	for id, v := range src.redacted {
		m.redacted[id] = v
	}
	return &m
}

func (c *ReadCache) mailboxHit(name, path string, stamp FileStamp) (*Mailbox, bool) {
	if c == nil || name != c.identity || !stamp.Present {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mailbox == nil || path != c.mailboxPath {
		return nil, false
	}
	if !stamp.Equal(c.mailboxStamp) {
		// A changed file may be an append. Drop the exact-byte result but keep
		// the incremental snapshot for its own identity and prefix verification.
		c.forgetMailboxBytesLocked()
		return nil, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	// One extra byte detects growth without retaining an unbounded append.
	raw := make([]byte, len(c.mailboxRaw)+1)
	n, err := io.ReadFull(f, raw)
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false
	}
	if !bytes.Equal(raw[:n], c.mailboxRaw) {
		// The incremental path verifies the complete parsed prefix before it
		// can reuse any prior mailbox state.
		c.forgetMailboxBytesLocked()
		return nil, false
	}
	return cloneMailbox(c.mailbox), true
}

const mailboxCacheMaxInput = 8 << 20

func (c *ReadCache) rememberMailbox(name, path string, before FileStamp, raw []byte, m *Mailbox) {
	if c == nil || name != c.identity {
		return
	}
	after := StampFile(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !before.Present || !before.Equal(after) || before.Size > mailboxCacheMaxInput || before.Size != int64(len(raw)) {
		c.forgetMailboxLocked()
		return
	}
	c.mailboxPath, c.mailboxStamp, c.mailbox = path, before, cloneMailbox(m)
	c.mailboxRaw = cloneBytes(raw)
}
