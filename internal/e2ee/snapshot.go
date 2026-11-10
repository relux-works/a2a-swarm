package e2ee

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
)

// Codec is AES-256-GCM around bounded payload bytes, with a fixed binary v1
// header. Length/version are checked before allocation/unseal; identity and
// generation are AAD, so a file cannot be moved to another binding or device.
// The serialized envelope is at most MaxSnapshot+header+nonce+GCM tag.
type Codec struct {
	key     [32]byte
	secrets *Inventory
}

func (c *Codec) String() string   { return "<e2ee-codec>" }
func (c *Codec) GoString() string { return c.String() }
func NewCodec(key []byte, secrets *Inventory) (*Codec, error) {
	if len(key) != 32 || secrets == nil {
		return nil, ErrInvalid
	}
	secrets.Register(key)
	c := &Codec{secrets: secrets}
	copy(c.key[:], key)
	return c, nil
}

var snapshotMagic = []byte{'A', '2', 'A', 'E', '2', 'E', 'E', 1}

const envelopeOverhead = 8 + 4 + 12 + 16

func (c *Codec) aead() cipher.AEAD {
	block, _ := aes.NewCipher(c.key[:])
	aead, _ := cipher.NewGCM(block)
	return aead
}
func aad(b Binding) []byte {
	raw, _ := json.Marshal(b)
	return append([]byte("a2a-e2ee-snapshot-v1\x00"), raw...)
}
func (c *Codec) Seal(b Binding, raw []byte) ([]byte, error) {
	if !b.valid() {
		return nil, ErrInvalid
	}
	if len(raw) > MaxSnapshot {
		return nil, ErrCapacity
	}
	c.secrets.ObserveJSON(raw)
	a := c.aead()
	out := make([]byte, 12+a.NonceSize(), len(raw)+envelopeOverhead)
	copy(out, snapshotMagic)
	binary.BigEndian.PutUint32(out[8:12], uint32(len(raw)))
	nonce := out[12:]
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrUnavailable
	}
	auth := append(aad(b), out[:12]...)
	return a.Seal(out, nonce, raw, auth), nil
}
func (c *Codec) Unseal(b Binding, sealed []byte) ([]byte, error) {
	if !b.valid() {
		return nil, ErrInvalid
	}
	if len(sealed) < envelopeOverhead || len(sealed) > MaxSnapshot+envelopeOverhead || !bytes.Equal(sealed[:8], snapshotMagic) {
		return nil, ErrCodec
	}
	size := binary.BigEndian.Uint32(sealed[8:12])
	if size > MaxSnapshot || int(size)+envelopeOverhead != len(sealed) {
		return nil, ErrCodec
	}
	auth := append(aad(b), sealed[:12]...)
	raw, err := c.aead().Open(nil, sealed[12:24], sealed[24:], auth)
	if err != nil {
		return nil, ErrIntegrity
	}
	c.secrets.ObserveJSON(raw)
	return raw, nil
}
func (l *LockedStore) WriteSealed(c *Codec, b Binding, raw []byte, createOnly bool) error {
	sealed, err := c.Seal(b, raw)
	if err != nil {
		return err
	}
	return l.publish("state.bin", sealed, createOnly)
}
func (l *LockedStore) ReadSealed(c *Codec, b Binding) ([]byte, error) {
	sealed, err := l.read("state.bin", MaxSnapshot+envelopeOverhead)
	if err != nil {
		return nil, err
	}
	return c.Unseal(b, sealed)
}

type Snapshot struct {
	Version   int                     `json:"v"`
	Revision  uint64                  `json:"revision"`
	Engine    EngineState             `json:"engine"`
	Pins      []PinRecord             `json:"pins"`
	Replay    map[string]ReplayWindow `json:"replay"`
	Retention Retention               `json:"retention"`
	Prepared  *Prepared               `json:"prepared,omitempty"`
}

func (s Snapshot) Marshal() ([]byte, error) {
	if s.Version != 1 || s.Revision == 0 || s.Engine.Version != 1 {
		return nil, ErrCodec
	}
	for _, pin := range s.Pins {
		if err := pin.Validate(); err != nil {
			return nil, err
		}
	}
	// Reserve space independently of current prepared size. Protected evidence is
	// never evicted to fit a batch; the caller must prune or stop before mutation.
	base := s
	base.Prepared = nil
	raw, err := json.Marshal(base)
	if err != nil {
		return nil, ErrCodec
	}
	if len(raw) > MaxSnapshot-RecoveryReserve {
		return nil, ErrCapacity
	}
	if s.Prepared != nil {
		p, err := json.Marshal(s.Prepared)
		if err != nil {
			return nil, ErrCodec
		}
		if len(p) > RecoveryReserve || !s.Prepared.valid() {
			return nil, ErrCapacity
		}
	}
	raw, err = json.Marshal(s)
	if err != nil {
		return nil, ErrCodec
	}
	if len(raw) > MaxSnapshot {
		return nil, ErrCapacity
	}
	// Opaque prepared evidence must not make our own output unreadable (e.g.
	// duplicate fields or excessive nesting). Validate before any publication.
	var check Snapshot
	if err := strictJSON(raw, &check, MaxSnapshot); err != nil {
		return nil, err
	}
	return raw, nil
}
func DecodeSnapshot(raw []byte) (Snapshot, error) {
	var s Snapshot
	if err := strictJSON(raw, &s, MaxSnapshot); err != nil {
		return Snapshot{}, err
	}
	if _, err := s.Marshal(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}
