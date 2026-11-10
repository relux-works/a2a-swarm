package e2ee

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// CustodyBackend is the typed future seam for prompt-free _a2a custody. U1
// implements only file custody. There is no OS keychain implementation or probe.
type CustodyBackend interface {
	ReadRoot(Pointer) (Root, error)
	CreateRoot(Pointer, Root) error
}

type Pointer struct {
	Version   int     `json:"v"`
	Namespace string  `json:"namespace"`
	Binding   Binding `json:"binding"`
}

func (p Pointer) valid() bool     { return p.Version == 1 && hexSize(p.Namespace, 32) && p.Binding.valid() }
func (p Pointer) Account() string { return "root." + p.Binding.Generation }
func (p Pointer) Service() string { return RootServicePrefix + p.Namespace }

func NewPointer(n Namespace, binding, device string) (Pointer, error) {
	digest, err := n.Digest()
	if err != nil {
		return Pointer{}, err
	}
	var generation [16]byte
	if _, err = rand.Read(generation[:]); err != nil {
		return Pointer{}, ErrUnavailable
	}
	p := Pointer{1, digest, Binding{binding, n.MXID, device, hex.EncodeToString(generation[:])}}
	if !p.valid() {
		return Pointer{}, ErrInvalid
	}
	return p, nil
}

// ValidateServices must receive every configured token/password/registration
// service, not only the selected profile. The legacy token service is implicit.
func ValidateServices(p Pointer, reserved []string) error {
	if !p.valid() {
		return ErrInvalid
	}
	service := p.Service()
	if service == LegacyTokenService || service == PasswordService {
		return ErrConflict
	}
	for _, candidate := range reserved {
		if candidate == service {
			return ErrConflict
		}
	}
	return nil
}

// Root is an immutable generation bundle. Seeds/wrapping key are canonical
// unpadded base64url. It is never a replacement for the sealed device account.
// U2a must derive signing keys with concrete crypto/goolm/pk constructors: the
// crypto/olm PK driver variables are nil without global registration, which U1
// intentionally does not perform.
type Root struct {
	Version int       `json:"v"`
	Binding Binding   `json:"binding"`
	Seeds   [3]string `json:"seeds"`
	Wrap    string    `json:"wrap"`
}

func (r Root) String() string   { return "<e2ee-root>" }
func (r Root) GoString() string { return r.String() }
func NewRoot(p Pointer, secrets *Inventory) (Root, error) {
	if !p.valid() || secrets == nil {
		return Root{}, ErrInvalid
	}
	r := Root{Version: 1, Binding: p.Binding}
	for i := range r.Seeds {
		var key [32]byte
		if _, err := rand.Read(key[:]); err != nil {
			return Root{}, ErrUnavailable
		}
		secrets.Register(key[:])
		r.Seeds[i] = base64.RawURLEncoding.EncodeToString(key[:])
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return Root{}, ErrUnavailable
	}
	secrets.Register(key[:])
	r.Wrap = base64.RawURLEncoding.EncodeToString(key[:])
	return r, nil
}
func keyBytes(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) != 32 || base64.RawURLEncoding.EncodeToString(b) != s {
		return nil, ErrCodec
	}
	return b, nil
}
func (r Root) validate(p Pointer, secrets *Inventory) error {
	if r.Version != 1 || r.Binding != p.Binding || !p.valid() {
		return ErrIntegrity
	}
	for _, s := range append(r.Seeds[:], r.Wrap) {
		b, err := keyBytes(s)
		if err != nil {
			return err
		}
		secrets.Register(b)
	}
	return nil
}
func (r Root) WrappingKey(secrets *Inventory) ([]byte, error) {
	b, err := keyBytes(r.Wrap)
	if err == nil {
		secrets.Register(b)
	}
	return b, err
}

const rootPrefix = "a2a-e2ee-root-v1:"

func encodeRoot(p Pointer, r Root, secrets *Inventory) ([]byte, error) {
	if err := r.validate(p, secrets); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, ErrCodec
	}
	secrets.ObserveJSON(raw)
	out := []byte(rootPrefix + base64.RawURLEncoding.EncodeToString(raw))
	if len(out) > MaxItem {
		return nil, ErrCapacity
	}
	secrets.Register(out)
	return out, nil
}
func decodeRoot(p Pointer, data []byte, secrets *Inventory) (Root, error) {
	secrets.Register(data)
	if len(data) > MaxItem || !strings.HasPrefix(string(data), rootPrefix) {
		return Root{}, ErrCodec
	}
	encoded := strings.TrimPrefix(string(data), rootPrefix)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return Root{}, ErrCodec
	}
	secrets.ObserveJSON(raw)
	var r Root
	if err = strictJSON(raw, &r, MaxItem); err != nil {
		return Root{}, err
	}
	canonical, _ := json.Marshal(r)
	if string(canonical) != string(raw) {
		return Root{}, ErrCodec
	}
	if err = r.validate(p, secrets); err != nil {
		return Root{}, err
	}
	return r, nil
}

// FileCustody always checks the complete reserved-service inventory before any
// lookup/write. The lease covers generation pointer, roots and state together.
type FileCustody struct {
	lease     *LockedStore
	namespace string
	identity  string
	reserved  []string
	secrets   *Inventory
}

