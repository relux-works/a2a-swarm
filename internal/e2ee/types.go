// Package e2ee provides offline crypto/store primitives. It owns no Matrix sync,
// network client, room lifecycle, UIA, or trust-admission policy.
package e2ee

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Code is deliberately value-free: never wrap library, filesystem, or payload errors.
type Code string

func (c Code) Error() string { return string(c) }

const (
	ErrInvalid     Code = "e2ee_invalid"
	ErrUnavailable Code = "e2ee_unavailable"
	ErrUnsafe      Code = "e2ee_unsafe_store"
	ErrExists      Code = "e2ee_item_exists"
	ErrIO          Code = "e2ee_store_io"
	ErrCodec       Code = "e2ee_codec"
	ErrIntegrity   Code = "e2ee_integrity"
	ErrConflict    Code = "e2ee_conflict"
	ErrCapacity    Code = "e2ee_capacity"
	ErrLock        Code = "e2ee_lock"
	ErrReplay      Code = "e2ee_replay"
	ErrExpired     Code = "e2ee_replay_window_expired"
	ErrRefused     Code = "e2ee_refusal_history_blocked"
)
const MaxSnapshot = 64 << 20
const RecoveryReserve = 8 << 20
const MaxItem = 1024
const LegacyTokenService = "works.relux.a2a.relux.works"
const RootServicePrefix = "works.relux.a2a.e2ee.v1."
const PasswordService = "works.relux.a2a.account-password.v1"

// TupleHash is unambiguous even when fields contain delimiter characters.
func TupleHash(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		_ = binary.Write(h, binary.BigEndian, uint32(len(f)))
		_, _ = h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Namespace includes public credential-service references, never credential values.
type Namespace struct {
	Endpoint, Server, Connection, Backend, TokenService, PasswordService, Class, MXID string
}

func (n Namespace) Digest() (string, error) {
	for _, s := range []string{n.Endpoint, n.Server, n.Connection, n.Backend, n.TokenService, n.Class, n.MXID} {
		if !text(s, 2048) {
			return "", ErrInvalid
		}
	}
	if n.PasswordService != "" && !text(n.PasswordService, 200) {
		return "", ErrInvalid
	}
	if (n.Class != "agents" && n.Class != "humans") || !fullMXID(n.MXID) {
		return "", ErrInvalid
	}
	return TupleHash("1", n.Endpoint, n.Server, n.Connection, n.Backend, n.TokenService, n.PasswordService, n.Class, n.MXID), nil
}
func fullMXID(s string) bool {
	return text(s, 255) && strings.HasPrefix(s, "@") && strings.IndexByte(s[1:], ':') > 0 && !strings.HasSuffix(s, ":")
}
func text(s string, max int) bool {
	if len(s) == 0 || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r <= 32 || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func hexSize(s string, n int) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == n && hex.EncodeToString(b) == s
}

// Binding is authenticated in every snapshot and repeated in pointer/root readback.
type Binding struct {
	Binding    string `json:"binding"`
	Identity   string `json:"identity"`
	Device     string `json:"device_id"`
	Generation string `json:"generation"`
}

func (b Binding) valid() bool {
	return hexSize(b.Binding, 32) && fullMXID(b.Identity) && text(b.Device, 255) && hexSize(b.Generation, 16)
}

// PinRecord retains the owner's public MSK and USK, both fingerprinted, and the
// MSK signature over the standard USK object: user_id=Owner, usage=[user_signing],
// keys={UserSigningKeyID:UserSigningPublicKey}. This is host-provisioned evidence,
// not permission to provision pins or trust a key. U2a verifies that signature
// and the USK signature on the agent's own queried MSK; U2 validates host ownership
// and exact revision + 1. U1 validates shape only, never cryptographic authority.
type PinRecord struct {
	Revision                   uint64 `json:"revision"`
	Digest                     string `json:"digest"`
	Owner                      string `json:"owner"`
	MasterKeyID                string `json:"master_key_id"`
	MasterPublicKey            string `json:"master_public_key"`
	MasterSHA256               string `json:"master_sha256"`
	UserSigningKeyID           string `json:"user_signing_key_id"`
	UserSigningPublicKey       string `json:"user_signing_public_key"`
	UserSigningSHA256          string `json:"user_signing_sha256"`
	UserSigningMasterSignature string `json:"user_signing_master_signature"`
}

// Validate checks the public evidence shape and fingerprints. A structurally
// valid signature is not proof of authority: cryptographic verification is U2a.
func (p PinRecord) Validate() error {
	if p.Revision == 0 || !hexSize(p.Digest, 32) || !fullMXID(p.Owner) {
		return ErrCodec
	}
	for _, key := range []struct{ id, public, fingerprint string }{
		{p.MasterKeyID, p.MasterPublicKey, p.MasterSHA256},
		{p.UserSigningKeyID, p.UserSigningPublicKey, p.UserSigningSHA256},
	} {
		decoded, err := publicBase64(key.public, 32)
		if err != nil || key.id != "ed25519:"+key.public || !hexSize(key.fingerprint, 32) {
			return ErrCodec
		}
		hash := sha256.Sum256(decoded)
		if hex.EncodeToString(hash[:]) != key.fingerprint {
			return ErrIntegrity
		}
	}
	if _, err := publicBase64(p.UserSigningMasterSignature, 64); err != nil {
		return err
	}
	return nil
}

func publicBase64(value string, size int) ([]byte, error) {
	if len(value) != base64.RawStdEncoding.EncodedLen(size) {
		return nil, ErrCodec
	}
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != size || base64.RawStdEncoding.EncodeToString(decoded) != value {
		return nil, ErrCodec
	}
	return decoded, nil
}

// strictJSON rejects duplicate keys at every depth before Go's struct decoder can
// discard them. All durable records also reject unknown fields and trailing input.
func strictJSON(data []byte, dst any, max int) error {
	if len(data) == 0 || len(data) > max || !utf8.Valid(data) {
		return ErrCodec
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return ErrCodec
		}
		tok, e := d.Token()
		if e != nil {
			return ErrCodec
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					key, ok := k.(string)
					if e != nil || !ok || seen[key] {
						return ErrCodec
					}
					seen[key] = true
					if e = walk(depth + 1); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim('}') {
					return ErrCodec
				}
			case '[':
				for d.More() {
					if e := walk(depth + 1); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim(']') {
					return ErrCodec
				}
			default:
				return ErrCodec
			}
		}
		return nil
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrCodec
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode(dst); e != nil {
		return ErrCodec
	}
	return nil
}
