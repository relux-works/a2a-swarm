package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
)

// ReadCache belongs to one consumer and identity. It retains at most one State
// and one mailbox, never errors or absent files. Stamps only pre-check reuse:
// every hit also reads and compares the exact file bytes, without decoding.
// Callers still run every connection, migration, seal and fence check.
// Paths without an explicitly attached cache retain the ordinary read path.
// Cache admission limits retained data, never accepted input.
type ReadCache struct {
	identity     string
	mu           sync.Mutex
	statePath    string
	stateStamp   FileStamp
	stateDir     directoryStamp
	state        State
	durable      bool
	mailboxPath  string
	mailboxStamp FileStamp
	mailboxRaw   []byte
	mailbox      *Mailbox
}

func NewReadCache(identity string) *ReadCache { return &ReadCache{identity: identity} }

// WithReadCache returns a Paths copy sharing this consumer's cache.
func (p Paths) WithReadCache(c *ReadCache) Paths { p.readCache = c; return p }

// ReadCache returns the explicitly attached consumer cache, if any.
func (p Paths) ReadCache() *ReadCache { return p.readCache }

func (c *ReadCache) forgetStateLocked() {
	c.statePath = ""
	c.stateStamp = FileStamp{}
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

func (c *ReadCache) forgetMailboxLocked() {
	c.mailboxPath = ""
	c.mailboxStamp = FileStamp{}
	c.mailboxRaw = nil
	c.mailbox = nil
}

func (c *ReadCache) stateHit(name, path string, stamp FileStamp) (State, bool) {
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
		c.forgetMailboxLocked()
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
		c.forgetMailboxLocked()
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
