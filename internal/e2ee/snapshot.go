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

func (c Codec) String() string   { return "<e2ee-codec>" }
func (c Codec) GoString() string { return c.String() }
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
	return c.seal(b, raw)
}

// Typed snapshot IO inventories its existing value instead of parsing JSON again.
func (c *Codec) seal(b Binding, raw []byte) ([]byte, error) {
	if !b.valid() {
		return nil, ErrInvalid
	}
	if len(raw) > MaxSnapshot {
		return nil, ErrCapacity
	}
	out := make([]byte, 24, len(raw)+envelopeOverhead)
	return c.sealInto(b, out, raw)
}

// sealInto permits exact plaintext/output overlap after the envelope prefix.
// Typed Save encodes into this owned buffer; raw Seal keeps caller input intact.
func (c *Codec) sealInto(b Binding, out, raw []byte) ([]byte, error) {
	if !b.valid() {
		return nil, ErrInvalid
	}
	if len(raw) > MaxSnapshot {
		return nil, ErrCapacity
	}
	a := c.aead()
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
	raw, err := c.unseal(b, sealed)
	if err == nil {
		c.secrets.ObserveJSON(raw)
	}
	return raw, err
}
func (c *Codec) unseal(b Binding, sealed []byte) ([]byte, error) {
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

// validate checks the single value, including exact canonical encoded budgets.
// It does not marshal it or decode it again. Opaque evidence is scanned at its
// actual nesting depth; all other fields are generated from the typed schema.
func (s Snapshot) validate() (int, error) {
	if s.Version != 1 || s.Revision == 0 || s.Engine.Version != 1 {
		return 0, ErrCodec
	}
	if err := s.validateBounds(); err != nil {
		return 0, err
	}
	for _, pin := range s.Pins {
		if err := pin.Validate(); err != nil {
			return 0, err
		}
	}
	base, prepared, total, err := snapshotSizes(s)
	if err != nil {
		return 0, err
	}
	if base > MaxSnapshot-RecoveryReserve {
		return 0, ErrCapacity
	}
	if s.Prepared != nil && (prepared > RecoveryReserve || !s.Prepared.valid()) {
		return 0, ErrCapacity
	}
	return total, nil
}

func (s Snapshot) Marshal() ([]byte, error) {
	size, err := s.validate()
	if err != nil {
		return nil, err
	}
	// Public owned-JSON helper: one standard-library encoding. SaveSnapshot uses
	// Encoder directly into its AEAD input buffer with the same value validation.
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, ErrCodec
	}
	if len(raw) > MaxSnapshot {
		return nil, ErrCapacity
	}
	// A schema/size-counter drift must fail closed, never silently weaken either
	// partition budget. This check uses the sole encoded output, without parsing it.
	if len(raw) != size {
		return nil, ErrCodec
	}
	return raw, nil
}
func DecodeSnapshot(raw []byte) (Snapshot, error) {
	if len(raw) > MaxSnapshot {
		return Snapshot{}, ErrCodec
	}
	if _, err := scanJSON(raw, 0); err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return Snapshot{}, ErrCodec
	}
	if _, err := s.validate(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}
