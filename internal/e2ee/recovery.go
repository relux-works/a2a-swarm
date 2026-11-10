package e2ee

import "encoding/json"

// Cursor identity includes the batch digest and revision: matching a token alone
// never proves that a prepared batch was committed.
type Cursor struct {
	Token    string `json:"token"`
	Revision uint64 `json:"revision"`
	Batch    string `json:"batch"`
}
type Prepared struct {
	Generation string `json:"generation"`
	Since      Cursor `json:"since"`
	Next       Cursor `json:"next"`
	Digest     string `json:"digest"`
	// Opaque bounded evidence is interpreted by U4; it may contain accepted
	// plaintext and stays inside the sealed snapshot, never in cursor metadata.
	Evidence json.RawMessage `json:"evidence"`
}

func (p Prepared) valid() bool {
	return hexSize(p.Generation, 16) && hexSize(p.Digest, 32) && p.Since.Revision < ^uint64(0) && p.Next.Revision == p.Since.Revision+1 && p.Next.Batch == p.Digest && p.Next.Token != "" && p.Next.Token != p.Since.Token && len(p.Next.Token) <= 8192 && len(p.Since.Token) <= 8192 && json.Valid(p.Evidence)
}

// BatchArtifacts must revalidate current policy without crypto, idempotently
// fsync delivery/journal effects, and verify their durable readback. U4 supplies
// this adapter; no mailbox, subscriptions, cursor or sync caller is changed here.
type BatchArtifacts interface {
	Apply(Prepared) error
	Verify(Prepared) error
}

func (l *LockedStore) ReadCursor() (Cursor, error) {
	b, err := l.read("cursor.json", 20000)
	if err != nil {
		return Cursor{}, err
	}
	var c Cursor
	if err = strictJSON(b, &c, 20000); err != nil {
		return Cursor{}, err
	}
	return c, nil
}
func (l *LockedStore) CreateCursor(c Cursor) error {
	if c.Revision != 0 || c.Batch != "" || len(c.Token) > 8192 {
		return ErrInvalid
	}
	b, _ := json.Marshal(c)
	return l.publish("cursor.json", b, true)
}
func (l *LockedStore) advanceCursor(old, next Cursor) error {
	c, err := l.ReadCursor()
	if err != nil {
		return err
	}
	if c != old {
		return ErrConflict
	}
	b, _ := json.Marshal(next)
	return l.publish("cursor.json", b, false)
}
func (l *LockedStore) SaveSnapshot(c *Codec, b Binding, s Snapshot, createOnly bool) error {
	if s.Prepared != nil && s.Prepared.Generation != b.Generation {
		return ErrIntegrity
	}
	raw, err := s.Marshal()
	if err != nil {
		return err
	}
	return l.WriteSealed(c, b, raw, createOnly)
}
func (l *LockedStore) LoadSnapshot(c *Codec, b Binding) (Snapshot, error) {
	raw, err := l.ReadSealed(c, b)
	if err != nil {
		return Snapshot{}, err
	}
	s, err := DecodeSnapshot(raw)
	if err != nil {
		return Snapshot{}, err
	}
	if s.Prepared != nil && s.Prepared.Generation != b.Generation {
		return Snapshot{}, ErrIntegrity
	}
	return s, nil
}

// Recover completes a previously sealed prepared transaction. It has no Engine
// or incoming response argument: even a to-device-only batch cannot be fed to
// Olm here. It rereads authoritative disk state rather than trusting a caller's
// cached snapshot. Recovery always runs before a new sync or preparation.
func (l *LockedStore) Recover(c *Codec, b Binding, artifacts BatchArtifacts) (Snapshot, bool, error) {
	s, err := l.LoadSnapshot(c, b)
	if err != nil {
		return Snapshot{}, false, err
	}
	if s.Prepared == nil {
		return s, false, nil
	}
	p := *s.Prepared
	if !p.valid() {
		return Snapshot{}, false, ErrCodec
	}
	current, err := l.ReadCursor()
	if err != nil {
		return Snapshot{}, false, err
	}
	if current != p.Since && current != p.Next {
		return Snapshot{}, false, ErrConflict
	}
	if artifacts == nil {
		return Snapshot{}, false, ErrInvalid
	}
	if current == p.Since {
		if err = artifacts.Apply(p); err != nil {
			return Snapshot{}, false, err
		}
		if err = artifacts.Verify(p); err != nil {
			return Snapshot{}, false, err
		}
		if err = l.advanceCursor(p.Since, p.Next); err != nil {
			return Snapshot{}, false, err
		}
	}
	// This also handles power loss immediately after cursor rename: ratchets and
	// outcomes remain the ones sealed before the cursor ever advanced.
	current, err = l.ReadCursor()
	if err != nil {
		return Snapshot{}, false, err
	}
	if current != p.Next {
		return Snapshot{}, false, ErrConflict
	}
	if err = artifacts.Verify(p); err != nil {
		return Snapshot{}, false, err
	}
	if s.Revision == ^uint64(0) {
		return Snapshot{}, false, ErrCapacity
	}
	s.Prepared = nil
	s.Revision++
	if err = l.SaveSnapshot(c, b, s, false); err != nil {
		return Snapshot{}, false, err
	}
	check, err := l.LoadSnapshot(c, b)
	if err != nil {
		return Snapshot{}, false, err
	}
	if check.Prepared != nil || check.Revision != s.Revision {
		return Snapshot{}, false, ErrConflict
	}
	return check, true, nil
}
