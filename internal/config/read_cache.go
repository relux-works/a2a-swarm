package config

import (
	"bytes"
	"io"
	"os"
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

func (c *ReadCache) stateHit(name, path string, stamp FileStamp) (State, bool) {
	if c == nil || name != c.identity || !stamp.Present {
		return State{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if path != c.statePath || !stamp.Equal(c.stateStamp) {
		return State{}, false
	}
	raw, err := readStateFile(path)
	if err != nil || !bytes.Equal(raw, c.state.rawState) {
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
// write, with a still-matching stamp, bytes and separately verified seal under this
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
	raw, err := readStateFile(path)
	if err != nil || !bytes.Equal(raw, c.state.rawState) {
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
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	// One extra byte detects growth without retaining an unbounded append.
	raw := make([]byte, len(c.mailboxRaw)+1)
	n, err := io.ReadFull(f, raw)
	if (err != io.EOF && err != io.ErrUnexpectedEOF) || !bytes.Equal(raw[:n], c.mailboxRaw) {
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
		c.mailbox = nil
		c.mailboxRaw = nil
		c.mailboxPath = ""
		return
	}
	c.mailboxPath, c.mailboxStamp, c.mailbox = path, before, cloneMailbox(m)
	c.mailboxRaw = cloneBytes(raw)
}