func NewFileCustody(lease *LockedStore, n Namespace, reserved []string, secrets *Inventory) (*FileCustody, error) {
	if lease == nil || secrets == nil {
		return nil, ErrInvalid
	}
	digest, err := n.Digest()
	if err != nil {
		return nil, err
	}
	probe := Pointer{1, digest, Binding{strings.Repeat("0", 64), n.MXID, "namespace-check", strings.Repeat("0", 32)}}
	if err = ValidateServices(probe, reserved); err != nil {
		return nil, err
	}
	return &FileCustody{lease: lease, namespace: digest, identity: n.MXID, reserved: append([]string(nil), reserved...), secrets: secrets}, nil
}
func (c *FileCustody) check(p Pointer) error {
	if p.Namespace != c.namespace || p.Binding.Identity != c.identity {
		return ErrIntegrity
	}
	return ValidateServices(p, c.reserved)
}
func (c *FileCustody) ReadPointer(expected Binding) (Pointer, error) {
	b, err := c.lease.read("generation.json", 4096)
	if err != nil {
		return Pointer{}, err
	}
	var p Pointer
	if err = strictJSON(b, &p, 4096); err != nil {
		return Pointer{}, err
	}
	if err = c.check(p); err != nil {
		return Pointer{}, err
	}
	if p.Binding.Binding != expected.Binding || p.Binding.Identity != expected.Identity || p.Binding.Device != expected.Device || (expected.Generation != "" && p.Binding.Generation != expected.Generation) {
		return Pointer{}, ErrIntegrity
	}
	return p, nil
}
func (c *FileCustody) CreatePointer(p Pointer) error {
	if err := c.check(p); err != nil {
		return err
	}
	// Never fill in a missing locator in a directory with roots or state.
	for _, name := range []string{"secrets.json", "state.bin", "cursor.json"} {
		f, err := c.lease.store.open(name)
		if err == nil {
			f.Close()
			return ErrConflict
		}
		if err != ErrUnavailable {
			return err
		}
	}
	b, _ := json.Marshal(p)
	if err := c.lease.publish("generation.json", b, true); err != nil {
		return err
	}
	got, err := c.ReadPointer(p.Binding)
	if err != nil {
		return err
	}
	if got != p {
		return ErrIntegrity
	}
	return nil
}
func (c *FileCustody) requirePointer(p Pointer) error {
	if err := c.check(p); err != nil {
		return err
	}
	actual, err := c.ReadPointer(p.Binding)
	if err != nil {
		return err
	}
	if actual != p {
		return ErrIntegrity
	}
	return nil
}
func (c *FileCustody) ReadRoot(p Pointer) (Root, error) {
	if err := c.requirePointer(p); err != nil {
		return Root{}, err
	}
	b, err := c.lease.read("secrets.json", MaxItem)
	if err != nil {
		return Root{}, err
	}
	return decodeRoot(p, b, c.secrets)
}
func (c *FileCustody) CreateRoot(p Pointer, r Root) error {
	if err := c.requirePointer(p); err != nil {
		return err
	}
	if f, err := c.lease.store.open("state.bin"); err == nil {
		f.Close()
		return ErrConflict
	} else if err != ErrUnavailable {
		return err
	}
	b, err := encodeRoot(p, r, c.secrets)
	if err != nil {
		return err
	}
	if err = c.lease.publish("secrets.json", b, true); err != nil {
		return err
	}
	got, err := c.ReadRoot(p)
	if err != nil {
		return err
	}
	if got != r {
		return ErrIntegrity
	}
	return nil
}

// PasswordSection is public metadata for the agent card. Its naming never
// depends on password bytes, profile aliases or caller-selected purpose strings.
type PasswordSection struct {
	Service string `json:"service"`
	Account string `json:"account"`
}

func AccountPasswordSection(mxid string) (PasswordSection, error) {
	if !fullMXID(mxid) {
		return PasswordSection{}, ErrInvalid
	}
	return PasswordSection{PasswordService, mxid}, nil
}

type passwordRecord struct {
	Version int             `json:"v"`
	Section PasswordSection `json:"section"`
	Value   string          `json:"value"`
}

func passwordName(s PasswordSection) string {
	return "password." + TupleHash(s.Service, s.Account) + ".json"
}
func (l *LockedStore) CreatePassword(mxid string, value []byte, secrets *Inventory) error {
	if secrets == nil {
		return ErrInvalid
	}
	secrets.Register(value)
	section, err := AccountPasswordSection(mxid)
	if err != nil {
		return err
	}
	if len(value) < 16 || len(value) > 256 || !text(string(value), 256) {
		return ErrInvalid
	}
	r := passwordRecord{1, section, string(value)}
	b, _ := json.Marshal(r)
	secrets.ObserveJSON(b)
	if err = l.publish(passwordName(section), b, true); err != nil {
		return err
	}
	got, err := l.ReadPassword(mxid, secrets)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, value) {
		return ErrIntegrity
	}
	return nil
}
func (l *LockedStore) ReadPassword(mxid string, secrets *Inventory) ([]byte, error) {
	if secrets == nil {
		return nil, ErrInvalid
	}
	section, err := AccountPasswordSection(mxid)
	if err != nil {
		return nil, err
	}
	b, err := l.read(passwordName(section), 2048)
	if err != nil {
		return nil, err
	}
	secrets.ObserveJSON(b)
	var r passwordRecord
	if err = strictJSON(b, &r, 2048); err != nil {
		return nil, err
	}
	if r.Version != 1 || r.Section != section || len(r.Value) < 16 || !text(r.Value, 256) {
		return nil, ErrCodec
	}
	secrets.Register([]byte(r.Value))
	return []byte(r.Value), nil
}
