package config

import (
	"bytes"
	"os"
	"reflect"
	"sync"
)

// ReadCache belongs to one consumer and identity. It retains at most one State
// and one mailbox, never errors or absent files. Stamps validate data reuse,
// not authority: callers still run every connection, migration, seal and fence
// check. It assumes cooperative atomic State replacements and mailbox writes;
// an in-place edit that restores every stamp field is outside this contract.
// Paths without an explicitly attached cache retain the ordinary read path.
// Cache admission limits retained data, never accepted input.
type ReadCache struct {
	identity     string
	mu           sync.Mutex
	statePath    string
	stateStamp   FileStamp
	state        State
	durable      bool
	mailboxPath  string
	mailboxStamp FileStamp
	mailbox      *Mailbox
}

func NewReadCache(identity string) *ReadCache { return &ReadCache{identity: identity} }

// WithReadCache returns a Paths copy sharing this consumer's cache.
func (p Paths) WithReadCache(c *ReadCache) Paths { p.readCache = c; return p }

// ReadCache returns the explicitly attached consumer cache, if any.
func (p Paths) ReadCache() *ReadCache { return p.readCache }

func (c *ReadCache) stateHit(name, path string, stamp FileStamp) (State, bool) {
	if c == nil || name != c.identity || !stamp.Present {
		return State{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != c.statePath || !stamp.Equal(c.stateStamp) {
		return State{}, false
	}
	return cloneState(&c.state)
}

func (c *ReadCache) rememberState(name, path string, before FileStamp, s State, durable bool) {
	if c == nil || name != c.identity {
		return
	}
	after := StampFile(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Invalidate even on uncertainty; no stale frame is a fallback.
	if !before.Present || !before.Equal(after) || before.Size > stateMemoMaxInput {
		c.statePath = ""
		c.state = State{}
		c.durable = false
		return
	}
	copy, ok := cloneState(&s)
	if !ok {
		c.statePath = ""
		c.state = State{}
		c.durable = false
		return
	}
	c.durable = durable || (c.durable && path == c.statePath && before.Equal(c.stateStamp) && bytes.Equal(s.rawState, c.state.rawState))
	c.statePath, c.stateStamp, c.state = path, before, copy
}

// rememberWrittenState binds decoded written bytes to the actual synced
// inode's stamp captured by the atomic writer, never a later path occupant.
func (c *ReadCache) rememberWrittenState(p Paths, name string, s State, data []byte, stamp FileStamp) {
	if c == nil || name != c.identity {
		return
	}
	s.rawState = cloneBytes(data)
	path := p.StateFile(name)
	if !stamp.Known {
		// The strict Host writer does not export its pinned descriptor stamp.
		// It can still refresh decoded data after an exact-byte reread, but
		// this must never mint the ordinary no-op durability shortcut.
		stamp = StampFile(path)
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, data) {
			stamp = FileStamp{}
		}
		c.rememberState(name, path, stamp, s, false)
		return
	}
	c.rememberState(name, path, stamp, s, true)
}

// unchangedDurable skips even encoding/sync only for our own completed durable
// write, with a still-matching stamp and separately verified seal under this
// state lock. A foreign compatible seal alone is not durability evidence.
// Unsupported schema, unknown stamps and any typed difference fall back to
// the existing writer, including its durability and strict Host-v2 fences.
func (c *ReadCache) unchangedDurable(name, path string, old, next State) bool {
	if c == nil || name != c.identity || !stateCloneSchemaOK {
		return false
	}
	stamp := StampFile(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.durable || path != c.statePath || !stamp.Equal(c.stateStamp) || !bytes.Equal(old.rawState, c.state.rawState) || old.Writer == nil || next.Writer == nil {
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
	if c.mailbox == nil || path != c.mailboxPath || !stamp.Equal(c.mailboxStamp) {
		return nil, false
	}
	return cloneMailbox(c.mailbox), true
}

func (c *ReadCache) rememberMailbox(name, path string, before FileStamp, m *Mailbox) {
	if c == nil || name != c.identity {
		return
	}
	after := StampFile(path)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !before.Present || !before.Equal(after) || before.Size > 8<<20 {
		c.mailbox = nil
		c.mailboxPath = ""
		return
	}
	c.mailboxPath, c.mailboxStamp, c.mailbox = path, before, cloneMailbox(m)
}
